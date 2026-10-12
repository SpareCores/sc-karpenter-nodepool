#!/usr/bin/env bash
# Installs the pinned eksctl, kubectl and Helm into ${E2E_BIN:-$HOME/.local/bin}
# after checking their SHA-256 (linux/amd64, the GitHub-hosted runner). Run by
# the workflows; the AWS CLI, jq, yq and envsubst are the ones of the runner
# image.
#
# To bump a version: change it here together with its checksum, from the
# project's own release files (eksctl_checksums.txt of the release, the
# .sha256 file next to kubectl, the .sha256sum file next to the Helm archive).
set -euo pipefail

EKSCTL_VERSION="v0.231.0"
EKSCTL_SHA256="13cd1c4ae6ad3f74ea27aa1117c72e34ffa4ca8ff6d709261f7b1d7f411dc435" # eksctl_Linux_amd64.tar.gz
# Kubernetes 1.35 clusters (eksctl-*.yaml): kubectl within one minor version.
KUBECTL_VERSION="v1.35.9"
KUBECTL_SHA256="3cfeaf80be482b435b0aa214aff6e0b2c312ee23c0ff20810c75517b6004c6eb"
HELM_VERSION="v4.3.0"
HELM_SHA256="86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb" # helm-v4.3.0-linux-amd64.tar.gz

bin="${E2E_BIN:-${HOME}/.local/bin}"
mkdir -p "${bin}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

fetch() { # <url> <file> <sha256>
  curl -fsSL --retry 3 -o "${tmp}/$2" "$1"
  echo "$3  ${tmp}/$2" | sha256sum --check --strict
}

fetch "https://github.com/eksctl-io/eksctl/releases/download/${EKSCTL_VERSION}/eksctl_Linux_amd64.tar.gz" eksctl.tgz "${EKSCTL_SHA256}"
tar -xzf "${tmp}/eksctl.tgz" -C "${tmp}" eksctl
install -m 0755 "${tmp}/eksctl" "${bin}/eksctl"

fetch "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" kubectl "${KUBECTL_SHA256}"
install -m 0755 "${tmp}/kubectl" "${bin}/kubectl"

fetch "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" helm.tgz "${HELM_SHA256}"
tar -xzf "${tmp}/helm.tgz" -C "${tmp}" linux-amd64/helm
install -m 0755 "${tmp}/linux-amd64/helm" "${bin}/helm"

if [[ -n "${GITHUB_PATH:-}" ]]; then
  echo "${bin}" >>"${GITHUB_PATH}"
fi
export PATH="${bin}:${PATH}"
eksctl version
kubectl version --client
helm version --short
