# Kubernetes Cost Optimization Engine
#
# `make help` lists the targets. The important ones:
#   make test         — the full test suite
#   make experiments  — regenerate every experimental result (~2 min)
#   make analysis     — regenerate every figure and table
#   make verify       — everything CI runs

SHELL := /bin/bash
.DEFAULT_GOAL := help

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
  -X github.com/anishc23/k8s-cost-optimizer/internal/version.Version=$(VERSION) \
  -X github.com/anishc23/k8s-cost-optimizer/internal/version.Commit=$(COMMIT) \
  -X github.com/anishc23/k8s-cost-optimizer/internal/version.BuildDate=$(BUILD_DATE)

BIN     := bin
CHART   := helm/k8s-cost-optimizer
IMAGE   ?= ghcr.io/anishc23/k8s-cost-optimizer
TAG     ?= $(VERSION)
KIND_CLUSTER ?= cost-optimizer

# The Python environment for the analysis layer. Created on demand so that a
# clone can run `make analysis` without a separate setup step.
VENV   := .venv
PYTHON := $(VENV)/bin/python
PIP    := $(VENV)/bin/pip

EXPERIMENTS := main windows sensitivity \
               ablation_no_oom_protection ablation_unified_strategy ablation_no_gates \
               oom_recovery oom_recovery_no_gate percentile_method validate_default

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: ## Build all binaries into bin/
	@mkdir -p $(BIN)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/optimizer ./cmd/optimizer
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/koctl ./cmd/koctl
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/experiment ./cmd/experiment
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/workload-gen ./cmd/workload-gen
	@echo "built $(VERSION) ($(COMMIT)) -> $(BIN)/"

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) dist

##@ Quality

.PHONY: fmt
fmt: ## Format Go source
	gofmt -w $(shell find . -name '*.go' -not -path './.venv/*')

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: test
test: ## Run the full test suite
	go test ./... -count=1

.PHONY: test-race
test-race: ## Run tests under the race detector
	go test -race ./... -count=1

.PHONY: cover
cover: ## Report test coverage by package
	go test ./... -coverprofile=coverage.out -count=1
	@go tool cover -func=coverage.out | tail -25

.PHONY: bench
bench: ## Run benchmarks
	go test -run '^$$' -bench=. -benchmem ./internal/... ./pkg/...

.PHONY: lint-chart
lint-chart: ## Lint and render the Helm chart
	helm lint $(CHART)
	helm template test-release $(CHART) > /dev/null
	@echo "chart renders"

.PHONY: verify
verify: fmt vet test lint-chart ## Everything CI runs
	@git diff --exit-code -- '*.go' \
	  || (echo "ERROR: gofmt produced changes; commit them" && exit 1)
	@echo "verify: OK"

##@ Research

$(VENV)/bin/activate:
	python3 -m venv $(VENV)
	$(PIP) install --quiet --upgrade pip
	$(PIP) install --quiet numpy pandas scipy matplotlib

.PHONY: venv
venv: $(VENV)/bin/activate ## Create the Python analysis environment

.PHONY: experiment-smoke
experiment-smoke: ## Fast pipeline check (~1s)
	go run ./cmd/experiment -config experiments/configs/smoke.yaml

.PHONY: experiments
experiments: ## Regenerate every experimental result (~2 min)
	@for e in $(EXPERIMENTS); do \
	  printf '%-32s' "$$e"; \
	  go run ./cmd/experiment -config experiments/configs/$$e.yaml 2>&1 \
	    | grep 'records in' || { echo "FAILED"; exit 1; }; \
	done
	@echo "results in experiments/results/"

.PHONY: experiment-sizes
experiment-sizes: ## Show the matrix size of each experiment without running it
	@for e in $(EXPERIMENTS); do \
	  printf '%-32s' "$$e"; \
	  go run ./cmd/experiment -config experiments/configs/$$e.yaml -dry-run \
	    | grep conditions; \
	done

.PHONY: analysis
analysis: venv ## Regenerate every figure and table
	$(PYTHON) analysis/scripts/figures.py

.PHONY: research
research: experiments analysis ## Full research pipeline: experiments then analysis

##@ Container

.PHONY: docker
docker: ## Build the optimizer image
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  -t $(IMAGE):$(TAG) .

.PHONY: docker-multiarch
docker-multiarch: ## Build for amd64 and arm64
	docker buildx build --platform linux/amd64,linux/arm64 \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  -t $(IMAGE):$(TAG) .

.PHONY: docker-workload-gen
docker-workload-gen: ## Build the synthetic workload generator image
	docker build -f cmd/workload-gen/Dockerfile -t k8s-cost-optimizer/workload-gen:$(TAG) .

##@ Local cluster

.PHONY: kind-up
kind-up: ## Create a kind cluster with Prometheus and demo workloads
	./scripts/kind-up.sh $(KIND_CLUSTER)

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: kind-load
kind-load: docker docker-workload-gen ## Load locally built images into kind
	kind load docker-image $(IMAGE):$(TAG) --name $(KIND_CLUSTER)
	kind load docker-image k8s-cost-optimizer/workload-gen:$(TAG) --name $(KIND_CLUSTER)

.PHONY: kind-deploy
kind-deploy: kind-load ## Install the chart into the kind cluster
	helm upgrade --install cost-optimizer $(CHART) \
	  --namespace cost-optimizer --create-namespace \
	  --set image.repository=$(IMAGE) --set image.tag=$(TAG) \
	  --set prometheus.address=http://prometheus.monitoring.svc.cluster.local:9090 \
	  --set policy.observationWindow=30m \
	  --set policy.minSamples=10 \
	  --set policy.minDuration=5m \
	  --set analysis.interval=1m \
	  --wait --timeout 5m

.PHONY: e2e
e2e: ## Run the end-to-end test against a running kind cluster
	go test ./test/e2e/ -tags=e2e -v -count=1 -timeout 20m

.PHONY: kind-e2e
kind-e2e: kind-up kind-deploy e2e ## Full end-to-end run from an empty machine
