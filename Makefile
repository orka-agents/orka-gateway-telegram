SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

APP ?= orka-gateway-telegram
GO ?= go
DOCKER ?= docker
KUBECTL ?= kubectl

DEFAULT_IMAGE := orka-gateway-telegram
IMAGE ?= $(DEFAULT_IMAGE)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf 'dev')
TAG ?= $(VERSION)
REVISION ?= $(shell git rev-parse --verify HEAD 2>/dev/null || printf 'unknown')
CREATED ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MAIN_PACKAGE ?= ./cmd/orka-gateway-telegram

BUILDER ?=
BUILDER_FLAG := $(if $(strip $(BUILDER)),--builder "$(BUILDER)",)
PLATFORMS ?= linux/amd64
KUBE_CONTEXT ?=
NAMESPACE ?=
ORKA_API_URL ?=
ADAPTER_URL ?=
STORAGE_CLASS ?=
ORKA_DIR ?=

export IMAGE TAG KUBE_CONTEXT NAMESPACE ORKA_API_URL ADAPTER_URL STORAGE_CLASS KUBECTL ORKA_DIR

.PHONY: help fmt vet test test-scripts test-orka-compatibility check build clean image validate-release-tag validate-distribution-image require-kube-context image-push inspect-image render-manifests deploy rollout-status live-validate

help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target> [VAR=value]\n\n"} /^[a-zA-Z0-9_.-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format Go sources.
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*')"; \
	if [[ -n "$$files" ]]; then gofmt -w $$files; fi

vet: ## Run go vet.
	$(GO) vet ./...

test: ## Run unit tests.
	$(GO) test ./...

test-scripts: ## Check build inputs and rendered deployment configuration.
	./scripts/test-build-config.sh
	./scripts/test-render-manifests.sh
	python3 ./scripts/test-validate-base-url.py

test-orka-compatibility: ## Run current Orka conformance locally; set ORKA_DIR to an Orka checkout.
	python3 ./scripts/test-orka-compatibility.py --orka-dir "$$ORKA_DIR"

check: vet test test-scripts ## Run Go and deployment configuration checks.

build: ## Build the adapter into bin/.
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -o "bin/$(APP)" "$(MAIN_PACKAGE)"

clean: ## Remove local build artifacts.
	rm -rf bin dist coverage .coverage

image: ## Build one local-platform image and load it into the local Docker engine.
	$(DOCKER) buildx build \
		--load \
		--tag "$(IMAGE):$(TAG)" \
		--build-arg "MAIN_PACKAGE=$(MAIN_PACKAGE)" \
		--build-arg "VERSION=$(VERSION)" \
		--build-arg "REVISION=$(REVISION)" \
		--build-arg "CREATED=$(CREATED)" \
		.

validate-release-tag: ## Validate release tag syntax and reject development builds.
	@./scripts/validate-image.sh tag

validate-distribution-image: ## Require an explicit registry image for publish and deploy operations.
	@./scripts/validate-image.sh repository

require-kube-context: ## Require an explicit Kubernetes context and watched namespace.
	@if [[ -z "$${KUBE_CONTEXT//[[:space:]]/}" ]]; then \
		echo "KUBE_CONTEXT must name the target Kubernetes context" >&2; \
		exit 2; \
	fi; \
	if [[ -z "$$NAMESPACE" || "$${#NAMESPACE}" -gt 63 || ! "$$NAMESPACE" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$$ ]]; then \
		echo "NAMESPACE must name the existing Orka watched namespace" >&2; \
		exit 2; \
	fi

image-push: validate-release-tag validate-distribution-image ## Build with buildx and push a release tag.
	$(DOCKER) buildx inspect $(if $(strip $(BUILDER)),"$(BUILDER)",) --bootstrap
	$(DOCKER) buildx build $(BUILDER_FLAG) \
		--platform "$(PLATFORMS)" \
		--push \
		--provenance=mode=max \
		--sbom=true \
		--tag "$(IMAGE):$(TAG)" \
		--build-arg "MAIN_PACKAGE=$(MAIN_PACKAGE)" \
		--build-arg "VERSION=$(VERSION)" \
		--build-arg "REVISION=$(REVISION)" \
		--build-arg "CREATED=$(CREATED)" \
		.

inspect-image: validate-release-tag validate-distribution-image ## Inspect the pushed multi-platform image.
	$(DOCKER) buildx imagetools inspect "$(IMAGE):$(TAG)"

render-manifests: ## Render the Kubernetes base without contacting a cluster.
	@./scripts/render-manifests.sh

deploy: validate-release-tag validate-distribution-image require-kube-context ## Render and apply the adapter in the existing Orka v2 namespace.
	@set -euo pipefail; \
	manifest="$$(mktemp)"; \
	trap 'rm -f "$$manifest"' EXIT; \
	./scripts/render-manifests.sh >"$$manifest"; \
	mode="$$("$$KUBECTL" --context "$$KUBE_CONTEXT" get namespace "$$NAMESPACE" -o 'jsonpath={.metadata.labels.orka\.ai/controller-mode}')"; \
	if [[ "$$mode" != harness-v2 ]]; then \
		echo "NAMESPACE must belong to an existing Orka harness-v2 controller" >&2; \
		exit 2; \
	fi; \
	"$$KUBECTL" --context "$$KUBE_CONTEXT" apply -f "$$manifest"
	$(MAKE) rollout-status

rollout-status: require-kube-context ## Wait for the adapter rollout on the explicit Kubernetes context.
	"$$KUBECTL" --context "$$KUBE_CONTEXT" --namespace "$$NAMESPACE" \
		rollout status deployment/$(APP) --timeout=5m

live-validate: require-kube-context ## Check the configured adapter and Orka deployment.
	./scripts/live-validate.sh
