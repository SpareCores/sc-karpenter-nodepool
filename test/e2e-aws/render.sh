#!/usr/bin/env bash
# Renders the templates of the target into a directory (default: $E2E_OUT/rendered):
#
#   eksctl.yaml           the cluster config
#   nodeclass.yaml        NodeClass (auto) or EC2NodeClass (karpenter), named sckn-ci
#   infra-nodepool.yaml   auto only: the NodePool of the controller
#   scnodepool.yaml       the SCNodePool under test
#   pause.yaml            the workload
#
# Needs E2E_TARGET and the repository secrets (see lib.sh). Used by the
# workflow and, with placeholder values, by validate.sh.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
target_init
template_init

dest="${1:-${E2E_OUT}/rendered}"
mkdir -p "${dest}"
m="${here}/manifests"

render "${here}/eksctl-${E2E_TARGET}.yaml" "${dest}/eksctl.yaml"
render "${m}/scnodepool.yaml" "${dest}/scnodepool.yaml"
render "${m}/pause.yaml" "${dest}/pause.yaml"
case "${E2E_TARGET}" in
  auto)
    render "${m}/auto-nodeclass.yaml" "${dest}/nodeclass.yaml"
    render "${m}/auto-infra-nodepool.yaml" "${dest}/infra-nodepool.yaml"
    ;;
  karpenter)
    render "${m}/karpenter-ec2nodeclass.yaml" "${dest}/nodeclass.yaml"
    ;;
esac
echo "rendered ${E2E_TARGET} into ${dest}"
