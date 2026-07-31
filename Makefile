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
NAMESPACE ?= orka-gateway-telegram

.PHONY: help fmt vet test check build clean image validate-release-tag validate-distribution-image require-kube-context image-push inspect-image render-manifests deploy rollout-status live-validate

help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target> [VAR=value]\n\n"} /^[a-zA-Z0-9_.-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format Go sources.
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*')"; \
	if [[ -n "$$files" ]]; then gofmt -w $$files; fi

vet: ## Run go vet.
	$(GO) vet ./...

test: ## Run unit tests.
	$(GO) test ./...

check: vet test ## Run the non-mutating Go checks.

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

validate-release-tag: ## Reject mutable or dirty release tags.
	@tag="$(TAG)"; \
	if [[ -z "$$tag" || "$${#tag}" -gt 128 || ! "$$tag" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$$ ]]; then \
		echo "TAG must be a valid container image tag" >&2; \
		exit 2; \
	fi; \
	case "$$tag" in dev|latest|*dirty*) echo "TAG must be explicit, immutable, and clean" >&2; exit 2;; esac

validate-distribution-image: ## Require an explicit registry image for publish and deploy operations.
	@image="$(IMAGE)"; final_component="$${image##*/}"; \
	if [[ -z "$$image" || "$$image" != */* || "$$image" == *://* || "$$image" == *@* || \
		"$$image" =~ [[:space:]] || "$$final_component" == *:* ]]; then \
		echo "IMAGE must be a registry repository without a tag or digest" >&2; \
		exit 2; \
	fi

require-kube-context: ## Require an explicit Kubernetes context for cluster operations.
	@if [[ -z "$(KUBE_CONTEXT)" ]]; then \
		echo "KUBE_CONTEXT must name the target Kubernetes context" >&2; \
		exit 2; \
	fi

image-push: validate-release-tag validate-distribution-image ## Build with buildx and push an immutable image tag.
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
	$(KUBECTL) kustomize deploy

deploy: validate-release-tag validate-distribution-image require-kube-context ## Atomically render and apply the requested immutable image.
	@$(KUBECTL) kustomize deploy | \
		sed 's|image: orka-gateway-telegram:REPLACE_WITH_IMMUTABLE_TAG|image: $(IMAGE):$(TAG)|' | \
		$(KUBECTL) --context "$(KUBE_CONTEXT)" apply -f -
	$(MAKE) KUBE_CONTEXT="$(KUBE_CONTEXT)" rollout-status

rollout-status: require-kube-context ## Wait for the adapter rollout on the explicit Kubernetes context.
	$(KUBECTL) --context "$(KUBE_CONTEXT)" --namespace "$(NAMESPACE)" \
		rollout status deployment/$(APP) --timeout=5m

live-validate: require-kube-context ## Run the AKS live validation skeleton.
	KUBE_CONTEXT="$(KUBE_CONTEXT)" ./scripts/live-validate.sh
