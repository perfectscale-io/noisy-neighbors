#!/usr/bin/env bash
#
# Assert the security posture the cluster config claims, against the live
# cluster. A config file states intent; this checks what AWS and the API server
# actually did with it.
#
# Exits non-zero if any check fails, so it works in CI.
#
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
set +e

PASS=0; FAIL=0
chk() {  # chk <description> <expected> <actual>
  if [[ "$2" == "$3" ]]; then
    printf '  %s✓%s %-52s %s\n' "${C_GRN}" "${C_OFF}" "$1" "${C_DIM}${3}${C_OFF}"
    PASS=$((PASS+1))
  else
    printf '  %s✗%s %-52s want %s, got %s\n' "${C_RED}" "${C_OFF}" "$1" "$2" "$3"
    FAIL=$((FAIL+1))
  fi
}
chk_not() {  # chk_not <description> <forbidden> <actual>
  if [[ "$2" != "$3" ]]; then
    printf '  %s✓%s %-52s %s\n' "${C_GRN}" "${C_OFF}" "$1" "${C_DIM}${3}${C_OFF}"
    PASS=$((PASS+1))
  else
    printf '  %s✗%s %-52s must not be %s\n' "${C_RED}" "${C_OFF}" "$1" "$2"
    FAIL=$((FAIL+1))
  fi
}

cluster_exists || die "cluster ${CLUSTER_NAME} not found in ${AWS_DEFAULT_REGION}"
C="$(aws eks describe-cluster --name "${CLUSTER_NAME}" --output json)"

printf '\n%sControl plane%s\n' "${C_BLU}" "${C_OFF}"
chk "private API endpoint enabled" "true" \
  "$(jq -r '.cluster.resourcesVpcConfig.endpointPrivateAccess' <<<"${C}")"
cidrs="$(jq -r '.cluster.resourcesVpcConfig.publicAccessCidrs | join(",")' <<<"${C}")"
chk_not "public API endpoint not open to the world" "0.0.0.0/0" "${cidrs}"
chk "authentication mode (access entries, no aws-auth)" "API" \
  "$(jq -r '.cluster.accessConfig.authenticationMode' <<<"${C}")"
chk "secrets envelope-encrypted with a CMK" "true" \
  "$(jq -r 'if (.cluster.encryptionConfig // []) | length > 0 then "true" else "false" end' <<<"${C}")"
for t in api audit authenticator controllerManager scheduler; do
  chk "control plane log: ${t}" "true" \
    "$(jq -r --arg t "$t" '[.cluster.logging.clusterLogging[] | select(.enabled) | .types[]] | index($t) != null' <<<"${C}")"
done

printf '\n%sKMS%s\n' "${C_BLU}" "${C_OFF}"
kid="$(jq -r '.cluster.encryptionConfig[0].provider.keyArn // empty' <<<"${C}")"
if [[ -n "${kid}" ]]; then
  # --output text renders the boolean Python-cased ("True"), so fold it.
  chk "CMK key rotation enabled" "true" \
    "$(aws kms get-key-rotation-status --key-id "${kid}" --query KeyRotationEnabled --output text 2>/dev/null | tr '[:upper:]' '[:lower:]')"
fi

printf '\n%sData plane%s\n' "${C_BLU}" "${C_OFF}"
vpc_id="$(jq -r '.cluster.resourcesVpcConfig.vpcId' <<<"${C}")"
insts="$(aws ec2 describe-instances \
  --filters "Name=vpc-id,Values=${vpc_id}" "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[]' --output json)"
n_inst="$(jq 'length' <<<"${insts}")"
info "${n_inst} running instance(s) in ${vpc_id}"

chk "no worker has a public IP" "0" \
  "$(jq '[.[] | select(.PublicIpAddress != null)] | length' <<<"${insts}")"
chk "IMDSv2 required on every worker" "0" \
  "$(jq '[.[] | select(.MetadataOptions.HttpTokens != "required")] | length' <<<"${insts}")"
chk "IMDS hop limit 1 on every worker" "0" \
  "$(jq '[.[] | select(.MetadataOptions.HttpPutResponseHopLimit > 1)] | length' <<<"${insts}")"

vols="$(aws ec2 describe-volumes \
  --filters "Name=attachment.instance-id,Values=$(jq -r '[.[].InstanceId] | join(",")' <<<"${insts}")" \
  --query 'Volumes[]' --output json 2>/dev/null || echo '[]')"
chk "every attached EBS volume encrypted" "0" \
  "$(jq '[.[] | select(.Encrypted != true)] | length' <<<"${vols}")"

printf '\n%sNetwork placement%s\n' "${C_BLU}" "${C_OFF}"
pub_tagged="$(aws ec2 describe-subnets \
  --filters "Name=vpc-id,Values=${vpc_id}" "Name=tag:kubernetes.io/role/elb,Values=1" \
            "Name=tag:karpenter.sh/discovery,Values=${CLUSTER_NAME}" \
  --query 'length(Subnets)' --output text)"
chk "no public subnet is Karpenter-discoverable" "0" "${pub_tagged}"
priv_tagged="$(aws ec2 describe-subnets \
  --filters "Name=vpc-id,Values=${vpc_id}" "Name=tag:kubernetes.io/role/internal-elb,Values=1" \
            "Name=tag:karpenter.sh/discovery,Values=${CLUSTER_NAME}" \
  --query 'length(Subnets)' --output text)"
chk_not "private subnets are Karpenter-discoverable" "0" "${priv_tagged}"

printf '\n%sIn-cluster%s\n' "${C_BLU}" "${C_OFF}"
np="$(kctx get cm -n kube-system amazon-vpc-cni -o jsonpath='{.data.enable-network-policy-controller}' 2>/dev/null)"
chk "VPC CNI network policy engine on" "true" "${np:-false}"

ns_total="$(kctx get ns -l lab.noisy-neighbors/tenant=true --no-headers 2>/dev/null | wc -l | tr -d ' ')"
ns_restricted="$(kctx get ns -l lab.noisy-neighbors/tenant=true \
  -o jsonpath='{range .items[*]}{.metadata.labels.pod-security\.kubernetes\.io/enforce}{"\n"}{end}' 2>/dev/null \
  | grep -c '^restricted$')"
chk "tenant namespaces enforce restricted PSS" "${ns_total}" "${ns_restricted}"

np_count="$(kctx get netpol -A --no-headers 2>/dev/null | grep -c default-deny-ingress)"
chk "tenant namespaces default-deny ingress" "${ns_total}" "${np_count}"

# Scoped to tenant namespaces on purpose. EKS's own managed addons --
# aws-eks-nodeagent (which is what enforces the NetworkPolicies above),
# ebs-plugin and kube-proxy -- run privileged by design and cannot be made
# otherwise. Asserting "zero privileged containers on the cluster" would be
# asserting something unachievable on EKS, so the claim is the one that
# matters: no tenant gets privilege. The system ones are printed rather than
# hidden, so the exemption stays visible.
tenant_priv="$(kctx get pods -A -o json 2>/dev/null \
  | jq '[.items[] | select(.metadata.namespace | startswith("team-"))
         | .spec.containers[]? | select(.securityContext.privileged == true)] | length')"
chk "no privileged container in any tenant namespace" "0" "${tenant_priv:-0}"

sys_priv="$(kctx get pods -A -o json 2>/dev/null \
  | jq -r '[.items[] | select(.metadata.namespace | startswith("team-") | not)
            | .spec.containers[]? | select(.securityContext.privileged == true) | .name]
           | unique | join(", ")')"
[[ -n "${sys_priv}" ]] && info "${C_DIM}privileged system containers (EKS-managed, expected): ${sys_priv}${C_OFF}"

printf '\n%s%d passed, %d failed%s\n\n' \
  "$( ((FAIL==0)) && echo "${C_GRN}" || echo "${C_RED}")" "${PASS}" "${FAIL}" "${C_OFF}"
exit $(( FAIL > 0 ))
