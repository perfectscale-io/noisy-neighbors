# Single source of truth for the EKS lab. Sourced by every script and by eks.mk.
#
# Everything downstream is derived from these values, so the stack is
# reproducible: change a variable, tear down, spin up, and you get the same
# cluster with the new setting. Nothing is hardcoded anywhere else.

# --- identity -----------------------------------------------------------
export CLUSTER_NAME="${CLUSTER_NAME:-noisy-neighbors-lab}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export AWS_PARTITION="${AWS_PARTITION:-aws}"

# --- versions -----------------------------------------------------------
# Pinned on purpose. An unpinned lab is not a repeatable lab.
export K8S_VERSION="${K8S_VERSION:-1.36}"
export KARPENTER_VERSION="${KARPENTER_VERSION:-1.14.1}"
export KARPENTER_NAMESPACE="${KARPENTER_NAMESPACE:-kube-system}"
export METRICS_SERVER_VERSION="${METRICS_SERVER_VERSION:-3.13.0}"

# --- networking ---------------------------------------------------------
export VPC_CIDR="${VPC_CIDR:-10.42.0.0/16}"
# Single NAT gateway: ~$32/mo versus ~$96/mo for one per AZ. This is a lab.
# Use "HighlyAvailable" for anything you care about surviving an AZ outage.
export NAT_MODE="${NAT_MODE:-Single}"

# --- node sizing --------------------------------------------------------
# The system node group carries kube-system, Karpenter and metrics-server.
export SYSTEM_INSTANCE_TYPE="${SYSTEM_INSTANCE_TYPE:-m5.large}"
export SYSTEM_MIN_SIZE="${SYSTEM_MIN_SIZE:-2}"
export SYSTEM_MAX_SIZE="${SYSTEM_MAX_SIZE:-4}"
export SYSTEM_DESIRED_SIZE="${SYSTEM_DESIRED_SIZE:-2}"

# The tenant node is Karpenter-provisioned and deliberately single: co-tenancy
# is the entire subject of the tool, so every tenant has to land on one node.
# See karpenter/nodepools.yaml for how that is forced.
#
# The pool CPU limit must equal this instance's full vCPU count. That equality
# is what makes a second node impossible -- launching one would exceed the
# limit -- so the two values move together or the guarantee is lost.
#
# m5.xlarge (4 vCPU) rather than m5.large (2): the busybox tenants alone
# request 1380m, which left no room for a real workload alongside them. A
# second node would have been the wrong answer -- it would spread the tenants
# and dissolve the co-tenancy this lab exists to show.
export TENANT_INSTANCE_TYPE="${TENANT_INSTANCE_TYPE:-m5.xlarge}"
export TENANT_POOL_CPU_LIMIT="${TENANT_POOL_CPU_LIMIT:-4}"

# --- security -----------------------------------------------------------
export KMS_ALIAS="${KMS_ALIAS:-alias/eks-${CLUSTER_NAME}}"
export LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-7}"
# Public API endpoint is locked to this CIDR. Detected at spin-up time unless
# you set it yourself (e.g. to your office egress range).
export ADMIN_CIDR="${ADMIN_CIDR:-}"

# --- derived ------------------------------------------------------------
export KCTX_ALIAS="${KCTX_ALIAS:-$CLUSTER_NAME}"
export KARPENTER_STACK="Karpenter-${CLUSTER_NAME}"
