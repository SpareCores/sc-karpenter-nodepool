#!/usr/bin/env bash
# Deletes the kind cluster and the images that `make kwok-up` (or a
# KEEP_CLUSTER=1 run of run.sh) left behind: hack/dev.sh make kwok-down
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cluster="${KIND_CLUSTER_NAME:-sckn-kwok}"
cache="${here}/.cache"

# shellcheck source=test/lib.sh
source "${here}/../lib.sh"

log "deleting kind cluster ${cluster}"
kind_down "${cluster}" "${cache}"
rm -f "${cache}/${cluster}.kubeconfig"
docker rmi -f sckn-manager:kwok sckn-keeper-replay:kwok >/dev/null 2>&1 || true
docker images --format '{{.Repository}}:{{.Tag}}' | grep '^sckn-kwok:' | xargs -r docker rmi -f >/dev/null 2>&1 || true
