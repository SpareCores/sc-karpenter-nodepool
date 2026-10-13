#!/usr/bin/env bash
# After a run, whatever its outcome: deletes the workload, the SCNodePool and
# the NodePools (so that Karpenter terminates its instances while it still
# runs), then the cluster, then what is left in the account. Exits non-zero if
# anything of the cluster is left: a leaked control plane costs $2.40 a day.
# Needs AWS credentials, E2E_TARGET and AWS_REGION; KUBECONFIG if the cluster
# was created.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
target_init
require_env AWS_REGION

try() { "$@" || warn "failed (ignored): $*"; }

# Only the test pool. The infrastructure pool of Auto Mode (sckn-ci-infra) and
# its NodeClass go with the cluster: its last node cannot be drained, since the
# PodDisruptionBudgets of the add-ons on it (metrics-server) allow no
# eviction, and Auto Mode would wait out the termination grace period (24h).
nodeclaims_gone() { [[ "$(kubectl get nodeclaims -l "karpenter.sh/nodepool=${E2E_SCNODEPOOL}" -o name | wc -l)" == 0 ]]; }

if kubectl version --request-timeout=30s >/dev/null 2>&1; then
  log "removing the workload, the SCNodePool and the NodePool"
  try kubectl delete deployment pause --ignore-not-found --wait=true --timeout=300s
  try kubectl delete scnodepool --all --ignore-not-found --wait=true --timeout=300s
  try kubectl delete nodepool "${E2E_SCNODEPOOL}" --ignore-not-found --wait=true --timeout=900s
  # Karpenter terminates the instances behind the NodeClaims; give it time.
  deadline=$((SECONDS + 900))
  until nodeclaims_gone; do
    if ((SECONDS >= deadline)); then
      warn "NodeClaims are left; the cluster deletion and the orphan cleanup remove their instances"
      break
    fi
    sleep 10
  done
  # Self-managed Karpenter removes the instance profile it made for the
  # EC2NodeClass; the Auto Mode NodeClass is still in use by sckn-ci-infra.
  if [[ "${E2E_TARGET}" == karpenter ]]; then
    try kubectl delete "${E2E_NODECLASS_KIND}" sckn-ci --ignore-not-found --wait=true --timeout=300s
  fi
else
  echo "the cluster is not reachable; skipping the Kubernetes cleanup"
fi

status=0
delete_cluster "${E2E_CLUSTER}" || status=1
cleanup_orphans "${E2E_CLUSTER}" || status=1
if cluster_exists "${E2E_CLUSTER}"; then
  warn "cluster ${E2E_CLUSTER} STILL EXISTS: delete it by hand or run the sweeper"
  status=1
fi
exit "${status}"
