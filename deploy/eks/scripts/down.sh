#!/usr/bin/env bash
#
# Tear the lab down, in the order that actually works.
#
# The ordering is the whole point of this script. Delete the cluster while
# Karpenter still owns EC2 instances and you get orphaned nodes billing
# quietly against a VPC that CloudFormation then refuses to delete because
# their ENIs are still attached. So: drain Karpenter's capacity first, remove
# the controller, then the cluster, then the IAM behind it.
#
#   ./scripts/down.sh
#
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

START="$(date +%s)"

step "Preflight"
need aws "brew install awscli"
need eksctl "brew install eksctl"
AWS_ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text 2>/dev/null)" \
  || die "no valid AWS credentials. Run: aws login"
export AWS_ACCOUNT_ID
ok "account ${AWS_ACCOUNT_ID}, region ${AWS_DEFAULT_REGION}, cluster ${CLUSTER_NAME}"

# ---------------------------------------------------------------------------
if cluster_exists; then
  aws eks update-kubeconfig --name "${CLUSTER_NAME}" --region "${AWS_DEFAULT_REGION}" \
    --alias "${KCTX_ALIAS}" >/dev/null 2>&1 || true

  step "Tenant workloads"
  helm uninstall noisy-tenants --namespace default --kube-context "${KCTX_ALIAS}" 2>/dev/null \
    && ok "removed" || info "not installed"

  # Namespaces are created by the chart but Helm will not always reap them
  # cleanly when a release is only partially applied.
  kctx delete namespace -l lab.noisy-neighbors/tenant=true --ignore-not-found --timeout 3m 2>/dev/null || true

  step "Draining Karpenter capacity"
  if kctx get nodepools >/dev/null 2>&1; then
    # Deleting the NodePool cordons, drains and terminates every node it owns.
    # Waiting here is what keeps the VPC deletable later.
    kctx delete nodepool --all --timeout 10m 2>/dev/null || true
    kctx delete ec2nodeclass --all --timeout 5m 2>/dev/null || true
    for _ in $(seq 1 36); do
      left="$(kctx get nodes -l karpenter.sh/nodepool --no-headers 2>/dev/null | wc -l | tr -d ' ')"
      [[ "${left}" == "0" ]] && break
      info "${left} Karpenter node(s) still terminating"
      sleep 10
    done
    left="$(kctx get nodes -l karpenter.sh/nodepool --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    [[ "${left}" == "0" ]] && ok "all Karpenter nodes gone" \
      || warn "${left} node(s) did not terminate; eksctl will force them"
  else
    info "no Karpenter CRDs present"
  fi

  step "Karpenter controller"
  helm uninstall karpenter --namespace "${KARPENTER_NAMESPACE}" --kube-context "${KCTX_ALIAS}" 2>/dev/null \
    && ok "removed" || info "not installed"

  step "metrics-server"
  helm uninstall metrics-server --namespace kube-system --kube-context "${KCTX_ALIAS}" 2>/dev/null \
    && ok "removed" || info "not installed"
else
  info "cluster ${CLUSTER_NAME} does not exist; cleaning up whatever is left"
fi

# ---------------------------------------------------------------------------
# Launch templates Karpenter generated. They are not owned by any stack, so
# nothing else will ever delete them.
step "Karpenter launch templates"
lts="$(aws ec2 describe-launch-templates \
  --filters "Name=tag:karpenter.k8s.aws/cluster,Values=${CLUSTER_NAME}" \
  --query 'LaunchTemplates[].LaunchTemplateName' --output text 2>/dev/null || true)"
if [[ -n "${lts}" ]]; then
  for lt in ${lts}; do
    aws ec2 delete-launch-template --launch-template-name "${lt}" >/dev/null && info "deleted ${lt}"
  done
  ok "cleaned up"
else
  ok "none left"
fi

# ---------------------------------------------------------------------------
step "EKS cluster"
if cluster_exists; then
  info "this takes 10-15 minutes"
  eksctl delete cluster --name "${CLUSTER_NAME}" --region "${AWS_DEFAULT_REGION}" --disable-nodegroup-eviction --wait
  ok "deleted"
else
  ok "already gone"
fi

# ---------------------------------------------------------------------------
step "Karpenter IAM stack"
if [[ "$(stack_status "${KARPENTER_STACK}")" != "MISSING" ]]; then
  aws cloudformation delete-stack --stack-name "${KARPENTER_STACK}"
  aws cloudformation wait stack-delete-complete --stack-name "${KARPENTER_STACK}" 2>/dev/null || true
  ok "deleted"
else
  ok "already gone"
fi

# ---------------------------------------------------------------------------
step "CloudWatch control plane logs"
lg="/aws/eks/${CLUSTER_NAME}/cluster"
if aws logs describe-log-groups --log-group-name-prefix "${lg}" \
     --query 'logGroups[0].logGroupName' --output text 2>/dev/null | grep -q "${lg}"; then
  aws logs delete-log-group --log-group-name "${lg}" && ok "deleted ${lg}"
else
  ok "none"
fi

# ---------------------------------------------------------------------------
# KMS has no immediate delete. Seven days is the minimum window; up.sh cancels
# the pending deletion and reuses the key if you spin the lab back up inside
# it, so the round trip costs nothing.
step "KMS key"
if key_arn="$(aws kms describe-key --key-id "${KMS_ALIAS}" --query 'KeyMetadata.Arn' --output text 2>/dev/null)"; then
  state="$(aws kms describe-key --key-id "${KMS_ALIAS}" --query 'KeyMetadata.KeyState' --output text)"
  if [[ "${state}" == "PendingDeletion" ]]; then
    ok "already pending deletion"
  else
    aws kms schedule-key-deletion --key-id "${key_arn}" --pending-window-in-days 7 >/dev/null
    ok "scheduled for deletion in 7 days (up.sh will revive it before then)"
  fi
else
  ok "none"
fi

step "kubeconfig"
kubectl config delete-context "${KCTX_ALIAS}" >/dev/null 2>&1 && ok "context removed" || ok "no context"

printf '\n%s==>%s Lab down in %ss.\n\n' "${C_GRN}" "${C_OFF}" "$(( $(date +%s) - START ))"
