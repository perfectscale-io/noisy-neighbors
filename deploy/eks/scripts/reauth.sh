#!/usr/bin/env bash
#
# Re-pin the public API endpoint to the address you are calling from today.
#
# The cluster's public endpoint is locked to a single /32, detected at spin-up.
# That is the right posture and it has one consequence: the moment you change
# network, kubectl stops answering and hangs rather than saying why, because a
# CIDR-locked endpoint drops the packet instead of refusing it. Rebuilding the
# cluster to fix an address is twenty minutes and a new cluster; this is one
# API call.
#
# It narrows nothing and widens nothing. The endpoint stays public, private and
# pinned to exactly one address, which is what cluster.yaml.tmpl asks for. All
# that changes is which address.
#
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

need aws "brew install awscli"
need jq  "brew install jq"

aws sts get-caller-identity >/dev/null 2>&1 \
  || die "no valid AWS credentials. Run: aws login"

cluster_exists || die "cluster ${CLUSTER_NAME} not found in ${AWS_DEFAULT_REGION}"

step "Current endpoint"
C="$(aws eks describe-cluster --name "${CLUSTER_NAME}" --output json)"
current="$(jq -r '.cluster.resourcesVpcConfig.publicAccessCidrs | join(",")' <<<"${C}")"
private="$(jq -r '.cluster.resourcesVpcConfig.endpointPrivateAccess' <<<"${C}")"
info "allowed: ${current}"
info "private access: ${private}"

# ADMIN_CIDR is honoured if the caller pinned one, exactly as at spin-up.
if [[ -z "${ADMIN_CIDR}" ]]; then
  ip="$(curl -fsS --max-time 10 https://checkip.amazonaws.com | tr -d '[:space:]')" \
    || die "could not detect your public IP; set ADMIN_CIDR yourself"
  [[ "${ip}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "bad IP from checkip: ${ip}"
  ADMIN_CIDR="${ip}/32"
fi

# Refuse to be the thing that opens the cluster to the internet, even by
# typo. This script exists to move a pin, never to remove one.
[[ "${ADMIN_CIDR}" == "0.0.0.0/0" ]] && die "refusing to allow 0.0.0.0/0"
[[ "${ADMIN_CIDR}" =~ ^[0-9.]+/(3[0-2]|2[4-9])$ ]] \
  || die "ADMIN_CIDR=${ADMIN_CIDR} is wider than /24; pin it tighter"

if [[ "${current}" == "${ADMIN_CIDR}" ]]; then
  ok "already pinned to ${ADMIN_CIDR}; nothing to do"
  exit 0
fi

step "Re-pinning to ${ADMIN_CIDR}"
warn "was ${current}, will be ${ADMIN_CIDR}"
aws eks update-cluster-config \
  --name "${CLUSTER_NAME}" \
  --resources-vpc-config \
    "publicAccessCidrs=${ADMIN_CIDR},endpointPublicAccess=true,endpointPrivateAccess=true" \
  >/dev/null || die "update-cluster-config failed"

# EKS applies this asynchronously and the endpoint keeps dropping your packets
# until it lands, so waiting here is the difference between "it did not work"
# and "it had not finished".
step "Waiting for the update to apply"
for _ in $(seq 1 40); do
  state="$(aws eks describe-cluster --name "${CLUSTER_NAME}" \
    --query 'cluster.status' --output text 2>/dev/null || echo UNKNOWN)"
  cidrs="$(aws eks describe-cluster --name "${CLUSTER_NAME}" \
    --query 'cluster.resourcesVpcConfig.publicAccessCidrs|join(`,`,@)' \
    --output text 2>/dev/null || echo "")"
  if [[ "${state}" == "ACTIVE" && "${cidrs}" == "${ADMIN_CIDR}" ]]; then
    ok "endpoint pinned to ${ADMIN_CIDR}"
    break
  fi
  printf '    %s.%s' "${C_DIM}" "${C_OFF}"
  sleep 15
done
echo

step "Checking kubectl actually reaches it"
if kubectl --context "${KCTX_ALIAS}" get --raw /readyz --request-timeout=20s >/dev/null 2>&1; then
  ok "API server reachable as ${KCTX_ALIAS}"
else
  warn "still not reachable. The update can take a few minutes to propagate."
  info "if it persists: make eks-kubeconfig, then kubectl get nodes"
fi
