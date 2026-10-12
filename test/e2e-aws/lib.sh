#!/usr/bin/env bash
# shellcheck disable=SC2016 # JMESPath backticks and a literal ${NAME} pattern are not expansions
# Helpers shared by the scripts of the L5 lane (test/e2e-aws, overview §8.5) and
# by the sweeper. Source it; it only defines functions and variables.
#
# These scripts talk to AWS and to the clusters they create. They run in the
# GitHub workflows .github/workflows/e2e-aws.yml and e2e-aws-sweeper.yml,
# never on a developer machine.
#
# The identifiers of the account come from repository variables, exported to
# the job environment by the workflow: AWS_REGION, AWS_ACCOUNT_ID,
# SCKN_VPC_ID, SCKN_SUBNET_IDS (comma or space separated),
# SCKN_CLUSTER_ROLE_ARN, SCKN_AUTO_NODE_ROLE_NAME,
# SCKN_KARPENTER_NODE_ROLE_NAME, SCKN_KARPENTER_NODE_ROLE_ARN and
# SCKN_KARPENTER_CONTROLLER_ROLE_ARN. The run itself is described by
#
#   E2E_TARGET         auto | karpenter
#   E2E_CAPACITY_TYPE  on-demand (default) | spot
#   E2E_IMAGE_TAG      tag of the controller image (sha-<commit>)
#   E2E_OUT            directory for diagnostics (default: a temp directory)

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export E2E_DIR

# Every cluster of the lane has this prefix; the role of the workflow can only
# manage such clusters.
E2E_PREFIX="sckn-ci"

log() { printf '\n==> %s\n' "$*"; }
warn() { printf '::warning::%s\n' "$*" >&2; }
die() {
  printf '::error::%s\n' "$*" >&2
  exit 1
}

# require_env NAME...: stops unless every variable is set and not empty.
require_env() {
  local name
  for name in "$@"; do
    [[ -n "${!name:-}" ]] || die "environment variable ${name} is not set"
  done
}

# wait_for <timeout seconds> <what> <command...>: runs the command every five
# seconds until it succeeds.
wait_for() {
  local timeout="$1" what="$2" deadline
  shift 2
  deadline=$((SECONDS + timeout))
  until "$@"; do
    if ((SECONDS >= deadline)); then
      die "timed out after ${timeout}s waiting for ${what}"
    fi
    sleep 5
  done
}

# ---------------------------------------------------------------------------
# The target
# ---------------------------------------------------------------------------

# target_init: derives the per-target names from E2E_TARGET.
target_init() {
  require_env E2E_TARGET
  case "${E2E_TARGET}" in
    auto)
      E2E_CLUSTER="${E2E_PREFIX}-auto"
      E2E_NODECLASS_GROUP="eks.amazonaws.com"
      E2E_NODECLASS_KIND="NodeClass"
      E2E_PROVIDER="aws-auto"
      ;;
    karpenter)
      E2E_CLUSTER="${E2E_PREFIX}-karpenter"
      E2E_NODECLASS_GROUP="karpenter.k8s.aws"
      E2E_NODECLASS_KIND="EC2NodeClass"
      E2E_PROVIDER="aws"
      ;;
    *) die "E2E_TARGET must be auto or karpenter, got ${E2E_TARGET}" ;;
  esac
  E2E_CAPACITY_TYPE="${E2E_CAPACITY_TYPE:-on-demand}"
  case "${E2E_CAPACITY_TYPE}" in
    on-demand | spot) ;;
    *) die "E2E_CAPACITY_TYPE must be on-demand or spot, got ${E2E_CAPACITY_TYPE}" ;;
  esac
  E2E_SCNODEPOOL="sckn-e2e"
  E2E_NAMESPACE="sc-karpenter-nodepool"
  E2E_OUT="${E2E_OUT:-$(mktemp -d)}"
  export E2E_CLUSTER E2E_NODECLASS_GROUP E2E_NODECLASS_KIND E2E_PROVIDER E2E_CAPACITY_TYPE \
    E2E_SCNODEPOOL E2E_NAMESPACE E2E_OUT
}

# expected_types: the instance types the e2e SCNodePool must select (one per
# line, sorted). Plain files, so that a Go test can check them against the
# committed snapshot (internal/providers/e2eaws_test.go).
expected_types() { grep -v '^#' "${E2E_DIR}/expected-instance-types.txt" | grep -v '^$' | sort; }

# ---------------------------------------------------------------------------
# Rendering the templates (envsubst, a fixed list of variables)
# ---------------------------------------------------------------------------

# Variables the templates may use. Anything else, such as a "$" in a script
# inside a manifest, is left alone.
E2E_TEMPLATE_VARS=(
  E2E_CLUSTER E2E_CAPACITY_TYPE E2E_NODECLASS_GROUP E2E_NODECLASS_KIND E2E_SCNODEPOOL
  E2E_SUBNETS_MAP E2E_SUBNET_TERMS E2E_AUTO_NODE_ROLE_ARN
  AWS_REGION AWS_ACCOUNT_ID SCKN_VPC_ID SCKN_CLUSTER_ROLE_ARN SCKN_AUTO_NODE_ROLE_NAME
  SCKN_KARPENTER_NODE_ROLE_NAME SCKN_KARPENTER_NODE_ROLE_ARN SCKN_KARPENTER_CONTROLLER_ROLE_ARN
)

# subnet_ids: one subnet id per line from SCKN_SUBNET_IDS ("a,b", "a b" or a
# JSON array).
subnet_ids() {
  tr -d '[]"' <<<"${SCKN_SUBNET_IDS}" | tr -s ',[:space:]' '\n' | grep -v '^$'
}

# template_init: the values the templates need on top of the variables of the
# job. Needs the repository variables, so it is separate from target_init.
template_init() {
  require_env AWS_REGION AWS_ACCOUNT_ID SCKN_VPC_ID SCKN_SUBNET_IDS SCKN_CLUSTER_ROLE_ARN \
    SCKN_AUTO_NODE_ROLE_NAME SCKN_KARPENTER_NODE_ROLE_NAME SCKN_KARPENTER_NODE_ROLE_ARN \
    SCKN_KARPENTER_CONTROLLER_ROLE_ARN
  local map="" terms="" n=0 id
  while read -r id; do
    n=$((n + 1))
    map+="${map:+, }s${n}: {id: ${id}}"
    terms+="${terms:+, }{id: ${id}}"
  done < <(subnet_ids)
  ((n > 0)) || die "SCKN_SUBNET_IDS has no subnet id"
  # Single-line flow collections, so that envsubst can put them into a
  # template without knowing the indentation.
  E2E_SUBNETS_MAP="{${map}}"
  E2E_SUBNET_TERMS="[${terms}]"
  E2E_AUTO_NODE_ROLE_ARN="arn:aws:iam::${AWS_ACCOUNT_ID}:role/${SCKN_AUTO_NODE_ROLE_NAME}"
  export E2E_SUBNETS_MAP E2E_SUBNET_TERMS E2E_AUTO_NODE_ROLE_ARN
}

# render <template> <destination>
render() {
  local vars=""
  local v
  for v in "${E2E_TEMPLATE_VARS[@]}"; do vars+="\${${v}} "; done
  envsubst "${vars}" <"$1" >"$2"
  # A variable that is unset or empty would leave a hole in the document.
  if grep -n '\${[A-Z0-9_]*}' "$2" >&2; then
    die "unrendered variables in $2 (listed above)"
  fi
}

# ---------------------------------------------------------------------------
# Redaction: the repository is public, the account is not.
# ---------------------------------------------------------------------------

# redact: filters stdin. The workflow also masks these values in the log; this
# covers files that are uploaded as artifacts.
redact() {
  local args=() v id
  for v in AWS_ACCOUNT_ID SCKN_VPC_ID SCKN_CLUSTER_ROLE_ARN SCKN_AUTO_NODE_ROLE_NAME \
    SCKN_KARPENTER_NODE_ROLE_NAME SCKN_KARPENTER_NODE_ROLE_ARN SCKN_KARPENTER_CONTROLLER_ROLE_ARN; do
    if [[ -n "${!v:-}" ]]; then
      args+=(-e "s|$(printf '%s' "${!v}" | sed 's/[][\.*^$|]/\\&/g')|<${v}>|g")
    fi
  done
  if [[ -n "${SCKN_SUBNET_IDS:-}" ]]; then
    while read -r id; do
      args+=(-e "s|$(printf '%s' "${id}" | sed 's/[][\.*^$|]/\\&/g')|<subnet>|g")
    done < <(subnet_ids)
  fi
  # Whatever else looks like an account id inside an ARN.
  args+=(-E -e 's/(arn:aws[a-z-]*:[a-z0-9-]*:[a-z0-9-]*:)[0-9]{12}:/\1<account>:/g')
  sed "${args[@]}"
}

# ---------------------------------------------------------------------------
# AWS: clusters and what they leave behind
# ---------------------------------------------------------------------------

# cluster_exists <name>: 0 if the EKS cluster exists (in any state), 1 if
# not; anything else is an error, never "not there".
cluster_exists() {
  local out
  if out="$(aws eks describe-cluster --name "$1" --region "${AWS_REGION}" 2>&1)"; then
    return 0
  fi
  if grep -q 'ResourceNotFoundException' <<<"${out}"; then
    return 1
  fi
  die "describing cluster $1: ${out}"
}

# stack_exists <stack name>: the eksctl CloudFormation stack, in any state.
stack_exists() {
  local out
  if out="$(aws cloudformation describe-stacks --stack-name "$1" --region "${AWS_REGION}" 2>&1)"; then
    return 0
  fi
  if grep -q 'does not exist' <<<"${out}"; then
    return 1
  fi
  die "describing stack $1: ${out}"
}

# delete_cluster <name>: deletes a cluster of this lane and the eksctl stacks
# around it. Does nothing if neither exists. Returns 1 if something is left.
delete_cluster() {
  local name="$1"
  [[ "${name}" == "${E2E_PREFIX}-"* ]] || die "refusing to delete ${name}: not a ${E2E_PREFIX}-* cluster"
  if ! cluster_exists "${name}" && ! stack_exists "eksctl-${name}-cluster"; then
    echo "cluster ${name}: nothing to delete"
    return 0
  fi
  log "deleting cluster ${name}"
  if ! timeout 50m eksctl delete cluster --name "${name}" --region "${AWS_REGION}" --wait --disable-nodegroup-eviction; then
    warn "eksctl could not delete ${name}; falling back to the EKS API"
    delete_cluster_with_api "${name}"
  fi
  if cluster_exists "${name}"; then
    warn "cluster ${name} still exists"
    return 1
  fi
}

# delete_cluster_with_api <name>: node groups, then the cluster, with the AWS
# CLI. The CloudFormation stacks stay for cleanup_orphans.
delete_cluster_with_api() {
  local name="$1" ng
  if ! cluster_exists "${name}"; then return 0; fi
  for ng in $(aws eks list-nodegroups --cluster-name "${name}" --region "${AWS_REGION}" --query 'nodegroups[]' --output text); do
    aws eks delete-nodegroup --cluster-name "${name}" --nodegroup-name "${ng}" --region "${AWS_REGION}" >/dev/null || true
  done
  for ng in $(aws eks list-nodegroups --cluster-name "${name}" --region "${AWS_REGION}" --query 'nodegroups[]' --output text); do
    aws eks wait nodegroup-deleted --cluster-name "${name}" --nodegroup-name "${ng}" --region "${AWS_REGION}" || true
  done
  aws eks delete-cluster --name "${name}" --region "${AWS_REGION}" >/dev/null || true
  aws eks wait cluster-deleted --name "${name}" --region "${AWS_REGION}" || true
}

# cleanup_orphans <cluster name glob> [min age in minutes]
#
# Removes what a deleted cluster of this lane leaves behind, for the clusters
# whose name matches the glob and that no longer exist: instances tagged
# eks:eks-cluster-name (Auto Mode instances are only listed with
# --include-managed-resources), Karpenter launch templates, instance profiles
# named <cluster>_<id> and eksctl stacks in DELETE_FAILED. Nothing here
# fails the caller: it reports what it could not remove and returns 1.
cleanup_orphans() {
  local pattern="$1" min_age="${2:-0}" failed=0
  [[ "${pattern}" == "${E2E_PREFIX}-"* ]] || die "refusing to clean up ${pattern}: not a ${E2E_PREFIX}-* pattern"
  local id cluster launched name now cutoff
  now="$(date +%s)"
  cutoff=$((now - min_age * 60))

  log "orphaned instances of ${pattern}"
  while read -r id launched cluster; do
    [[ -n "${id}" ]] || continue
    if cluster_exists "${cluster}"; then continue; fi
    if (($(date -d "${launched}" +%s) > cutoff)); then continue; fi
    echo "terminating ${id} (cluster ${cluster}, launched ${launched})"
    aws ec2 terminate-instances --region "${AWS_REGION}" --instance-ids "${id}" >/dev/null || {
      warn "could not terminate ${id}"
      failed=1
    }
  done < <(aws ec2 describe-instances --region "${AWS_REGION}" --include-managed-resources \
    --filters "Name=tag:eks:eks-cluster-name,Values=${pattern}" \
    "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].[InstanceId, LaunchTime, Tags[?Key==`eks:eks-cluster-name`]|[0].Value]' --output text)

  log "orphaned launch templates of ${pattern}"
  while read -r id cluster; do
    [[ -n "${id}" ]] || continue
    if cluster_exists "${cluster}"; then continue; fi
    echo "deleting launch template ${id} (cluster ${cluster})"
    aws ec2 delete-launch-template --region "${AWS_REGION}" --launch-template-id "${id}" >/dev/null || {
      warn "could not delete launch template ${id}"
      failed=1
    }
  done < <(aws ec2 describe-launch-templates --region "${AWS_REGION}" \
    --filters "Name=tag:karpenter.k8s.aws/cluster,Values=${pattern}" \
    --query 'LaunchTemplates[].[LaunchTemplateId, Tags[?Key==`karpenter.k8s.aws/cluster`]|[0].Value]' --output text)

  log "orphaned instance profiles of ${pattern}"
  local role
  for name in $(aws iam list-instance-profiles --query 'InstanceProfiles[].InstanceProfileName' --output text); do
    cluster="${name%%_*}"
    # shellcheck disable=SC2053 # the pattern is a glob on purpose
    [[ "${cluster}" == ${pattern} && "${name}" == *_* ]] || continue
    if cluster_exists "${cluster}"; then continue; fi
    echo "deleting instance profile ${name}"
    for role in $(aws iam get-instance-profile --instance-profile-name "${name}" --query 'InstanceProfile.Roles[].RoleName' --output text); do
      aws iam remove-role-from-instance-profile --instance-profile-name "${name}" --role-name "${role}" || true
    done
    aws iam delete-instance-profile --instance-profile-name "${name}" || {
      warn "could not delete instance profile ${name}"
      failed=1
    }
  done

  log "eksctl stacks of ${pattern} in DELETE_FAILED"
  for name in $(aws cloudformation list-stacks --region "${AWS_REGION}" --stack-status-filter DELETE_FAILED \
    --query 'StackSummaries[].StackName' --output text); do
    # shellcheck disable=SC2053 # the pattern is a glob on purpose
    [[ "${name}" == eksctl-${pattern}-* ]] || continue
    echo "deleting stack ${name}"
    if aws cloudformation delete-stack --region "${AWS_REGION}" --stack-name "${name}" &&
      aws cloudformation wait stack-delete-complete --region "${AWS_REGION}" --stack-name "${name}"; then
      continue
    fi
    warn "could not delete stack ${name}"
    failed=1
  done
  return "${failed}"
}

# ---------------------------------------------------------------------------
# The cluster under test
# ---------------------------------------------------------------------------

# nodepool_types <nodepool>: the node.kubernetes.io/instance-type values of
# the NodePool, one per line, sorted.
nodepool_types() {
  kubectl get nodepool "$1" -o json |
    jq -r '.spec.template.spec.requirements[] | select(.key == "node.kubernetes.io/instance-type") | .values[]' |
    sort
}

# pool_node_types: the instance types of the nodes of the SCNodePool's
# NodePool, one per line (with repetitions), sorted.
pool_node_types() {
  kubectl get nodes -l "karpenter.sh/nodepool=${E2E_SCNODEPOOL}" -o json |
    jq -r '.items[].metadata.labels["node.kubernetes.io/instance-type"]' | sort
}

# pause_ready: number of Ready pause pods.
pause_ready() {
  kubectl get pods -l app=pause -o json |
    jq '[.items[] | select(.metadata.deletionTimestamp == null) | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length'
}

# claims: the NodeClaims of the SCNodePool's NodePool as compact JSON lines
# {name, type, drifted, deleting}.
claims() {
  kubectl get nodeclaims -l "karpenter.sh/nodepool=${E2E_SCNODEPOOL}" -o json |
    jq -c '.items[] | {name: .metadata.name,
      type: .metadata.labels["node.kubernetes.io/instance-type"],
      drifted: any(.status.conditions[]?; .type == "Drifted" and .status == "True"),
      deleting: (.metadata.deletionTimestamp != null)}'
}
