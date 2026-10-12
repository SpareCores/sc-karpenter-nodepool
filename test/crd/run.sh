#!/usr/bin/env bash
# L2: every provider's NodePool CRD against the golden NodePools and the
# negative cases (docs/plans/v1/wip/00-overview.md §8.3), with server-side
# dry-runs in one kind cluster. Run it in the dev container:
# hack/dev.sh make test-crd
#
#   KEEP_CLUSTER=1     keep the kind cluster
#   KIND_CLUSTER_NAME  cluster name (default sckn-crd)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
cd "${root}"
# shellcheck source=test/lib.sh
source "${root}/test/lib.sh"

cluster="${KIND_CLUSTER_NAME:-sckn-crd}"
cache="${here}/.cache"
out="${here}/.out"
mkdir -p "${cache}" "${out}"
export KUBECONFIG="${cache}/${cluster}.kubeconfig"

start="$(date +%s)"
cleanup() {
  local status=$?
  if [[ "${KEEP_CLUSTER:-}" != "1" ]]; then
    kind_down "${cluster}" "${cache}"
    rm -f "${KUBECONFIG}"
  fi
  printf '\nL2 CRD matrix: %s after %ss\n' "$([[ "${status}" -eq 0 ]] && echo PASS || echo FAIL)" "$(($(date +%s) - start))"
}
trap cleanup EXIT

log "creating kind cluster ${cluster}"
kind_up "${cluster}" "${here}/kind.yaml" "${KUBECONFIG}" "${cache}"

log "running the admission matrix"
go test -tags crd -count=1 -timeout 20m -v ./test/crd/ 2>&1 | tee "${out}/crd.log"
