#!/usr/bin/env bash
# Creates or deletes the demo cluster (cluster.yaml) in the default VPC of
# AWS_REGION. Needs AWS credentials that may create EKS clusters and IAM roles.
#
#   cluster.sh up | down
#
# `down` first deletes the demo's workloads and SCNodePools, so that Auto Mode
# terminates their instances, then the cluster, then checks that no instance
# of the cluster is left.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${AWS_REGION:?}"
export DEMO_CLUSTER="${DEMO_CLUSTER:-sc-demo}"

up() {
  DEMO_VPC_ID="$(aws ec2 describe-vpcs --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId' --output text)"
  DEMO_SUBNETS_MAP="$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=${DEMO_VPC_ID}" Name=default-for-az,Values=true \
    --query 'Subnets[].[AvailabilityZone,SubnetId]' --output json |
    jq -c 'sort | .[:3] | map({(.[0]): {id: .[1]}}) | add')"
  export DEMO_VPC_ID DEMO_SUBNETS_MAP
  envsubst <"${here}/cluster.yaml" >/tmp/cluster.rendered.yaml
  eksctl create cluster --config-file /tmp/cluster.rendered.yaml --kubeconfig "${KUBECONFIG}"
}

down() {
  if kubectl version --request-timeout=20s >/dev/null 2>&1; then
    kubectl delete deployment gilty-handpicked gilty-sparecores gilty batch --ignore-not-found --wait=true --timeout=300s || true
    kubectl delete nodepool python-apps --ignore-not-found --wait=true --timeout=300s || true
    kubectl delete scnodepool --all --ignore-not-found --wait=true --timeout=300s || true
    for _ in $(seq 60); do
      [[ "$(kubectl get nodeclaims -l 'karpenter.sh/nodepool in (gilty,batch,python-apps)' -o name | wc -l)" == 0 ]] && break
      sleep 10
    done
    helm uninstall sc-karpenter-nodepool -n sc-karpenter-nodepool --wait || true
  fi
  eksctl delete cluster --name "${DEMO_CLUSTER}" --region "${AWS_REGION}" --wait --disable-nodegroup-eviction
  left="$(aws ec2 describe-instances --filters "Name=tag:eks:eks-cluster-name,Values=${DEMO_CLUSTER}" \
    Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' --output text)"
  if [[ -n "${left}" ]]; then
    echo "instances of ${DEMO_CLUSTER} are left: ${left}" >&2
    exit 1
  fi
  echo "cluster ${DEMO_CLUSTER} and its instances are gone"
}

"${1:?usage: cluster.sh up|down}"
