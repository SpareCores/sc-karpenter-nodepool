#!/usr/bin/env bash
# Installs the controller from the Helm chart of this checkout, with the image
# the workflow built (E2E_IMAGE_TAG, public on ghcr, no pull secret). Needs
# KUBECONFIG and E2E_TARGET.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
target_init
require_env E2E_IMAGE_TAG

log "installing the chart with image ${E2E_IMAGE_TAG}"
helm upgrade --install sc-karpenter-nodepool "${root}/charts/sc-karpenter-nodepool" \
  --namespace "${E2E_NAMESPACE}" --create-namespace \
  --values "${here}/manifests/values-${E2E_TARGET}.yaml" \
  --set "image.tag=${E2E_IMAGE_TAG}" \
  --wait --timeout 15m
kubectl -n "${E2E_NAMESPACE}" get pods -o wide
kubectl get crd scnodepools.karpenter.sparecores.com
