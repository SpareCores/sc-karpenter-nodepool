#!/usr/bin/env bash
# Run a command inside the dev container (hack/devcontainer/Dockerfile).
#
#   hack/dev.sh make test
#   hack/dev.sh go test ./internal/selection/...
#
# The image is built on first use and rebuilt when the Dockerfile changes. The
# repository is mounted at /src; Go module and build caches live in a named
# volume. The Docker socket is mounted so the container can run kind and
# `docker build`.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dockerfile="${repo_root}/hack/devcontainer/Dockerfile"

image_repo="${SCKN_DEV_IMAGE_REPO:-sc-karpenter-nodepool-dev}"
cache_volume="${SCKN_DEV_CACHE_VOLUME:-sc-karpenter-nodepool-dev-cache}"
tag="$(sha256sum "${dockerfile}" | cut -c1-12)"
image="${image_repo}:${tag}"

if [[ "${1:-}" == "--build" ]]; then
  # Used by `make dev-image`: only make sure the image exists.
  shift
  want_build_only=1
else
  want_build_only=0
fi

if ! docker image inspect "${image}" >/dev/null 2>&1; then
  docker build -t "${image}" -f "${dockerfile}" "${repo_root}/hack/devcontainer"
fi

if [[ "${want_build_only}" == 1 ]]; then
  exit 0
fi

if [[ $# -eq 0 ]]; then
  set -- bash
fi

tty_flags=(-i)
if [[ -t 0 && -t 1 ]]; then
  tty_flags=(-it)
fi

sock_args=()
if [[ -S /var/run/docker.sock ]]; then
  sock_args=(-v /var/run/docker.sock:/var/run/docker.sock --group-add "$(stat -c %g /var/run/docker.sock)")
fi

# Settings of the local test lanes (test/crd/run.sh, test/kwok/run.sh) that
# pass through when set in the caller's environment.
env_args=()
for name in KWOK_CORE_VERSION KWOK_VENDORS KEEP_CLUSTER REUSE_CLUSTER KIND_CLUSTER_NAME; do
  if [[ -n "${!name:-}" ]]; then
    env_args+=(-e "${name}=${!name}")
  fi
done

exec docker run --rm "${tty_flags[@]}" \
  "${env_args[@]}" \
  -u "$(id -u):$(id -g)" \
  --network host \
  "${sock_args[@]}" \
  -v "${repo_root}:/src" \
  -v "${cache_volume}:/cache" \
  -w /src \
  -e HOME=/cache/home \
  -e GOPATH=/cache/go \
  -e GOCACHE=/cache/go-build \
  -e GOMODCACHE=/cache/go-mod \
  -e GOLANGCI_LINT_CACHE=/cache/golangci-lint \
  -e GOFLAGS=-buildvcs=false \
  -e GOTOOLCHAIN=local \
  -e SCKN_DEV_CONTAINER=1 \
  "${image}" "$@"
