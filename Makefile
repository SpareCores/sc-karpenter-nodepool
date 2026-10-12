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
.PHONY: manifests
manifests: ## Generate the CRD and RBAC manifests.
	controller-gen rbac:roleName=manager-role crd:allowDangerousTypes=true paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: ## Generate DeepCopy implementations.
	controller-gen object:headerFile=hack/boilerplate.go.txt paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

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
