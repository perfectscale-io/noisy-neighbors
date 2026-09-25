BINARY   := kubectl-noisy_neighbors
PKG      := ./cmd/$(BINARY)
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
CLUSTER  := noisy-lab
KCTX     := kind-$(CLUSTER)

.DEFAULT_GOAL := help

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the plugin binary into ./bin
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)
	@echo "built bin/$(BINARY) ($(VERSION))"

.PHONY: install
install: build ## Install the plugin onto your PATH (~/.local/bin)
	@mkdir -p $(HOME)/.local/bin
	cp bin/$(BINARY) $(HOME)/.local/bin/$(BINARY)
	@echo "installed to $(HOME)/.local/bin/$(BINARY)"
	@echo "ensure $(HOME)/.local/bin is on PATH, then: kubectl noisy-neighbors"

.PHONY: test
test: ## Run go vet and unit tests
	go vet ./...
	go test ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: scan
scan: ## Scan the history for secrets and the code for known vulnerabilities
	@command -v gitleaks >/dev/null || { echo "need gitleaks: brew install gitleaks"; exit 1; }
	gitleaks git --no-banner .
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: check
check: vet test ## Vet and test

.PHONY: clean
clean: ## Remove build output
	rm -rf bin

# ---------------------------------------------------------------------------
# Lab
#
# A reproducible multi-tenant cluster with deliberately broken tenants. The
# point is not that the scenarios are realistic in isolation -- they are
# exaggerated -- but that they are the real failure modes, on demand, so the
# tool's output can be checked against a known answer.
# ---------------------------------------------------------------------------

.PHONY: lab-up
lab-up: lab-cluster lab-metrics lab-tenants ## Create the lab cluster and deploy the tenants
	@echo
	@echo "Lab is up. Give the workloads ~60s to misbehave, then:"
	@echo "  make demo"

.PHONY: lab-cluster
lab-cluster: ## Create the kind cluster
	kind create cluster --config examples/lab/kind-cluster.yaml --wait 120s

.PHONY: lab-metrics
lab-metrics: ## Install metrics-server (patched for kind's self-signed kubelet certs)
	kubectl --context $(KCTX) apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
	kubectl --context $(KCTX) -n kube-system patch deployment metrics-server --type=json \
	  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
	kubectl --context $(KCTX) -n kube-system rollout status deployment/metrics-server --timeout=180s

.PHONY: lab-tenants
lab-tenants: ## Deploy the tenant workloads
	kubectl --context $(KCTX) apply -f examples/lab/tenants/

.PHONY: lab-status
lab-status: ## Show what the lab is doing
	@echo "--- pods ---"
	@kubectl --context $(KCTX) get pods -A -l 'app' -o wide
	@echo
	@echo "--- restarts and OOMKills ---"
	@kubectl --context $(KCTX) get pods -A -o custom-columns=\
'NS:.metadata.namespace,POD:.metadata.name,RESTARTS:.status.containerStatuses[0].restartCount,LAST:.status.containerStatuses[0].lastState.terminated.reason' \
	  --no-headers 2>/dev/null | grep -v '<none>$$' || echo "(no terminations recorded yet)"

.PHONY: demo
demo: build ## Run the tool against the lab
	./bin/$(BINARY) --context $(KCTX) --sample-window 30s --wide

.PHONY: demo-md
demo-md: build ## Produce the shareable markdown report from the lab
	./bin/$(BINARY) --context $(KCTX) --sample-window 30s -o markdown

.PHONY: demo-degraded
demo-degraded: build ## Show the report with tier 1 and tier 2 both unavailable
	./bin/$(BINARY) --context $(KCTX) --no-metrics --no-kubelet

.PHONY: lab-down
lab-down: ## Delete the lab cluster
	kind delete cluster --name $(CLUSTER)

# ---------------------------------------------------------------------------
# The wall
#
# A live picture of the same analysis the CLI prints, for showing this to a
# room rather than reading it in a terminal. Live and replay feed the same
# renderer, which is what makes a recording a usable stage fallback.
# ---------------------------------------------------------------------------

WALL     := noisy-wall
WALL_PKG := ./cmd/noisy-wall
GOLDEN   ?= golden-run.jsonl
# The five lab tenants. A real cluster carries namespaces the demo is not
# about, including the vendor's own exporter, and a wall that lists the tool
# you are pitching as a misbehaving tenant is worse than no wall.
TENANTS  ?= team-payments,team-search,team-billing,team-analytics,team-scratch

.PHONY: wall-build
wall-build: ## Build the wall server into ./bin
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o bin/$(WALL) $(WALL_PKG)

.PHONY: wall-synth
wall-synth: wall-build ## Serve a scripted run of the lab; no cluster needed
	./bin/$(WALL) -synth -allow-button

.PHONY: wall
wall: wall-build ## Serve the wall against the cluster in your kubeconfig
	./bin/$(WALL) -allow-button -tenants $(TENANTS) -sample-window 10s -interval 14s

.PHONY: wall-record
wall-record: wall-build ## Serve live and record every frame to $(GOLDEN)
	./bin/$(WALL) -allow-button -tenants $(TENANTS) -sample-window 10s -interval 14s -record $(GOLDEN)

.PHONY: wall-replay
wall-replay: wall-build ## Replay $(GOLDEN); the stage fallback
	./bin/$(WALL) -replay $(GOLDEN) -tenants $(TENANTS)

# EKS lab targets (eks-up, eks-down, eks-verify, eks-scan, ...)
include eks.mk
