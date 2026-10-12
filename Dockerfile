# Manager image. Built for the architecture of the build host by default;
# `make docker-buildx` builds multi-arch images (BUILDPLATFORM runs the Go
# toolchain natively, the binary is cross-compiled for TARGETARCH).
FROM --platform=$BUILDPLATFORM golang:1.26.9-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
