#!/usr/bin/env bash
# L5, part one: the controller turns the SCNodePool into the expected
# NodePool, and Karpenter launches nodes of exactly those types.
#
#   1. apply the SCNodePool; wait for Ready
#   2. the NodePool's instance-type list equals expected-instance-types.txt
#   3. scale the pause Deployment (3 pods, 900m, hostname anti-affinity)
#   4. every node of the pool has an instance type of the list
#
# Needs KUBECONFIG, E2E_TARGET and the variables of lib.sh.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
target_init

"${here}/render.sh" >/dev/null
rendered="${E2E_OUT}/rendered"

log "applying the SCNodePool"
kubectl apply -f "${rendered}/scnodepool.yaml"
kubectl wait --for=condition=Ready "scnodepool/${E2E_SCNODEPOOL}" --timeout=300s ||
  {
    kubectl get scnodepool "${E2E_SCNODEPOOL}" -o yaml
    die "the SCNodePool did not become Ready"
  }
kubectl get scnodepool "${E2E_SCNODEPOOL}"

log "the SCNodePool status"
status="$(kubectl get scnodepool "${E2E_SCNODEPOOL}" -o json)"
want_count="$(expected_types | wc -l)"
[[ "$(jq -r .status.provider <<<"${status}")" == "${E2E_PROVIDER}" ]] ||
  die "provider is $(jq -r .status.provider <<<"${status}"), want ${E2E_PROVIDER}"
[[ "$(jq -r '.status.regions | index(env.AWS_REGION) != null' <<<"${status}")" == true ]] ||
  die "regions $(jq -c .status.regions <<<"${status}") do not contain ${AWS_REGION}"
[[ "$(jq -r .status.selected.count <<<"${status}")" == "${want_count}" ]] ||
  die "status.selected.count is $(jq -r .status.selected.count <<<"${status}"), want ${want_count}"

log "the NodePool lists exactly the expected instance types"
got="$(nodepool_types "${E2E_SCNODEPOOL}")"
if [[ "${got}" != "$(expected_types)" ]]; then
  diff <(expected_types) <(echo "${got}") || true
  die "the NodePool's instance types differ from expected-instance-types.txt (diff: expected < > got)"
fi
echo "${got}" | tr '\n' ' '
echo

log "NodePool Ready"
kubectl wait --for=condition=Ready "nodepool/${E2E_SCNODEPOOL}" --timeout=300s

log "scaling the workload"
kubectl apply -f "${rendered}/pause.yaml"
ready() { [[ "$(pause_ready)" == 3 ]]; }
wait_for 900 "3 pause pods to be Ready" ready
kubectl get nodes -l "karpenter.sh/nodepool=${E2E_SCNODEPOOL}" -L node.kubernetes.io/instance-type,karpenter.sh/capacity-type
kubectl get nodeclaims -l "karpenter.sh/nodepool=${E2E_SCNODEPOOL}"

log "every node is of an expected type"
types="$(pool_node_types)"
[[ "$(wc -l <<<"${types}")" -ge 3 ]] || die "fewer than 3 nodes in the pool: ${types}"
bad="$(comm -23 <(sort -u <<<"${types}") <(expected_types))"
[[ -z "${bad}" ]] || die "nodes of unexpected instance types: ${bad}"
captype="$(kubectl get nodes -l "karpenter.sh/nodepool=${E2E_SCNODEPOOL}" -o json | jq -r '[.items[].metadata.labels["karpenter.sh/capacity-type"]] | unique | join(",")')"
[[ "${captype}" == "${E2E_CAPACITY_TYPE}" ]] || die "node capacity types ${captype}, want ${E2E_CAPACITY_TYPE}"
echo "select: PASS (types in use: $(sort -u <<<"${types}" | tr '\n' ' '))"
