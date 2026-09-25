#!/usr/bin/env bash
# Print a rendered template without touching AWS beyond a caller-identity read.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

AWS_ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text 2>/dev/null || echo '<ACCOUNT_ID>')"
export AWS_ACCOUNT_ID
export KMS_KEY_ARN="${KMS_KEY_ARN:-$(aws kms describe-key --key-id "${KMS_ALIAS}" --query KeyMetadata.Arn --output text 2>/dev/null || echo '<KMS_KEY_ARN>')}"
export ADMIN_CIDR="${ADMIN_CIDR:-$(curl -fsS --max-time 5 https://checkip.amazonaws.com 2>/dev/null | tr -d '[:space:]')/32}"

case "${1:-}" in
  cluster)   render "${EKS_DIR}/cluster.yaml.tmpl" ;;
  nodepools) render "${EKS_DIR}/karpenter/nodepools.yaml.tmpl" ;;
  *) die "usage: render.sh {cluster|nodepools}" ;;
esac
