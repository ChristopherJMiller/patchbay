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
	@govulncheck -version | sed -n '/^Scanner:/p'
	@# sipp -v exits 99 by design; keep its version line only.
	@{ sipp -v 2>&1 || true; } | grep SIPp | sed -n 1p
	@{ tftp -V 2>&1 || true; } | sed -n 1p
	@ffmpeg -version | sed -n 1p
	@espeak-ng --version
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

# govulncheck judges the standard library by the go command it runs. The Go
# that matters is the one that builds the image (the Dockerfile's builder;
# Renovate bumps its patch releases), and that can be ahead of nixpkgs'
# go_1_26, so vulncheck runs that exact release: the go command fetches it
# from the Go module proxy, checked against sum.golang.org.
GO_IMAGE_VERSION := $(shell sed -n 's/^FROM .*golang:\([0-9][0-9.]*\)-.*/\1/p' Dockerfile)

.PHONY: vulncheck
vulncheck: ## Known vulnerabilities in code patchbay calls, with the image's Go (govulncheck; fetches vuln.go.dev).
	@test -n "$(GO_IMAGE_VERSION)" || { echo "vulncheck: no golang builder version in Dockerfile"; exit 1; }
	GOTOOLCHAIN=go$(GO_IMAGE_VERSION) govulncheck ./...

.PHONY: fmt
fmt: ## Format Go code (gofmt + goimports, via golangci-lint).
	golangci-lint fmt ./...

.PHONY: vet
vet: ## go vet (including the e2e-tagged code and the asterisk-check).
	go vet ./...
	go vet -tags e2e ./...
	go vet -tags asterisk ./internal/compile/ ./internal/agent/ami/

.PHONY: lint
lint: ## golangci-lint (incl. exhaustive, and the e2e suite) and helm lint.
	golangci-lint run ./...
	@# The chart has required values, so it is linted with each ci/ values file.
	for f in charts/patchbay/ci/*-values.yaml; do helm lint --strict charts/patchbay -f "$$f"; done

# The e2e build tag also changes what internal/ builds (cisco79x5's e2e test
# load, which the kind suite relies on), so the unit tests run under it too.
# -short skips envtest, which already ran untagged.
E2E_TAGGED_TESTS = go test $(GO_TEST_FLAGS) -short -tags e2e ./internal/...
# The one envtest that walks the e2e load (only e2e builds carry it):
# controller-devices' TestPhoneLifecycleFirmware.
E2E_TAGGED_ENVTEST = go test $(GO_TEST_FLAGS) -tags e2e -run '^TestPhoneLifecycleFirmware$$' ./internal/controller/devices/

.PHONY: check-versions
check-versions: envtest-assets ## Fail unless envtest's kube-apiserver minor is go.mod's k8s.io one, and the flake's Go minor the Dockerfile's.
	hack/check-versions.sh

.PHONY: test
test: envtest-assets check-versions ## Unit tests plus envtest (real kube-apiserver + etcd), then the unit tests with the e2e tag.
	go test $(GO_TEST_FLAGS) ./...
	$(E2E_TAGGED_TESTS)
	$(E2E_TAGGED_ENVTEST)

.PHONY: test-unit
test-unit: ## Unit tests only (skips envtest), untagged and with the e2e tag.
	go test $(GO_TEST_FLAGS) -short ./...
	$(E2E_TAGGED_TESTS)

.PHONY: envtest-assets
envtest-assets:
	@test -n "$${KUBEBUILDER_ASSETS:-}" && test -x "$$KUBEBUILDER_ASSETS/kube-apiserver" || \
		{ echo "KUBEBUILDER_ASSETS is not set: run inside 'nix develop'"; exit 1; }

##@ Build

.PHONY: build
build: ## Build bin/patchbay.
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/patchbay ./cmd/patchbay

# Go build tags for the image. Empty for every real image; `make e2e-images`
# passes e2e, which adds cisco79x5's e2e test load to the firmware manifest
# (docs/plans/release-a.md §2(b)): never ship an image built with it.
GO_TAGS ?=

.PHONY: sounds
sounds: ## Render the sound prompts into build/sounds (espeak-ng + ffmpeg; the image copies them).
	hack/gen-sounds.sh build/sounds

.PHONY: image
image: sounds ## Build the container image (IMG, default ghcr.io/christopherjmiller/patchbay:dev; GO_TAGS for build tags).
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		--build-arg GO_TAGS=$(GO_TAGS) \
		-t $(IMG) .

.PHONY: clean
clean: ## Remove build output.
	rm -rf bin/ build/ cover.out

##@ Real Asterisk (docker, no cluster)

# Containers the check starts carry this label, valued with the PID of the
# test process that owns them, so a run cut short can be cleaned up by the
# next one. Only orphans are removed (no such process, or a label from
# before owners were recorded): a run still going in another shell keeps its
# containers. A recycled PID keeps an orphan until that process exits.
ASTERISK_CHECK_LABEL := patchbay.chrismiller.xyz/asterisk-check

.PHONY: asterisk-check
asterisk-check: asterisk-check-orphans ## Boot every compile golden in the chart's pinned Asterisk (docker): modules, endpoints, dialplan, AMI, REGISTER and calls.
	docker pull --quiet $(ASTERISK_REPO):$(ASTERISK_TAG)@$(ASTERISK_DIGEST)
	@# Static (CGO_ENABLED=0): the test binary also runs inside the Asterisk
	@# container, as the SIP client (internal/compile/sipclient_asterisk_test.go).
	CGO_ENABLED=0 go test -tags asterisk -count=1 -timeout 10m -run '^TestRealAsterisk' -v ./internal/compile/ ./internal/agent/ami/

.PHONY: asterisk-check-orphans
asterisk-check-orphans: ## Remove asterisk-check containers whose test process is gone.
	@docker ps -a --filter label=$(ASTERISK_CHECK_LABEL) \
		--format '{{.ID}} {{.Label "$(ASTERISK_CHECK_LABEL)"}}' | \
	while read -r id owner; do \
		if [[ $$owner =~ ^[0-9]+$$ ]] && (( owner > 1 )) && [[ -d /proc/$$owner ]]; then continue; fi; \
		echo "asterisk-check: removing orphan container $$id (owner $${owner:-unknown})"; \
		docker rm --force "$$id" >/dev/null; \
	done

##@ End-to-end (kind + the chart + real Asterisk + fakephone; see docs/e2e.md)

E2E_CLUSTER    := patchbay-e2e
# The ONLY kubeconfig e2e uses. Never ~/.kube/config.
E2E_KUBECONFIG := $(CURDIR)/.e2e/kubeconfig
# kubectl and helm cache discovery here, not in the shared ~/.kube/cache.
E2E_KUBECACHE  := $(CURDIR)/.e2e/kube-cache
E2E_IMG        := ghcr.io/christopherjmiller/patchbay:e2e
E2E_TOOLS_IMG  := ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e
# The CLI the e2e suite runs, built with the e2e tag (the e2e firmware load).
E2E_CLI        := $(CURDIR)/.e2e/bin/patchbay
E2E_TIMEOUT    ?= 30m
# The Asterisk image is whatever the chart pins.
CHART_VALUES    := charts/patchbay/values.yaml
ASTERISK_REPO    = $(shell yq '.pbx.asterisk.image.repository' $(CHART_VALUES))
ASTERISK_TAG     = $(shell yq '.pbx.asterisk.image.tag' $(CHART_VALUES))
ASTERISK_DIGEST  = $(shell yq '.pbx.asterisk.image.digest' $(CHART_VALUES))

.PHONY: e2e
e2e: e2e-images ## Run the e2e suite. PATCHBAY_E2E_REUSE=1 reuses the cluster; PATCHBAY_E2E_KEEP=1 keeps it.
	KUBECONFIG=$(E2E_KUBECONFIG) KUBECACHEDIR=$(E2E_KUBECACHE) \
		go test -tags e2e -count=1 -timeout $(E2E_TIMEOUT) -v ./test/e2e/

.PHONY: e2e-up
e2e-up: e2e-images ## Create (or reuse) the e2e cluster and install the chart, run no tests, keep it.
	PATCHBAY_E2E_REUSE=1 PATCHBAY_E2E_KEEP=1 KUBECONFIG=$(E2E_KUBECONFIG) KUBECACHEDIR=$(E2E_KUBECACHE) \
		go test -tags e2e -count=1 -timeout $(E2E_TIMEOUT) -v -run '^$$' ./test/e2e/

.PHONY: e2e-down
e2e-down: ## Delete the e2e kind cluster.
	kind delete cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_KUBECONFIG)

.PHONY: e2e-images
e2e-images: ## Build the patchbay image and CLI (GO_TAGS=e2e) and the fakephone image; pull the chart's Asterisk image by digest.
	@# The e2e tag gives the agent the e2e firmware load (E2E45.0-0-1S).
	$(MAKE) image IMG=$(E2E_IMG) GO_TAGS=e2e
	@# And the CLI: `patchbay firmware load` must know the e2e load too.
	CGO_ENABLED=0 go build -trimpath -tags e2e -ldflags '$(LDFLAGS)' -o $(E2E_CLI) ./cmd/patchbay
	docker build -f test/e2e/fakephone/Dockerfile -t $(E2E_TOOLS_IMG) .
	@# Pulled by digest, then tagged so `kind load` can carry it (kind load
	@# drops registry digests; the e2e values reference the tag).
	docker pull $(ASTERISK_REPO):$(ASTERISK_TAG)@$(ASTERISK_DIGEST)
	docker tag $(ASTERISK_REPO)@$(ASTERISK_DIGEST) $(ASTERISK_REPO):$(ASTERISK_TAG)
