#!/usr/bin/env bash
# Brings the demo cluster back to where the demo starts: no workloads, no
# SCNodePools, no controller, and no nodes of the demo's pools or of the
# general-purpose pool (the system pool keeps the add-ons' node).
set -euo pipefail

kubectl delete deployment gilty-handpicked gilty-sparecores gilty batch --ignore-not-found --wait=true --timeout=300s
kubectl delete nodepool python-apps --ignore-not-found --wait=true --timeout=300s
kubectl delete scnodepool --all --ignore-not-found --wait=true --timeout=300s
helm uninstall sc-karpenter-nodepool -n sc-karpenter-nodepool --wait --ignore-not-found
kubectl delete namespace sc-karpenter-nodepool --ignore-not-found --wait=true --timeout=300s
kubectl delete crd scnodepools.karpenter.sparecores.com --ignore-not-found
for _ in $(seq 90); do
  [[ "$(kubectl get nodeclaims -l 'karpenter.sh/nodepool in (gilty,batch,python-apps,general-purpose)' -o name | wc -l)" == 0 ]] && break
  sleep 10
done
kubectl get nodeclaims,nodes
