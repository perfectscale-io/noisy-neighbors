#!/usr/bin/env bash
# Shared helpers. Sourced, never executed.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EKS_DIR="$(cd "${HERE}/.." && pwd)"
REPO_ROOT="$(cd "${EKS_DIR}/../.." && pwd)"

# shellcheck source=../env.sh
source "${EKS_DIR}/env.sh"

# --- output -------------------------------------------------------------
if [[ -t 1 ]]; then
  C_DIM=$'\033[2m'; C_RED=$'\033[31m'; C_GRN=$'\033[32m'
  C_YEL=$'\033[33m'; C_BLU=$'\033[34m'; C_OFF=$'\033[0m'
else
  C_DIM=; C_RED=; C_GRN=; C_YEL=; C_BLU=; C_OFF=
fi

step() { printf '\n%s==>%s %s\n' "${C_BLU}" "${C_OFF}" "$*"; }
info() { printf '    %s\n' "$*"; }
ok()   { printf '    %s✓%s %s\n' "${C_GRN}" "${C_OFF}" "$*"; }
warn() { printf '    %s!%s %s\n' "${C_YEL}" "${C_OFF}" "$*" >&2; }
die()  { printf '\n%serror:%s %s\n' "${C_RED}" "${C_OFF}" "$*" >&2; exit 1; }

# --- preflight ----------------------------------------------------------
need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is not installed. $2"
}

preflight() {
  step "Preflight"
  need aws      "brew install awscli"
  need eksctl   "brew install eksctl"
  need helm     "brew install helm"
  need kubectl  "brew install kubectl"
  need jq       "brew install jq"
  need envsubst "brew install gettext"

  AWS_ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text 2>/dev/null)" \
    || die "no valid AWS credentials. Run: aws login"
  export AWS_ACCOUNT_ID
  local arn; arn="$(aws sts get-caller-identity --query Arn --output text)"
  ok "account ${AWS_ACCOUNT_ID} in ${AWS_DEFAULT_REGION}"
  info "${C_DIM}${arn}${C_OFF}"

  # The public API endpoint is pinned to a single address. Detect it unless
  # the caller pinned one explicitly.
  if [[ -z "${ADMIN_CIDR}" ]]; then
    local ip
    ip="$(curl -fsS --max-time 10 https://checkip.amazonaws.com | tr -d '[:space:]')" \
      || die "could not detect your public IP; set ADMIN_CIDR yourself"
    [[ "${ip}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "bad IP from checkip: ${ip}"
    ADMIN_CIDR="${ip}/32"
  fi
  export ADMIN_CIDR
  ok "API endpoint will be restricted to ${ADMIN_CIDR}"

  # Fail before spending 20 minutes on a version EKS will not accept.
  if aws eks describe-cluster-versions >/dev/null 2>&1; then
    local avail
    avail="$(aws eks describe-cluster-versions \
      --query 'clusterVersions[?status!=`unsupported`].clusterVersion' --output text 2>/dev/null || true)"
    if [[ -n "${avail}" ]] && ! grep -qw -- "${K8S_VERSION}" <<<"${avail}"; then
      die "K8S_VERSION=${K8S_VERSION} is not offered by EKS. Available: ${avail}"
    fi
    ok "kubernetes ${K8S_VERSION} is available"
  fi
}

# --- rendering ----------------------------------------------------------
# Templates are envsubst'd against the exported environment. Any variable that
# is referenced but unset becomes an empty string, which silently produces a
# broken manifest -- so check first.
render() {
  local tmpl="$1"
  local missing=()
  local v
  for v in $(grep -oE '\$\{[A-Z_][A-Z0-9_]*\}' "${tmpl}" | tr -d '${}' | sort -u); do
    [[ -n "${!v:-}" ]] || missing+=("${v}")
  done
  (( ${#missing[@]} == 0 )) || die "unset variables for ${tmpl##*/}: ${missing[*]}"
  envsubst < "${tmpl}"
}

# --- state probes -------------------------------------------------------
cluster_exists() {
  aws eks describe-cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1
}

stack_status() {
  aws cloudformation describe-stacks --stack-name "$1" \
    --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo "MISSING"
}

kctx() { kubectl --context "${KCTX_ALIAS}" "$@"; }
