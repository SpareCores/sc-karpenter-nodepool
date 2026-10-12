#!/usr/bin/env bash
# After the EKS cluster with the managed node group exists: Karpenter (chart
# v1.14.1, EKS Pod Identity set up by eksctl, no interruption queue) and the
# EC2NodeClass sckn-ci. Needs KUBECONFIG, the variables of lib.sh and
# E2E_TARGET=karpenter.
set -euo pipefail

KARPENTER_VERSION="1.14.1"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
target_init
[[ "${E2E_TARGET}" == karpenter ]] || die "setup-karpenter.sh is for the karpenter target"

log "rendering"
"${here}/render.sh" >/dev/null
rendered="${E2E_OUT}/rendered"

log "the managed node group is ready"
kubectl wait --for=condition=Ready nodes --all --timeout=600s
kubectl -n kube-system rollout status deployment/coredns --timeout=300s
kubectl -n kube-system rollout status daemonset/eks-pod-identity-agent --timeout=300s

# The pod identity association of kube-system/karpenter comes from eksctl.
# One replica and no interruption queue: spot interruptions are not handled
# (the lane uses on-demand capacity by default).
log "Karpenter ${KARPENTER_VERSION}"
helm upgrade --install karpenter oci://public.ecr.aws/karpenter/karpenter \
  --version "${KARPENTER_VERSION}" --namespace kube-system \
  --set "settings.clusterName=${E2E_CLUSTER}" \
  --set serviceAccount.name=karpenter \
  --set replicas=1 \
  --set controller.resources.requests.cpu=500m \
  --set controller.resources.requests.memory=512Mi \
  --set controller.resources.limits.memory=1Gi \
  --wait --timeout 10m

kubectl get crd ec2nodeclasses.karpenter.k8s.aws nodepools.karpenter.sh nodeclaims.karpenter.sh

log "EC2NodeClass sckn-ci"
kubectl apply -f "${rendered}/nodeclass.yaml"
nodeclass_ready() {
  [[ "$(kubectl get ec2nodeclass sckn-ci -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]]
}
wait_for 600 "EC2NodeClass sckn-ci to be Ready" nodeclass_ready
kubectl get ec2nodeclass sckn-ci -o yaml | redact
