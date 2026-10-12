#!/usr/bin/env bash
# Before a run: removes what a previous run of this target left behind, so that
# create cluster does not fail on a name that is taken and nothing keeps
# costing money. Needs AWS credentials, E2E_TARGET and AWS_REGION.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
target_init
require_env AWS_REGION

# Instances of a cluster that exists are removed with it, so the cluster goes
# first; then what is left of clusters that no longer exist.
delete_cluster "${E2E_CLUSTER}"
cleanup_orphans "${E2E_CLUSTER}"
cluster_exists "${E2E_CLUSTER}" && die "cluster ${E2E_CLUSTER} is still there"
echo "preflight: clean"
