# ---------------------------------------------------------------------------
# EKS lab
#
# The kind lab (`make lab-up`) proves the tool works. This one proves it works
# on a real, private, hardened cluster with Karpenter underneath -- which is
# where the questions the tool answers actually get asked.
#
# Everything is driven by deploy/eks/env.sh. Override on the command line:
#
#     make eks-up CLUSTER_NAME=my-lab AWS_DEFAULT_REGION=eu-west-1
# ---------------------------------------------------------------------------

EKS_DIR := deploy/eks
EKS_CTX := $(shell . $(EKS_DIR)/env.sh >/dev/null 2>&1; echo $${KCTX_ALIAS:-noisy-neighbors-lab})

.PHONY: eks-up
eks-up: ## Create the private EKS cluster and deploy everything onto it
	@$(EKS_DIR)/scripts/up.sh

.PHONY: eks-down
eks-down: ## Destroy the EKS cluster and every resource behind it
	@$(EKS_DIR)/scripts/down.sh

.PHONY: eks-reauth
eks-reauth: ## Re-pin the API endpoint to your current IP (run after changing network)
	@$(EKS_DIR)/scripts/reauth.sh

# --- PerfectScale before/after -----------------------------------------------
# Automation is configured with CRs (deploy/eks/perfectscale/), which let each
# namespace opt in to increases as well as decreases, per resource.

.PHONY: eks-ps-apply
eks-ps-apply: ## Apply the PerfectScale automation CRs (replaces UI automation)
	kubectl --context $(EKS_CTX) apply -f $(EKS_DIR)/perfectscale/

.PHONY: eks-ps-before
eks-ps-before: ## Automation off, tenants back to their original spec
	@$(EKS_DIR)/scripts/perfectscale.sh before

.PHONY: eks-ps-after
eks-ps-after: ## Automation on, and time how long PerfectScale takes to act
	@$(EKS_DIR)/scripts/perfectscale.sh after

.PHONY: eks-ps-status
eks-ps-status: ## Original spec vs running pod, per tenant
	@$(EKS_DIR)/scripts/perfectscale.sh status

.PHONY: eks-verify
eks-verify: ## Assert the cluster's security posture against the live cluster
	@$(EKS_DIR)/scripts/verify.sh

.PHONY: eks-config
eks-config: ## Print the rendered eksctl config without applying it
	@$(EKS_DIR)/scripts/render.sh cluster

.PHONY: eks-nodepools
eks-nodepools: ## Print the rendered Karpenter NodePools without applying them
	@$(EKS_DIR)/scripts/render.sh nodepools

.PHONY: eks-kubeconfig
eks-kubeconfig: ## Point kubectl at the lab cluster
	@. $(EKS_DIR)/env.sh && aws eks update-kubeconfig \
	  --name $$CLUSTER_NAME --region $$AWS_DEFAULT_REGION --alias $$KCTX_ALIAS

.PHONY: eks-status
eks-status: ## Show nodes, tenant placement and any terminations
	@echo "--- nodes ---"
	@kubectl --context $(EKS_CTX) get nodes \
	  -L noisy-neighbors.io/pool,node.kubernetes.io/instance-type,topology.kubernetes.io/zone
	@echo
	@echo "--- tenants ---"
	@kubectl --context $(EKS_CTX) get pods -A -l app -o wide
	@echo
	@echo "--- restarts and OOMKills ---"
	@kubectl --context $(EKS_CTX) get pods -A -o custom-columns=\
'NS:.metadata.namespace,POD:.metadata.name,RESTARTS:.status.containerStatuses[0].restartCount,LAST:.status.containerStatuses[0].lastState.terminated.reason' \
	  --no-headers 2>/dev/null | grep -v '<none>$$' || echo "(no terminations recorded yet)"

.PHONY: eks-scan
eks-scan: build ## Run noisy-neighbors against the EKS lab
	./bin/$(BINARY) --context $(EKS_CTX) --sample-window 30s --wide

.PHONY: eks-scan-md
eks-scan-md: build ## Produce the shareable markdown report from the EKS lab
	./bin/$(BINARY) --context $(EKS_CTX) --sample-window 30s -o markdown

.PHONY: eks-scan-json
eks-scan-json: build ## Machine-readable report from the EKS lab
	./bin/$(BINARY) --context $(EKS_CTX) --sample-window 30s -o json

.PHONY: eks-tenants
eks-tenants: ## Re-apply just the tenant chart (fast iteration on scenarios)
	@. $(EKS_DIR)/env.sh && helm upgrade --install noisy-tenants $(EKS_DIR)/charts/noisy-tenants \
	  --namespace default --kube-context $$KCTX_ALIAS --wait --timeout 5m
