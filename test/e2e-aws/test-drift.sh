#!/usr/bin/env bash
# L5, part two (after test-select.sh): narrow the selection to exclude every
# instance type in use. The controller updates the NodePool, Karpenter marks
# the nodes of the removed types Drifted (RequirementsDrifted) and replaces
# them, and the pool converges within 15 minutes onto types of the narrowed
# list. The SCNodePool allows 100 removals per refresh and the NodePool's
# budget is 100%, so all three nodes move at once.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
target_init

log "before"
used="$(pool_node_types | sort -u)"
echo "in use: $(tr '\n' ' ' <<<"${used}")"
before_claims="$(claims | jq -r .name | sort)"
[[ -n "${used}" && -n "${before_claims}" ]] || die "no nodes in the pool: run test-select.sh first"
want="$(comm -23 <(expected_types) <(echo "${used}"))"
[[ -n "${want}" ]] || die "excluding ${used} leaves nothing of the expected types"
echo "expected after: $(tr '\n' ' ' <<<"${want}")"

log "narrowing the selection"
patch="$(jq -cn --arg used "${used}" '{spec: {selector: {instanceTypes: {exclude: ($used | split("\n"))}}}}')"
kubectl patch scnodepool "${E2E_SCNODEPOOL}" --type merge -p "${patch}"
start=${SECONDS}

nodepool_narrowed() { [[ "$(nodepool_types "${E2E_SCNODEPOOL}")" == "${want}" ]]; }
wait_for 300 "the NodePool to list exactly the narrowed set" nodepool_narrowed
echo "NodePool updated after $((SECONDS - start))s"

log "waiting for Drifted=True and convergence (15 minutes)"
saw_drift=0
deadline=$((start + 900))
while :; do
  current="$(claims)"
  if jq -e 'select(.drifted)' <<<"${current}" >/dev/null 2>&1; then
    if ((saw_drift == 0)); then
      echo "first Drifted=True after $((SECONDS - start))s"
    fi
    saw_drift=1
  fi
  live="$(jq -rs '[.[] | select(.deleting | not)] | length' <<<"${current}")"
  total="$(jq -rs 'length' <<<"${current}")"
  outside="$(jq -rs --arg want "${want}" '[.[] | select(.deleting | not) | select(.type as $t | ($want | split("\n") | index($t)) == null)] | length' <<<"${current}")"
  drifted="$(jq -rs '[.[] | select(.drifted)] | length' <<<"${current}")"
  ready="$(pause_ready)"
  echo "t=$((SECONDS - start))s claims=${total} live=${live} drifted=${drifted} outside=${outside} pods_ready=${ready}"
  if ((live == 3 && total == 3 && outside == 0 && drifted == 0 && ready == 3)); then
    break
  fi
  if ((SECONDS >= deadline)); then
    kubectl get nodeclaims -l "karpenter.sh/nodepool=${E2E_SCNODEPOOL}" -o wide || true
    die "no convergence within 15 minutes"
  fi
  sleep 5
done

((saw_drift == 1)) || die "never saw a NodeClaim with Drifted=True"
after_claims="$(claims | jq -r .name | sort)"
[[ -z "$(comm -12 <(echo "${before_claims}") <(echo "${after_claims}"))" ]] ||
  die "a NodeClaim from before the change is still there"
types="$(pool_node_types)"
bad="$(comm -23 <(sort -u <<<"${types}") <(echo "${want}"))"
[[ -z "${bad}" ]] || die "nodes of types outside the narrowed set: ${bad}"
echo "drift: PASS after $((SECONDS - start))s (types now: $(sort -u <<<"${types}" | tr '\n' ' '))"
