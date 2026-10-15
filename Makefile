# patchbay — run every target inside the flake's shell:
#   nix develop -c make <target>
# The shell supplies go, controller-gen, golangci-lint, helm, kind and the
# envtest binaries (KUBEBUILDER_ASSETS); nothing here downloads tools.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE    := github.com/ChristopherJMiller/patchbay
IMG       ?= ghcr.io/christopherjmiller/patchbay:dev
CRD_DIR   := charts/patchbay/templates/crds

# Version metadata, stamped in with -ldflags. DATE is the commit time, not
# "now", so two builds of one commit are identical.
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE      ?= $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)
LDFLAGS   := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

GO_TEST_FLAGS ?= -race -count=1

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: nix develop -c make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.PHONY: tools-versions
tools-versions: ## Print the version of every tool in the dev shell.
	@# `sed -n 1p` (not head) reads all input, so no tool dies of SIGPIPE.
	@go version
	@gopls version | sed -n 1p
	@golangci-lint version
	@echo "controller-gen $$(controller-gen --version)"
	@kind version
	@kubectl version --client | sed -n 1p
	@echo "helm $$(helm version --short)"
	@docker --version
	@jq --version
	@yq --version
	@echo "actionlint $$(actionlint -version | sed -n 1p)"
	@echo "gitleaks $$(gitleaks version)"
	@# sipp -v exits 99 by design; keep its version line only.
	@{ sipp -v 2>&1 || true; } | grep SIPp | sed -n 1p
	@{ tftp -V 2>&1 || true; } | sed -n 1p
	@ffmpeg -version | sed -n 1p
	@echo "envtest: $$KUBEBUILDER_ASSETS"
	@"$$KUBEBUILDER_ASSETS/kube-apiserver" --version
	@"$$KUBEBUILDER_ASSETS/etcd" --version | sed -n 1p

##@ Development

.PHONY: generate
generate: ## Regenerate deepcopy code and the CRDs (into the chart).
	controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	rm -f $(CRD_DIR)/*.yaml
	controller-gen crd paths=./api/... output:crd:artifacts:config=$(CRD_DIR)
	@# CRDs live in templates/ so `helm upgrade` applies schema changes; keep
	@# them on uninstall so the resources (and phones) are not deleted.
	for f in $(CRD_DIR)/*.yaml; do \
		yq -i '.metadata.annotations."helm.sh/resource-policy" = "keep"' "$$f"; \
	done

.PHONY: check-generated
check-generated: generate ## Fail if generated files are out of date.
	@git diff --exit-code -- api $(CRD_DIR) || \
		{ echo "generated files are stale: run 'make generate' and commit"; exit 1; }
	@test -z "$$(git status --porcelain -- api $(CRD_DIR))" || \
		{ git status --porcelain -- api $(CRD_DIR); echo "untracked generated files"; exit 1; }

.PHONY: check-hygiene
check-hygiene: ## Fail on tracked binaries, files over 512 KiB, firmware/archive/secret types, or secrets in history.
	hack/check-hygiene.sh

.PHONY: fmt
fmt: ## Format Go code (gofmt + goimports, via golangci-lint).
	golangci-lint fmt ./...

.PHONY: vet
vet: ## go vet (including the e2e-tagged suite).
	go vet ./...
	go vet -tags e2e ./test/e2e/...

.PHONY: lint
lint: ## golangci-lint (incl. exhaustive, and the e2e suite) and helm lint.
	golangci-lint run ./...
	@# The chart has required values, so it is linted with each ci/ values file.
	for f in charts/patchbay/ci/*-values.yaml; do helm lint --strict charts/patchbay -f "$$f"; done

.PHONY: test
test: envtest-assets ## Unit tests plus envtest (real kube-apiserver + etcd).
	go test $(GO_TEST_FLAGS) ./...

.PHONY: test-unit
test-unit: ## Unit tests only (skips envtest).
	go test $(GO_TEST_FLAGS) -short ./...

.PHONY: envtest-assets
envtest-assets:
	@test -n "$${KUBEBUILDER_ASSETS:-}" && test -x "$$KUBEBUILDER_ASSETS/kube-apiserver" || \
		{ echo "KUBEBUILDER_ASSETS is not set: run inside 'nix develop'"; exit 1; }

##@ Build

.PHONY: build
build: ## Build bin/patchbay.
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/patchbay ./cmd/patchbay

.PHONY: image
image: ## Build the container image (IMG, default ghcr.io/christopherjmiller/patchbay:dev).
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMG) .

.PHONY: clean
clean: ## Remove build output.
	rm -rf bin/ cover.out

##@ End-to-end (kind + the chart + real Asterisk + fakephone; see docs/e2e.md)

E2E_CLUSTER    := patchbay-e2e
# The ONLY kubeconfig e2e uses. Never ~/.kube/config.
E2E_KUBECONFIG := $(CURDIR)/.e2e/kubeconfig
E2E_IMG        := ghcr.io/christopherjmiller/patchbay:e2e
E2E_TOOLS_IMG  := ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e
E2E_TIMEOUT    ?= 30m
# The Asterisk image is whatever the chart pins.
CHART_VALUES    := charts/patchbay/values.yaml
ASTERISK_REPO    = $(shell yq '.pbx.asterisk.image.repository' $(CHART_VALUES))
ASTERISK_TAG     = $(shell yq '.pbx.asterisk.image.tag' $(CHART_VALUES))
ASTERISK_DIGEST  = $(shell yq '.pbx.asterisk.image.digest' $(CHART_VALUES))

.PHONY: e2e
e2e: e2e-images ## Run the e2e suite. PATCHBAY_E2E_REUSE=1 reuses the cluster; PATCHBAY_E2E_KEEP=1 keeps it.
	KUBECONFIG=$(E2E_KUBECONFIG) go test -tags e2e -count=1 -timeout $(E2E_TIMEOUT) -v ./test/e2e/

.PHONY: e2e-up
e2e-up: e2e-images ## Create (or reuse) the e2e cluster and install the chart, run no tests, keep it.
	PATCHBAY_E2E_REUSE=1 PATCHBAY_E2E_KEEP=1 KUBECONFIG=$(E2E_KUBECONFIG) \
		go test -tags e2e -count=1 -timeout $(E2E_TIMEOUT) -v -run '^$$' ./test/e2e/

.PHONY: e2e-down
e2e-down: ## Delete the e2e kind cluster.
	kind delete cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_KUBECONFIG)

.PHONY: e2e-images
e2e-images: ## Build the patchbay and fakephone images; pull the chart's Asterisk image by digest.
	$(MAKE) image IMG=$(E2E_IMG)
	docker build -f test/e2e/fakephone/Dockerfile -t $(E2E_TOOLS_IMG) .
	@# Pulled by digest, then tagged so `kind load` can carry it (kind load
	@# drops registry digests; the e2e values reference the tag).
	docker pull $(ASTERISK_REPO):$(ASTERISK_TAG)@$(ASTERISK_DIGEST)
	docker tag $(ASTERISK_REPO)@$(ASTERISK_DIGEST) $(ASTERISK_REPO):$(ASTERISK_TAG)
