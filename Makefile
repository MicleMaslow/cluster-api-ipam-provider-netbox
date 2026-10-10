# Copyright 2026 The cluster-api-ipam-provider-netbox Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -o pipefail -ec
.DEFAULT_GOAL := help

#
# Versions and dev stand.
#
CAPI_VERSION ?= v1.14.3
ENVTEST_K8S_VERSION ?= 1.37.0
KIND_CLUSTER ?= capi-netbox-dev
DEV_COMPOSE := docker compose -f hack/dev/compose.yaml
DEV_ENV_FILE ?= hack/dev/.env
DEV_ENV := set -a && source $(DEV_ENV_FILE) && set +a
export KUBECONFIG := $(CURDIR)/hack/dev/kubeconfig

#
# Image.
#
REGISTRY ?= ghcr.io/miclemaslow
IMAGE_NAME ?= cluster-api-ipam-provider-netbox
TAG ?= $(or $(RELEASE_TAG),dev)
IMG ?= $(REGISTRY)/$(IMAGE_NAME):$(TAG)
PLATFORMS ?= linux/amd64,linux/arm64

# Build info baked into internal/version; falls back when not in a git repo or without commits.
GIT_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
GIT_COMMIT ?= $(shell git rev-parse --verify -q HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DOCKER_BUILD_ARGS = --build-arg GIT_VERSION=$(GIT_VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE)

#
# Tools, installed into hack/tools/bin as <name>-<version>.
#
TOOLS_BIN_DIR := $(CURDIR)/hack/tools/bin
GO_INSTALL := ./hack/go-install.sh

CONTROLLER_GEN_VER := v0.22.0
CONTROLLER_GEN_BIN := controller-gen
CONTROLLER_GEN := $(TOOLS_BIN_DIR)/$(CONTROLLER_GEN_BIN)-$(CONTROLLER_GEN_VER)
CONTROLLER_GEN_PKG := sigs.k8s.io/controller-tools/cmd/controller-gen

SETUP_ENVTEST_VER := v0.25.0
SETUP_ENVTEST_BIN := setup-envtest
SETUP_ENVTEST := $(TOOLS_BIN_DIR)/$(SETUP_ENVTEST_BIN)-$(SETUP_ENVTEST_VER)
SETUP_ENVTEST_PKG := sigs.k8s.io/controller-runtime/tools/setup-envtest

GOLANGCI_LINT_VER := v2.14.0
GOLANGCI_LINT_BIN := golangci-lint
GOLANGCI_LINT := $(TOOLS_BIN_DIR)/$(GOLANGCI_LINT_BIN)-$(GOLANGCI_LINT_VER)
GOLANGCI_LINT_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint

GOLANGCI_LINT_KAL_BIN := golangci-lint-kube-api-linter
GOLANGCI_LINT_KAL := $(TOOLS_BIN_DIR)/$(GOLANGCI_LINT_KAL_BIN)

KUSTOMIZE_VER := v5.7.0
KUSTOMIZE_BIN := kustomize
KUSTOMIZE := $(TOOLS_BIN_DIR)/$(KUSTOMIZE_BIN)-$(KUSTOMIZE_VER)
KUSTOMIZE_PKG := sigs.k8s.io/kustomize/kustomize/v5

CLUSTERCTL := $(TOOLS_BIN_DIR)/clusterctl-$(CAPI_VERSION)

$(CONTROLLER_GEN):
	GOBIN=$(TOOLS_BIN_DIR) $(GO_INSTALL) $(CONTROLLER_GEN_PKG) $(CONTROLLER_GEN_BIN) $(CONTROLLER_GEN_VER)

$(SETUP_ENVTEST):
	GOBIN=$(TOOLS_BIN_DIR) $(GO_INSTALL) $(SETUP_ENVTEST_PKG) $(SETUP_ENVTEST_BIN) $(SETUP_ENVTEST_VER)

$(GOLANGCI_LINT):
	GOBIN=$(TOOLS_BIN_DIR) $(GO_INSTALL) $(GOLANGCI_LINT_PKG) $(GOLANGCI_LINT_BIN) $(GOLANGCI_LINT_VER)

$(GOLANGCI_LINT_KAL): $(GOLANGCI_LINT) hack/tools/.custom-gcl.yaml
	cd hack/tools && $(GOLANGCI_LINT) custom

$(KUSTOMIZE):
	CGO_ENABLED=0 GOBIN=$(TOOLS_BIN_DIR) $(GO_INSTALL) $(KUSTOMIZE_PKG) $(KUSTOMIZE_BIN) $(KUSTOMIZE_VER)

$(CLUSTERCTL):
	mkdir -p $(TOOLS_BIN_DIR)
	curl -sSLf -o $@ https://github.com/kubernetes-sigs/cluster-api/releases/download/$(CAPI_VERSION)/clusterctl-$$(go env GOOS)-$$(go env GOARCH)
	chmod +x $@

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[0-9A-Za-z_-]+:.*##/ { printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Generate

CRD_ROOT := config/crd/bases
RBAC_ROOT := config/rbac

.PHONY: generate
generate: generate-go-deepcopy generate-manifests ## Run all generate targets

.PHONY: generate-go-deepcopy
generate-go-deepcopy: $(CONTROLLER_GEN) ## Generate zz_generated.deepcopy.go
	find api -name zz_generated.deepcopy.go -delete
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate/boilerplate.generatego.txt paths=./api/...

.PHONY: generate-manifests
generate-manifests: $(CONTROLLER_GEN) ## Generate CRDs and RBAC
	rm -f $(CRD_ROOT)/*.yaml
	$(CONTROLLER_GEN) crd rbac:roleName=manager-role paths=./... output:crd:dir=$(CRD_ROOT) output:rbac:dir=$(RBAC_ROOT)

##@ Lint and verify

.PHONY: lint
lint: $(GOLANGCI_LINT) lint-api ## Lint the codebase
	$(GOLANGCI_LINT) run -v $(GOLANGCI_LINT_EXTRA_ARGS)

.PHONY: lint-api
lint-api: $(GOLANGCI_LINT_KAL) ## Lint the API types with kube-api-linter
	$(GOLANGCI_LINT_KAL) run -v --config .golangci-kal.yml $(GOLANGCI_LINT_EXTRA_ARGS) ./api/...

.PHONY: lint-fix
lint-fix: $(GOLANGCI_LINT) ## Lint the codebase and apply auto-fixes
	GOLANGCI_LINT_EXTRA_ARGS=--fix $(MAKE) lint

# $(call verify-unchanged,<paths>,<command>,<message>): fail if the command fails or changes <paths>; restore them either way.
define verify-unchanged
	@tmp=$$(mktemp -d) || exit 1; trap 'rm -rf $$tmp' EXIT; \
	restore() { for p in $(1); do rm -rf $$p; cp -R $$tmp/$$p $$p; done; }; \
	for p in $(1); do mkdir -p $$tmp/$$(dirname $$p) && cp -R $$p $$tmp/$$p || exit 1; done; \
	if ! $(2); then restore; echo "command failed, files restored: $(2)" >&2; exit 1; fi; \
	rc=0; for p in $(1); do diff -ru $$tmp/$$p $$p || rc=1; done; \
	if [ $$rc -ne 0 ]; then restore; echo "$(3)" >&2; exit 1; fi
endef

.PHONY: verify
verify: verify-gen verify-modules verify-boilerplate ## Run all verify-* targets

.PHONY: verify-gen
verify-gen: ## Verify generated files are up to date
	$(call verify-unchanged,api $(CRD_ROOT) $(RBAC_ROOT)/role.yaml,$(MAKE) --no-print-directory generate,generated files are out of date: run make generate)

.PHONY: verify-modules
verify-modules: ## Verify go.mod and go.sum are tidy
	$(call verify-unchanged,go.mod go.sum,go mod tidy,go.mod or go.sum is not tidy: run go mod tidy)

.PHONY: verify-boilerplate
verify-boilerplate: ## Verify license headers
	./hack/verify-boilerplate.sh

##@ Test

ENVTEST_ENV = KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" CAPI_CRDS="$$(go mod download sigs.k8s.io/cluster-api && go list -m -f '{{.Dir}}' sigs.k8s.io/cluster-api)/core/config/crd/bases"

.PHONY: test
test: $(SETUP_ENVTEST) ## Run unit tests, CRD validation and the control-plane VIP command test (no NetBox needed)
	hack/test-control-plane-vip.sh
	$(ENVTEST_ENV) go test ./...

.PHONY: test-integration
test-integration: $(SETUP_ENVTEST) ## Run all tests, controllers against the dev NetBox (needs make netbox-up)
	$(DEV_ENV) && $(ENVTEST_ENV) go test -count=1 ./...

##@ Build

.PHONY: docker-build
docker-build: ## Build the manager image for the host platform
	docker build $(DOCKER_BUILD_ARGS) -t $(IMG) .

.PHONY: docker-build-all
docker-build-all: ## Validate the PLATFORMS build (no --push/--load); needs a multi-platform buildx builder, see BUILDX_BUILDER
	docker buildx build --platform $(PLATFORMS) $(DOCKER_BUILD_ARGS) -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the manager image
	docker push $(IMG)

.PHONY: docker-push-all
docker-push-all: ## Build and push the PLATFORMS image; needs a multi-platform buildx builder
	docker buildx build --platform $(PLATFORMS) $(DOCKER_BUILD_ARGS) -t $(IMG) --push .

##@ Release

.PHONY: release
release: ## Build the manifests for RELEASE_TAG=vX.Y.Z[-{alpha,beta,rc}.N] into out/ and the local clusterctl repository out/repo
	@[[ "$$RELEASE_TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$$ ]] || { echo "RELEASE_TAG must be vX.Y.Z or vX.Y.Z-{alpha,beta,rc}.N, got '$$RELEASE_TAG'" >&2; exit 1; }; \
		awk -v M=$${BASH_REMATCH[1]} -v m=$${BASH_REMATCH[2]} '/major:/ {a = $$NF} /minor:/ && a == M && $$NF == m {f = 1} END {exit !f}' metadata.yaml || { echo "metadata.yaml has no release series for $$RELEASE_TAG" >&2; exit 1; }
	rm -rf out
	$(MAKE) --no-print-directory release-manifests IMG=$(REGISTRY)/$(IMAGE_NAME):$(RELEASE_TAG)
	mkdir -p out/repo/ipam-netbox/$(RELEASE_TAG)
	cp out/ipam-components.yaml out/metadata.yaml out/repo/ipam-netbox/$(RELEASE_TAG)/

# kustomize edit runs on a copy, so the committed kustomization keeps its default image.
.PHONY: release-manifests
release-manifests: generate $(KUSTOMIZE) ## Build out/ipam-components.yaml with the image IMG and out/metadata.yaml
	mkdir -p out
	tmp=$$(mktemp -d); trap 'rm -rf $$tmp' EXIT; cp -R config $$tmp/; \
		(cd $$tmp/config/default && $(KUSTOMIZE) edit set image controller=$(IMG)); \
		$(KUSTOMIZE) build $$tmp/config/default > out/ipam-components.yaml
	cp metadata.yaml out/

##@ Dev stand

.PHONY: dev-up
dev-up: netbox-up kind-up ## NetBox + kind management cluster with CAPI core

.PHONY: dev-down
dev-down: kind-down netbox-down ## Remove the whole dev stand

.PHONY: netbox-up
netbox-up: ## Start NetBox on :18000 and seed it (4.6.7, NETBOX_IMAGE for another version)
	$(DEV_COMPOSE) up -d --wait
	hack/dev/seed.sh

.PHONY: netbox-down
netbox-down: ## Stop NetBox and drop its data
	$(DEV_COMPOSE) down -v
	rm -f hack/dev/.env

.PHONY: kind-up
kind-up: $(CLUSTERCTL) ## Create the kind cluster and install CAPI core
	if kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER); then kind export kubeconfig --name $(KIND_CLUSTER) --kubeconfig $(KUBECONFIG); else kind create cluster --name $(KIND_CLUSTER) --kubeconfig $(KUBECONFIG); fi
	kubectl -n capi-kubeadm-control-plane-system get deployment capi-kubeadm-control-plane-controller-manager >/dev/null 2>&1 || $(CLUSTERCTL) init --config hack/dev/clusterctl.yaml --core cluster-api:$(CAPI_VERSION) --bootstrap kubeadm:$(CAPI_VERSION) --control-plane kubeadm:$(CAPI_VERSION) --wait-providers

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER) --kubeconfig $(KUBECONFIG)
	rm -f $(KUBECONFIG)

.PHONY: install
install: generate ## Install the CRDs into the kind cluster
	kubectl apply -k config/crd

.PHONY: run
run: install ## Run the provider locally against the kind cluster and the dev NetBox
	$(DEV_ENV) && go run . --netbox-owner=$$NETBOX_OWNER --diagnostics-address=0 --health-addr=0 --leader-elect=false

.PHONY: deploy-dev
deploy-dev: release-manifests docker-build $(CLUSTERCTL) ## Run the provider inside kind against the dev NetBox (stop make run first)
	kind load docker-image $(IMG) --name $(KIND_CLUSTER)
	docker network connect kind capi-netbox-dev-netbox-1 2>/dev/null || true
	$(DEV_ENV) && NETBOX_URL=http://capi-netbox-dev-netbox-1:8080 \
		$(CLUSTERCTL) generate yaml --config hack/dev/clusterctl.yaml --from out/ipam-components.yaml | kubectl apply -f -
	kubectl -n capi-ipam-netbox-system rollout restart deployment/capi-ipam-netbox-controller-manager
	kubectl -n capi-ipam-netbox-system rollout status deployment/capi-ipam-netbox-controller-manager --timeout=180s

.PHONY: smoke
smoke: ## Apply the samples and print the allocated address (provider must be running)
	hack/dev/smoke.sh

.PHONY: smoke-clean
smoke-clean: ## Delete the samples
	kubectl delete -f config/samples/sample.yaml --ignore-not-found --timeout=120s

.PHONY: vcsim-up
vcsim-up: $(CLUSTERCTL) $(KUSTOMIZE) ## Install CAPV and its vcsim controller (a simulated vCenter) into kind
	CLUSTERCTL=$(CLUSTERCTL) KUSTOMIZE=$(KUSTOMIZE) KIND_CLUSTER=$(KIND_CLUSTER) hack/dev/vcsim.sh up

.PHONY: vcsim-cluster
vcsim-cluster: $(CLUSTERCTL) ## Create the CAPV cluster NAME (default vc) on vcsim, machine addresses from NetBox
	CLUSTERCTL=$(CLUSTERCTL) hack/dev/vcsim.sh cluster $(or $(NAME),vc)

##@ Clean

.PHONY: clean
clean: ## Remove tool binaries and build output
	rm -rf $(TOOLS_BIN_DIR) out cover.out
