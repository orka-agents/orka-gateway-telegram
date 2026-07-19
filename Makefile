SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

APP ?= orka-gateway-telegram
GO ?= go
DOCKER ?= docker
KUBECTL ?= kubectl

IMAGE ?= docker.io/sozercan/orka-gateway-telegram
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf 'dev')
TAG ?= $(VERSION)
IMAGE_REF ?= $(IMAGE):$(TAG)
REVISION ?= $(shell git rev-parse --verify HEAD 2>/dev/null || printf 'unknown')
CREATED ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MAIN_PACKAGE ?= ./cmd/orka-gateway-telegram

BUILDER ?= remote-vm
PLATFORMS ?= linux/amd64
KUBE_CONTEXT ?= sertac-aks
NAMESPACE ?= orka-gateway-telegram

.PHONY: help fmt vet test check build clean image validate-release-tag image-push inspect-image render-manifests deploy rollout-status live-validate

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
	@case "$(TAG)" in ""|dev|latest|*dirty*) echo "TAG must be explicit, immutable, and clean" >&2; exit 2;; esac

image-push: validate-release-tag ## Build with the named buildx builder and push an immutable image tag.
	$(DOCKER) buildx inspect "$(BUILDER)" --bootstrap >/dev/null
	$(DOCKER) buildx build \
		--builder "$(BUILDER)" \
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

inspect-image: ## Inspect the pushed multi-platform image.
	$(DOCKER) buildx imagetools inspect "$(IMAGE):$(TAG)"

render-manifests: ## Render the Kubernetes base without contacting a cluster.
	$(KUBECTL) kustomize deploy

deploy: validate-release-tag ## Atomically render and apply the requested immutable image.
	@case "$(IMAGE_REF)" in *:latest|*:dev|*dirty*) echo "IMAGE_REF must be immutable" >&2; exit 2;; esac
	@$(KUBECTL) kustomize deploy | \
		sed 's|image: docker.io/sozercan/orka-gateway-telegram:REPLACE_WITH_IMMUTABLE_TAG|image: $(IMAGE_REF)|' | \
		$(KUBECTL) --context "$(KUBE_CONTEXT)" apply -f -
	$(MAKE) rollout-status

rollout-status: ## Wait for the adapter rollout on the explicit Kubernetes context.
	$(KUBECTL) --context "$(KUBE_CONTEXT)" --namespace "$(NAMESPACE)" \
		rollout status deployment/$(APP) --timeout=5m

live-validate: ## Run the AKS live validation skeleton.
	./scripts/live-validate.sh
