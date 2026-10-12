#!/usr/bin/env bash
# After the Auto Mode cluster exists: the custom NodeClass sckn-ci and the
# infrastructure NodePool the controller runs on. Needs KUBECONFIG, the
# variables of lib.sh and E2E_TARGET=auto.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
target_init
[[ "${E2E_TARGET}" == auto ]] || die "setup-auto.sh is for the auto target"

log "rendering"
"${here}/render.sh" >/dev/null
rendered="${E2E_OUT}/rendered"

log "the Auto Mode CRDs"
kubectl get crd nodeclasses.eks.amazonaws.com nodepools.karpenter.sh nodeclaims.karpenter.sh

log "NodeClass sckn-ci and the infrastructure NodePool"
kubectl apply -f "${rendered}/nodeclass.yaml"
kubectl apply -f "${rendered}/infra-nodepool.yaml"

# The NodePool is Ready once Auto Mode accepts it and its NodeClass.
kubectl wait --for=condition=Ready nodepool/sckn-ci-infra --timeout=600s
kubectl get nodeclass sckn-ci -o yaml | redact
kubectl get nodepool sckn-ci-infra -o yaml
