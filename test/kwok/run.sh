#!/usr/bin/env bash
# L3: the controller against real core Karpenter with the KWOK provider in a
# kind cluster (docs/plans/v1/wip/00-overview.md §8.2). Run it in the dev
# container: hack/dev.sh make test-kwok
#
#   KWOK_CORE_VERSION  core minor line (v1.2, v1.8, v1.12, v1.14; default v1.14)
#                      or a full tag listed in core-versions.txt
#   KWOK_VENDORS       comma-separated vendors to run (default: all)
#   KEEP_CLUSTER=1     keep the kind cluster (and reuse it with REUSE_CLUSTER=1)
#   KIND_CLUSTER_NAME  cluster name (default sckn-kwok)
#
# Logs, catalogs and test output land in test/kwok/.out/<minor>/.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
cd "${root}"

KWOK_VERSION="v0.8.0"
cluster="${KIND_CLUSTER_NAME:-sckn-kwok}"
want="${KWOK_CORE_VERSION:-v1.14}"

minor="" core="" offering_keys=""
while read -r m t k _; do
  [[ -z "${m}" || "${m}" == \#* ]] && continue
  if [[ "${want}" == "${m}" || "${want}" == "${t}" ]]; then minor="${m}" core="${t}" offering_keys="${k}"; fi
done <"${here}/core-versions.txt"
if [[ -z "${minor}" ]]; then
  echo "unknown KWOK_CORE_VERSION ${want}; see test/kwok/core-versions.txt" >&2
  exit 2
fi

cache="${here}/.cache"
out="${here}/.out/${minor}"
mkdir -p "${cache}" "${out}"
export KUBECONFIG="${cache}/${cluster}.kubeconfig"

kwok_image="sckn-kwok:${core}"
manager_image="sckn-manager:kwok"
replay_image="sckn-keeper-replay:kwok"

# shellcheck source=test/lib.sh
source "${root}/test/lib.sh"

start="$(date +%s)"
phase_start="${start}"
phase() {
  local now
  now="$(date +%s)"
  printf '    (%ss)\n' "$((now - phase_start))"
  phase_start="${now}"
}

collect() {
  log "collecting logs into ${out}"
  mkdir -p "${out}/logs"
  kubectl get nodes -o wide >"${out}/logs/nodes.txt" 2>&1 || true
  kubectl get pods -A -o wide >"${out}/logs/pods.txt" 2>&1 || true
  kubectl get scnodepools,nodepools,nodeclaims -o yaml >"${out}/logs/karpenter-objects.yaml" 2>&1 || true
  kubectl get events -A --sort-by=.lastTimestamp >"${out}/logs/events.txt" 2>&1 || true
  kubectl -n kube-system logs deploy/karpenter --tail=-1 >"${out}/logs/karpenter-kwok.log" 2>&1 || true
  kubectl -n kube-system logs deploy/kwok-controller --tail=-1 >"${out}/logs/kwok-controller.log" 2>&1 || true
  kubectl -n sc-karpenter-nodepool-system logs deploy/sc-karpenter-nodepool-controller-manager --tail=-1 \
    >"${out}/logs/controller.log" 2>&1 || true
  kubectl -n sc-karpenter-nodepool-system logs deploy/keeper-replay --tail=-1 >"${out}/logs/keeper-replay.log" 2>&1 || true
}

status=0
cleanup() {
  status=$?
  if [[ "${status}" -ne 0 ]] && kubectl version >/dev/null 2>&1; then
    collect
  fi
  if [[ "${KEEP_CLUSTER:-}" != "1" ]]; then
    log "deleting kind cluster ${cluster} and the images"
    kind_down "${cluster}" "${cache}"
    docker rmi -f "${kwok_image}" "${manager_image}" "${replay_image}" >/dev/null 2>&1 || true
    rm -f "${KUBECONFIG}"
  fi
  printf '\nL3 core %s (%s): %s after %ss\n' "${minor}" "${core}" "$([[ "${status}" -eq 0 ]] && echo PASS || echo FAIL)" "$(($(date +%s) - start))"
}
trap cleanup EXIT

gobuild() { # gobuild <dir> <output> <package>
  (cd "$1" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$2" "$3")
}

download() { # download <url> <file>
  [[ -s "$2" ]] || curl -fsSL --retry 3 -o "$2" "$1"
}

# ---------------------------------------------------------------- images ----
log "building images (core ${core})"
src="${cache}/karpenter-${core}"
if [[ ! -d "${src}" ]]; then
  git clone -q --depth 1 --branch "${core}" https://github.com/kubernetes-sigs/karpenter.git "${src}"
fi
kwok_bin="${cache}/kwok-${core}"
[[ -x "${kwok_bin}" ]] || gobuild "${src}" "${kwok_bin}" ./kwok

ctx="${cache}/ctx"
rm -rf "${ctx}"
mkdir -p "${ctx}/kwok" "${ctx}/manager" "${ctx}/replay"

provider_keys=true
[[ "${offering_keys}" == "registered" ]] || provider_keys=false
go run ./test/kwok/catalog/cmd -provider-keys="${provider_keys}" -out "${out}/catalogs" | tee "${out}/catalogs.txt"
cp "${kwok_bin}" "${ctx}/kwok/kwok"
cp -r "${out}/catalogs" "${ctx}/kwok/catalogs"
docker build -q -t "${kwok_image}" -f "${here}/Dockerfile.kwok" "${ctx}/kwok"

gobuild "${root}" "${ctx}/manager/manager" ./cmd
docker build -q -t "${manager_image}" -f "${here}/Dockerfile.manager" "${ctx}/manager"

gobuild "${root}" "${ctx}/replay/replay" ./test/kwok/replay
cp -r "${root}/test/data/sc-snapshot" "${ctx}/replay/snapshot"
docker build -q -t "${replay_image}" -f "${here}/Dockerfile.replay" "${ctx}/replay"
phase

# --------------------------------------------------------------- cluster ----
if [[ "${REUSE_CLUSTER:-}" == "1" ]] && kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
  log "reusing kind cluster ${cluster}"
  kind export kubeconfig --name "${cluster}" --kubeconfig "${KUBECONFIG}"
else
  log "creating kind cluster ${cluster}"
  kind_up "${cluster}" "${here}/kind.yaml" "${KUBECONFIG}" "${cache}"
fi
for img in "${kwok_image}" "${manager_image}" "${replay_image}"; do
  kind load docker-image "${img}" --name "${cluster}"
done
# Workloads must not land on the real node, or Karpenter never provisions for
# them: taint it, and let the infrastructure tolerate the taint.
kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule- >/dev/null 2>&1 || true
kubectl taint nodes --all CriticalAddonsOnly=true:NoSchedule --overwrite
phase

# ------------------------------------------------------------------ kwok ----
log "installing kwok-controller ${KWOK_VERSION} and the stage-fast stages"
for f in kwok.yaml stage-fast.yaml; do
  download "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/${f}" "${cache}/${KWOK_VERSION}-${f}"
done
kubectl apply --server-side -f "${cache}/${KWOK_VERSION}-kwok.yaml"
kubectl wait --for=condition=Established crd/stages.kwok.x-k8s.io --timeout=60s
kubectl apply -f "${cache}/${KWOK_VERSION}-stage-fast.yaml"
# The kwok-controller must run on the real node and manage fake ones only.
kubectl -n kube-system patch deployment kwok-controller --type strategic -p '{"spec":{"template":{"spec":{
  "tolerations":[{"key":"CriticalAddonsOnly","operator":"Exists"}],
  "affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[
    {"matchExpressions":[{"key":"kwok.x-k8s.io/node","operator":"DoesNotExist"}]}]}}}}}}}'
kubectl -n kube-system rollout status deployment/kwok-controller --timeout=180s
phase

# ------------------------------------------------------------- karpenter ----
log "installing core Karpenter ${core} with the KWOK provider"
first_vendor="aws"
helm upgrade --install karpenter "${src}/kwok/charts" --namespace kube-system \
  -f "${here}/values/${minor}.yaml" \
  --set controller.image.repository="${kwok_image%%:*}" \
  --set controller.image.tag="${kwok_image##*:}" \
  --set-string "controller.env[0].name=INSTANCE_TYPES_FILE_PATH" \
  --set-string "controller.env[0].value=/catalogs/${first_vendor}.json"
kubectl wait --for=condition=Established crd/nodepools.karpenter.sh crd/nodeclaims.karpenter.sh crd/kwoknodeclasses.karpenter.kwok.sh --timeout=60s
kubectl -n kube-system rollout status deployment/karpenter --timeout=180s
kubectl apply -f "${here}/manifests/kwoknodeclass.yaml"
phase

# ------------------------------------------------------------ controller ----
log "deploying the controller and the Keeper replay server"
kubectl apply -f "${here}/manifests/upcloudnodeclass-crd.yaml"
kubectl kustomize "${here}/manifests/controller" | sed "s|controller:latest|${manager_image}|" | kubectl apply --server-side -f -
kubectl apply -f "${here}/manifests/keeper-replay.yaml"
kubectl -n sc-karpenter-nodepool-system rollout status deployment/keeper-replay --timeout=120s
kubectl -n sc-karpenter-nodepool-system rollout status deployment/sc-karpenter-nodepool-controller-manager --timeout=120s
phase

# ----------------------------------------------------------------- tests ----
log "running the scenarios"
KWOK_CORE_VERSION="${minor}" KWOK_CORE_TAG="${core}" KWOK_PROVIDER_KEYS="${provider_keys}" KWOK_OUT_DIR="${out}" \
  go test -tags kwok -count=1 -timeout 45m -v ./test/kwok/e2e/ 2>&1 | tee "${out}/e2e.log"
phase
