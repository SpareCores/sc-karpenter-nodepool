# Every target expects the tools of the dev container (hack/devcontainer), so
# run them through it: hack/dev.sh make test

IMG ?= ghcr.io/sparecores/sc-karpenter-nodepool:dev
PLATFORMS ?= linux/amd64,linux/arm64
CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# allowDangerousTypes: score, GiB, GHz and similar selector fields are float64
# (see api/v1alpha1/scnodepool_types.go).
# The paths are listed (not ./...): the lanes under test/ keep clones of other
# projects in their git-ignored caches, whose markers must not be picked up.
GEN_PATHS = {./api/...,./cmd/...,./internal/...}

.PHONY: manifests
manifests: ## Generate the CRD and RBAC manifests.
	controller-gen rbac:roleName=manager-role crd:allowDangerousTypes=true paths="$(GEN_PATHS)" output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: ## Generate DeepCopy implementations.
	controller-gen object:headerFile=hack/boilerplate.go.txt paths="$(GEN_PATHS)"

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

# kwok and crd are the build tags of the L3 and L2 lanes (test/kwok/e2e,
# test/crd): `make test` leaves them out, vet and lint check them.
.PHONY: vet
vet: ## Run go vet against code.
	go vet -tags kwok,crd ./...

.PHONY: lint
lint: vet ## Run go vet and golangci-lint.
	golangci-lint run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and apply fixes.
	golangci-lint run --fix

.PHONY: test
test: ## Run unit and envtest suites (needs KUBEBUILDER_ASSETS, set in the dev container).
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test ./... -coverprofile cover.out

.PHONY: chart-crds
chart-crds: manifests ## Copy the generated CRD into the Helm chart.
	hack/chart-sync.sh crds

.PHONY: chart-rbac
chart-rbac: manifests ## Copy the generated RBAC rules of the manager into the Helm chart.
	hack/chart-sync.sh rbac

.PHONY: docs-api
docs-api: manifests ## Generate docs/reference/api.md from the CRD.
	go run ./hack/apidocs config/crd/bases/karpenter.sparecores.com_scnodepools.yaml > docs/reference/api.md

##@ Test lanes

# These lanes create a kind cluster and need the dev container (docker socket,
# network): hack/dev.sh make test-crd

.PHONY: test-crd
test-crd: ## L2: every provider's NodePool CRD against the generated NodePools (kind, server-side dry-run).
	@test -n "$$SCKN_DEV_CONTAINER" || { echo "run via hack/dev.sh" >&2; exit 1; }
	test/crd/run.sh

# One of the core Karpenter versions of test/kwok/core-versions.txt.
KWOK_CORE_VERSION ?= v1.14

.PHONY: test-kwok
test-kwok: ## L3: real core Karpenter (KWOK provider) in kind, for KWOK_CORE_VERSION (default v1.14).
	@test -n "$$SCKN_DEV_CONTAINER" || { echo "run via hack/dev.sh" >&2; exit 1; }
	KWOK_CORE_VERSION=$(KWOK_CORE_VERSION) test/kwok/run.sh

.PHONY: kwok-up
kwok-up: ## kind + KWOK Karpenter + the controller + a Keeper replay server, left running (tutorial).
	@test -n "$$SCKN_DEV_CONTAINER" || { echo "run via hack/dev.sh" >&2; exit 1; }
	KWOK_SETUP_ONLY=1 KWOK_CORE_VERSION=$(KWOK_CORE_VERSION) test/kwok/run.sh

.PHONY: kwok-down
kwok-down: ## Delete the cluster and the images of kwok-up.
	@test -n "$$SCKN_DEV_CONTAINER" || { echo "run via hack/dev.sh" >&2; exit 1; }
	test/kwok/down.sh

.PHONY: test-kwok-matrix
test-kwok-matrix: ## L3 for every core version of test/kwok/core-versions.txt, one after the other.
	@test -n "$$SCKN_DEV_CONTAINER" || { echo "run via hack/dev.sh" >&2; exit 1; }
	@for v in $$(awk '!/^#/ && NF {print $$1}' test/kwok/core-versions.txt); do \
		KWOK_CORE_VERSION=$$v test/kwok/run.sh || exit 1; \
	done

.PHONY: test-chart
test-chart: ## Helm chart: lint, golden `helm template` output, CRD and RBAC in sync (UPDATE=1 rewrites the golden files).
	@test -n "$$SCKN_DEV_CONTAINER" || { echo "run via hack/dev.sh" >&2; exit 1; }
	test/chart/run.sh

.PHONY: test-docs
test-docs: ## Check that the relative links and anchors of the Markdown files resolve.
	python3 hack/check-docs.py

##@ Build

.PHONY: build
build: ## Build the manager binary.
	CGO_ENABLED=0 go build -o bin/manager ./cmd

.PHONY: docker-build
docker-build: ## Build the manager image for the host architecture.
	$(CONTAINER_TOOL) build -t $(IMG) .

# Multi-arch build; add --push (or --load for a single platform) to publish.
.PHONY: docker-buildx
docker-buildx: ## Build the manager image for $(PLATFORMS) with buildx and push it.
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) -t $(IMG) .

.PHONY: dev-image
dev-image: ## Build the dev container image (hack/devcontainer/Dockerfile).
	hack/dev.sh --build

##@ Deployment

.PHONY: install
install: manifests ## Install the CRD into the cluster of the current kubeconfig context.
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: ## Remove the CRD (and with it every SCNodePool) from the cluster.
	kubectl delete --ignore-not-found -f config/crd/bases

.PHONY: deploy
deploy: manifests ## Deploy the controller with kustomize.
	kubectl kustomize config/default | sed "s|controller:latest|$(IMG)|" | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Remove the controller deployed with `make deploy`.
	kubectl kustomize config/default | sed "s|controller:latest|$(IMG)|" | kubectl delete --ignore-not-found -f -
