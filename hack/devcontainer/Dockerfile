# Dev container: every tool needed to generate, lint, test and package the
# project. All versions are pinned; bump them here and nowhere else.
# Run commands in it with hack/dev.sh.
FROM golang:1.26.9-bookworm

ARG TARGETARCH
ARG KUBEBUILDER_VERSION=v4.16.0
ARG CONTROLLER_GEN_VERSION=v0.22.0
ARG SETUP_ENVTEST_VERSION=v0.25.2
ARG ENVTEST_K8S_VERSION=1.36.0
ARG GOLANGCI_LINT_VERSION=v2.14.0
ARG KIND_VERSION=v0.33.0
ARG KUBECTL_VERSION=v1.37.1
ARG HELM_VERSION=v4.3.0
ARG ACTIONLINT_VERSION=1.7.12
ARG SHELLCHECK_VERSION=v0.11.0
ARG YQ_VERSION=v4.54.1
ARG JQ_VERSION=1.8.2
ARG DOCKER_VERSION=29.9.0
ARG BUILDX_VERSION=v0.38.0

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

RUN apt-get update \
 && apt-get install -y --no-install-recommends make git ca-certificates curl xz-utils python3 \
 && rm -rf /var/lib/apt/lists/*

# Prebuilt binaries. TARGETARCH is amd64 or arm64; tools that name the
# architecture differently get their own mapping.
RUN case "${TARGETARCH}" in \
      amd64) UARCH=x86_64 ;; \
      arm64) UARCH=aarch64 ;; \
      *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    cd /tmp; \
    curl -fsSL -o /usr/local/bin/kubebuilder "https://github.com/kubernetes-sigs/kubebuilder/releases/download/${KUBEBUILDER_VERSION}/kubebuilder_linux_${TARGETARCH}"; \
    curl -fsSL "https://github.com/golangci/golangci-lint/releases/download/${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION#v}-linux-${TARGETARCH}.tar.gz" | tar -xz --strip-components=1 -C /usr/local/bin "golangci-lint-${GOLANGCI_LINT_VERSION#v}-linux-${TARGETARCH}/golangci-lint"; \
    curl -fsSL -o /usr/local/bin/kind "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-linux-${TARGETARCH}"; \
    curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz" | tar -xz --strip-components=1 -C /usr/local/bin "linux-${TARGETARCH}/helm"; \
    curl -fsSL "https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}/actionlint_${ACTIONLINT_VERSION}_linux_${TARGETARCH}.tar.gz" | tar -xz -C /usr/local/bin actionlint; \
    curl -fsSL "https://github.com/koalaman/shellcheck/releases/download/${SHELLCHECK_VERSION}/shellcheck-${SHELLCHECK_VERSION}.linux.${UARCH}.tar.xz" | tar -xJ --strip-components=1 -C /usr/local/bin "shellcheck-${SHELLCHECK_VERSION}/shellcheck"; \
    curl -fsSL -o /usr/local/bin/yq "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${TARGETARCH}"; \
    curl -fsSL -o /usr/local/bin/jq "https://github.com/jqlang/jq/releases/download/jq-${JQ_VERSION}/jq-linux-${TARGETARCH}"; \
    curl -fsSL "https://download.docker.com/linux/static/stable/${UARCH}/docker-${DOCKER_VERSION}.tgz" | tar -xz --strip-components=1 -C /usr/local/bin docker/docker; \
    mkdir -p /usr/local/lib/docker/cli-plugins; \
    curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-buildx "https://github.com/docker/buildx/releases/download/${BUILDX_VERSION}/buildx-${BUILDX_VERSION}.linux-${TARGETARCH}"; \
    chmod +x /usr/local/bin/* /usr/local/lib/docker/cli-plugins/*

# Go-built tools and the envtest control-plane assets (kube-apiserver, etcd),
# baked in so `make test` needs no network. Module and build caches of this
# layer are discarded.
ENV GOFLAGS=-buildvcs=false
RUN GOBIN=/usr/local/bin go install "sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_GEN_VERSION}" \
 && GOBIN=/usr/local/bin go install "sigs.k8s.io/controller-runtime/tools/setup-envtest@${SETUP_ENVTEST_VERSION}" \
 && mkdir -p /usr/local/kubebuilder \
 && ln -s "$(setup-envtest use "${ENVTEST_K8S_VERSION}" --bin-dir /usr/local/envtest -p path)" /usr/local/kubebuilder/bin \
 && chmod -R a+rX /usr/local/envtest \
 && go clean -cache -modcache \
 && git config --system --add safe.directory /src

# Mount point of the cache volume (see hack/dev.sh): world-writable so the
# volume is usable by whatever uid the container runs as.
RUN mkdir -p /cache && chmod 1777 /cache

ENV KUBEBUILDER_ASSETS=/usr/local/kubebuilder/bin \
    ENVTEST_K8S_VERSION=${ENVTEST_K8S_VERSION}

WORKDIR /src
