#!/usr/bin/env bash
# Collects what is needed to debug a failed run into ${E2E_OUT}/diagnostics
# (uploaded by the workflow as an artifact). Never fails, and redacts the
# account's identifiers: the repository, and with it the artifact, is public.
# Needs E2E_TARGET; KUBECONFIG if the cluster is reachable.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
target_init
dir="${E2E_OUT}/diagnostics"
mkdir -p "${dir}"

# save <file> <command...>: the output of the command, redacted.
save() {
  local file="$1"
  shift
  { "$@" 2>&1 | redact >"${dir}/${file}"; } || true
}

log "diagnostics into ${dir}"
if kubectl version --request-timeout=30s >/dev/null 2>&1; then
  save nodes.txt kubectl get nodes -o wide -L karpenter.sh/nodepool,node.kubernetes.io/instance-type
  save pods.txt kubectl get pods -A -o wide
  save scnodepools.yaml kubectl get scnodepools -o yaml
  save nodepools.yaml kubectl get nodepools -o yaml
  save nodeclaims.yaml kubectl get nodeclaims -o yaml
  save nodeclasses.yaml kubectl get "${E2E_NODECLASS_KIND}" -o yaml
  save events.txt kubectl get events -A --sort-by=.lastTimestamp
  save controller-pods.yaml kubectl -n "${E2E_NAMESPACE}" get pods -o yaml
  save controller-describe.txt kubectl -n "${E2E_NAMESPACE}" describe deployment,pods
  save controller.log kubectl -n "${E2E_NAMESPACE}" logs deployment/sc-karpenter-nodepool --tail=-1
  save controller-previous.log kubectl -n "${E2E_NAMESPACE}" logs deployment/sc-karpenter-nodepool --previous --tail=-1
  if [[ "${E2E_TARGET}" == karpenter ]]; then
    save karpenter.log kubectl -n kube-system logs deployment/karpenter --tail=-1
    save karpenter-describe.txt kubectl -n kube-system describe deployment/karpenter pods -l app.kubernetes.io/name=karpenter
  fi
else
  echo "the cluster is not reachable" >"${dir}/no-cluster.txt"
fi

save cluster.json aws eks describe-cluster --name "${E2E_CLUSTER}" --region "${AWS_REGION}"
save instances.txt aws ec2 describe-instances --region "${AWS_REGION}" --include-managed-resources \
  --filters "Name=tag:eks:eks-cluster-name,Values=${E2E_CLUSTER}" \
  --query 'Reservations[].Instances[].[InstanceId, InstanceType, State.Name, LifecycleType, LaunchTime]' --output text
save stacks.txt aws cloudformation list-stacks --region "${AWS_REGION}" \
  --query "StackSummaries[?starts_with(StackName, 'eksctl-${E2E_CLUSTER}-')].[StackName, StackStatus, StackStatusReason]" --output text
# shellcheck disable=SC2016 # the backticks are JMESPath literals
save stack-events.txt aws cloudformation describe-stack-events --region "${AWS_REGION}" \
  --stack-name "eksctl-${E2E_CLUSTER}-cluster" \
  --query 'StackEvents[?ResourceStatus==`CREATE_FAILED` || ResourceStatus==`DELETE_FAILED`].[Timestamp, LogicalResourceId, ResourceStatusReason]' --output text
ls -l "${dir}"
exit 0
