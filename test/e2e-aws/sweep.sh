#!/usr/bin/env bash
# The sweeper: deletes sckn-ci-* clusters older than E2E_MAX_AGE_HOURS (default
# 3, a normal run takes about an hour) and what such clusters leave behind.
# Run by .github/workflows/e2e-aws-sweeper.yml; needs AWS credentials, AWS_REGION
# and eksctl.
#
#   DRY_RUN=1   only list what would be deleted
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
require_env AWS_REGION
max_age="${E2E_MAX_AGE_HOURS:-3}"
now="$(date +%s)"
failed=0

log "clusters ${E2E_PREFIX}-* older than ${max_age} hours in ${AWS_REGION}"
for name in $(aws eks list-clusters --region "${AWS_REGION}" --query 'clusters[]' --output text); do
  [[ "${name}" == "${E2E_PREFIX}-"* ]] || continue
  created="$(aws eks describe-cluster --name "${name}" --region "${AWS_REGION}" --query cluster.createdAt --output text)"
  age=$(((now - $(date -d "${created}" +%s)) / 60))
  if ((age < max_age * 60)); then
    echo "${name}: ${age} minutes old, keeping"
    continue
  fi
  echo "${name}: ${age} minutes old, deleting"
  if [[ "${DRY_RUN:-}" == 1 ]]; then continue; fi
  delete_cluster "${name}" || failed=1
done

log "leftovers of deleted clusters"
if [[ "${DRY_RUN:-}" == 1 ]]; then
  echo "dry run: skipping the orphan cleanup"
else
  # Not younger than 30 minutes, so that a cluster that is being created or
  # deleted right now is left alone.
  cleanup_orphans "${E2E_PREFIX}-*" 30 || failed=1
fi
exit "${failed}"
