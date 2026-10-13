# syntax=docker/dockerfile:1
# Manager image. Built for the architecture of the build host by default;
# `make docker-buildx` builds multi-arch images (BUILDPLATFORM runs the Go
# toolchain natively, the binary is cross-compiled for TARGETARCH).
FROM --platform=$BUILDPLATFORM golang:1.26.9-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH
# Set by the Makefile (git describe) and the release workflow (the git tag);
# nothing in the repository carries a version number.
ARG VERSION=dev
ARG COMMIT=

WORKDIR /workspace
COPY go.mod go.sum ./
# The modules stay in this layer (no cache mount), so the layer cache, also the
# GitHub Actions one, skips the download while go.mod is unchanged. The build
# cache mount only speeds up local rebuilds (the gha cache does not keep mounts).
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X github.com/SpareCores/sc-karpenter-nodepool/internal/version.Version=${VERSION} -X github.com/SpareCores/sc-karpenter-nodepool/internal/version.Commit=${COMMIT}" \
      -o manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
