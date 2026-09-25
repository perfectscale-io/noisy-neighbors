#!/usr/bin/env bash
#
# Spin the lab up. Idempotent: every phase checks for its own output first, so
# a re-run after a failure resumes rather than restarts, and a re-run after a
# success is a no-op.
#
#   ./scripts/up.sh
#
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

START="$(date +%s)"

# ---------------------------------------------------------------------------
preflight

# ---------------------------------------------------------------------------
# 1. Customer-managed KMS key for etcd envelope encryption.
#
# Teardown schedules this key for deletion rather than deleting it (KMS has no
# immediate delete), so a spin-up that follows a teardown finds the key in
# PendingDeletion and has to revive it. That is the normal path here, not an
# edge case.
# ---------------------------------------------------------------------------
step "KMS key for secrets encryption"
if key_arn="$(aws kms describe-key --key-id "${KMS_ALIAS}" --query 'KeyMetadata.Arn' --output text 2>/dev/null)"; then
  key_state="$(aws kms describe-key --key-id "${KMS_ALIAS}" --query 'KeyMetadata.KeyState' --output text)"
  if [[ "${key_state}" == "PendingDeletion" ]]; then
    info "key is pending deletion from a previous teardown; reviving"
    aws kms cancel-key-deletion --key-id "${key_arn}" >/dev/null
    aws kms enable-key --key-id "${key_arn}" >/dev/null
  fi
  ok "reusing ${KMS_ALIAS}"
else
  info "creating ${KMS_ALIAS}"
  key_id="$(aws kms create-key \
    --description "EKS secrets envelope encryption for ${CLUSTER_NAME}" \
    --tags "TagKey=project,TagValue=noisy-neighbors" "TagKey=cluster,TagValue=${CLUSTER_NAME}" \
    --query 'KeyMetadata.KeyId' --output text)"
  aws kms create-alias --alias-name "${KMS_ALIAS}" --target-key-id "${key_id}" >/dev/null
  aws kms enable-key-rotation --key-id "${key_id}" >/dev/null
  key_arn="$(aws kms describe-key --key-id "${key_id}" --query 'KeyMetadata.Arn' --output text)"
  ok "created ${KMS_ALIAS} (annual rotation on)"
fi
export KMS_KEY_ARN="${key_arn}"

# ---------------------------------------------------------------------------
# 2. Karpenter's IAM + interruption queue, from the upstream CloudFormation.
#
# This creates KarpenterNodeRole-<cluster>, the six scoped controller policies,
# an SQS queue for spot interruption and capacity-rebalance notices, and the
# EventBridge rules that feed it. The cluster config below references the role
# and the policies by name, so this must land first.
# ---------------------------------------------------------------------------
step "Karpenter IAM (CloudFormation stack ${KARPENTER_STACK})"
cfn="${TMP}/karpenter-cfn.yaml"
curl -fsSL "https://raw.githubusercontent.com/aws/karpenter-provider-aws/v${KARPENTER_VERSION}/website/content/en/preview/getting-started/getting-started-with-karpenter/cloudformation.yaml" -o "${cfn}" \
  || die "could not fetch Karpenter ${KARPENTER_VERSION} CloudFormation template"
aws cloudformation deploy \
  --stack-name "${KARPENTER_STACK}" \
  --template-file "${cfn}" \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides "ClusterName=${CLUSTER_NAME}" \
  --tags project=noisy-neighbors cluster="${CLUSTER_NAME}" \
  --no-fail-on-empty-changeset
ok "$(stack_status "${KARPENTER_STACK}")"

# EC2 spot needs its service-linked role to exist in the account once.
aws iam create-service-linked-role --aws-service-name spot.amazonaws.com >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# 3. The cluster.
# ---------------------------------------------------------------------------
step "EKS cluster ${CLUSTER_NAME}"
render "${EKS_DIR}/cluster.yaml.tmpl" > "${TMP}/cluster.yaml"
if cluster_exists; then
  ok "already exists; skipping create"
else
  info "this takes 15-20 minutes"
  eksctl create cluster -f "${TMP}/cluster.yaml"
fi

aws eks update-kubeconfig \
  --name "${CLUSTER_NAME}" \
  --region "${AWS_DEFAULT_REGION}" \
  --alias "${KCTX_ALIAS}" >/dev/null
ok "kubeconfig context: ${KCTX_ALIAS}"

# ---------------------------------------------------------------------------
# 4. Narrow Karpenter's discovery tags.
#
# eksctl stamps metadata.tags onto every VPC resource it creates, public
# subnets included. Left alone, Karpenter would consider a public subnet a
# valid placement and hand a worker a public IP. Same for security groups:
# several match, and Karpenter attaches all of them. Both are narrowed here to
# exactly what should be eligible, so the tag state on the account matches what
# the EC2NodeClass claims to select.
# ---------------------------------------------------------------------------
step "Restricting Karpenter node placement to private subnets"
vpc_id="$(aws eks describe-cluster --name "${CLUSTER_NAME}" --query 'cluster.resourcesVpcConfig.vpcId' --output text)"
cluster_sg="$(aws eks describe-cluster --name "${CLUSTER_NAME}" --query 'cluster.resourcesVpcConfig.clusterSecurityGroupId' --output text)"

public_subnets="$(aws ec2 describe-subnets \
  --filters "Name=vpc-id,Values=${vpc_id}" "Name=tag:kubernetes.io/role/elb,Values=1" \
  --query 'Subnets[].SubnetId' --output text)"
private_subnets="$(aws ec2 describe-subnets \
  --filters "Name=vpc-id,Values=${vpc_id}" "Name=tag:kubernetes.io/role/internal-elb,Values=1" \
  --query 'Subnets[].SubnetId' --output text)"

[[ -n "${private_subnets}" ]] || die "found no private subnets in ${vpc_id}"

if [[ -n "${public_subnets}" ]]; then
  # shellcheck disable=SC2086
  aws ec2 delete-tags --resources ${public_subnets} --tags "Key=karpenter.sh/discovery" >/dev/null
  info "discovery tag removed from public subnets: ${public_subnets}"
fi
# shellcheck disable=SC2086
aws ec2 create-tags --resources ${private_subnets} \
  --tags "Key=karpenter.sh/discovery,Value=${CLUSTER_NAME}" >/dev/null
ok "eligible subnets: ${private_subnets}"

tagged_sgs="$(aws ec2 describe-security-groups \
  --filters "Name=vpc-id,Values=${vpc_id}" "Name=tag:karpenter.sh/discovery,Values=${CLUSTER_NAME}" \
  --query 'SecurityGroups[].GroupId' --output text)"
for sg in ${tagged_sgs}; do
  [[ "${sg}" == "${cluster_sg}" ]] && continue
  aws ec2 delete-tags --resources "${sg}" --tags "Key=karpenter.sh/discovery" >/dev/null
  info "discovery tag removed from ${sg}"
done
aws ec2 create-tags --resources "${cluster_sg}" \
  --tags "Key=karpenter.sh/discovery,Value=${CLUSTER_NAME}" >/dev/null
ok "eligible security group: ${cluster_sg} (cluster SG)"

# ---------------------------------------------------------------------------
# 5. Karpenter.
# ---------------------------------------------------------------------------
step "Karpenter ${KARPENTER_VERSION} (helm)"
helm registry logout public.ecr.aws >/dev/null 2>&1 || true
helm upgrade --install karpenter oci://public.ecr.aws/karpenter/karpenter \
  --version "${KARPENTER_VERSION}" \
  --namespace "${KARPENTER_NAMESPACE}" \
  --kube-context "${KCTX_ALIAS}" \
  -f "${EKS_DIR}/values/karpenter.yaml" \
  --set "settings.clusterName=${CLUSTER_NAME}" \
  --set "settings.interruptionQueue=${CLUSTER_NAME}" \
  --set "settings.enableZonalShift=true" \
  --wait --timeout 10m
ok "karpenter ready"

step "NodePools"
render "${EKS_DIR}/karpenter/nodepools.yaml.tmpl" > "${TMP}/nodepools.yaml"
kctx apply -f "${TMP}/nodepools.yaml"
ok "general + tenants pools applied"

# ---------------------------------------------------------------------------
# 6. metrics-server -- tier 1.
# ---------------------------------------------------------------------------
step "metrics-server ${METRICS_SERVER_VERSION} (helm)"
helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ >/dev/null 2>&1 || true
helm repo update metrics-server >/dev/null
helm upgrade --install metrics-server metrics-server/metrics-server \
  --version "${METRICS_SERVER_VERSION}" \
  --namespace kube-system \
  --kube-context "${KCTX_ALIAS}" \
  -f "${EKS_DIR}/values/metrics-server.yaml" \
  --wait --timeout 5m

# Verify tier 1 actually works rather than assuming it. EKS kubelets serve
# self-signed certificates unless serverTLSBootstrap is on and the CSRs get
# approved, and the usual answer is --kubelet-insecure-tls. That is a real TLS
# bypass, so it is applied only if it is genuinely needed, and it is announced.
metrics_ok=false
for _ in $(seq 1 12); do
  if kctx top nodes >/dev/null 2>&1; then metrics_ok=true; break; fi
  sleep 10
done

if [[ "${metrics_ok}" == false ]]; then
  warn "metrics.k8s.io is not serving; kubelet certificate verification is the usual cause"
  warn "applying --kubelet-insecure-tls: metrics-server will no longer verify kubelet"
  warn "serving certs. Acceptable in this lab; for production enable"
  warn "serverTLSBootstrap on the nodegroups and approve the kubelet CSRs instead."
  helm upgrade metrics-server metrics-server/metrics-server \
    --version "${METRICS_SERVER_VERSION}" \
    --namespace kube-system \
    --kube-context "${KCTX_ALIAS}" \
    -f "${EKS_DIR}/values/metrics-server.yaml" \
    --set-json 'args=["--kubelet-insecure-tls"]' \
    --wait --timeout 5m
  for _ in $(seq 1 12); do
    if kctx top nodes >/dev/null 2>&1; then metrics_ok=true; break; fi
    sleep 10
  done
fi

if [[ "${metrics_ok}" == true ]]; then ok "metrics.k8s.io serving"
else warn "metrics.k8s.io still not serving -- the scan will degrade to tier 0/2 and say so"; fi

# ---------------------------------------------------------------------------
# 7. Tenants.
# ---------------------------------------------------------------------------
step "Tenant workloads (helm)"
helm upgrade --install noisy-tenants "${EKS_DIR}/charts/noisy-tenants" \
  --namespace default \
  --kube-context "${KCTX_ALIAS}" \
  --wait --timeout 10m \
  || warn "helm --wait timed out; the OOM victim crashlooping by design can cause this"

step "Waiting for Karpenter to place the tenant node"
for _ in $(seq 1 30); do
  n="$(kctx get nodes -l noisy-neighbors.io/pool=tenants --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  [[ "${n}" -ge 1 ]] && break
  sleep 10
done
kctx get nodes -L noisy-neighbors.io/pool,node.kubernetes.io/instance-type,topology.kubernetes.io/zone

step "Tenant placement"
kctx get pods -A -l app -o wide --no-headers | awk '{printf "    %-18s %-34s %-9s %s\n", $1, $2, $4, $8}'

printf '\n%s==>%s Lab up in %ss.\n' "${C_GRN}" "${C_OFF}" "$(( $(date +%s) - START ))"
cat <<EOF

    Give the tenants ~90s to misbehave (the OOM victim needs a crash cycle),
    then run the scan:

        make eks-scan

EOF
