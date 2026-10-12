#!/usr/bin/env bash
# Offline validation of the L5 lane (plan 06): nothing here contacts AWS or any
# cluster. Run it in the dev container: hack/dev.sh make test-e2e-aws
#
#   1. renders every template with obviously fake values
#   2. validates the eksctl configs against eksctl's own JSON schema (the file
#      of the pinned release, downloaded from GitHub with a checksum, cached in
#      test/e2e-aws/.cache/)
#   3. validates the SCNodePool against the CRD of this repository and the
#      infrastructure NodePool against the EKS Auto Mode NodePool CRD
#      (test/crd/fixtures)
#   4. structural checks with yq: the names, roles and addons the scripts rely
#      on are in the configs
#   5. no account identifiers in the lane's files
#
# The same checks run in CI (make test-e2e-aws). `eksctl` itself is never run:
# even its dry-run and utility commands can reach AWS.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
cd "${root}"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"

# Keep in step with EKSCTL_VERSION in install-tools.sh.
SCHEMA_URL="https://raw.githubusercontent.com/eksctl-io/eksctl/v0.231.0/pkg/apis/eksctl.io/v1alpha5/assets/schema.json"
SCHEMA_SHA256="52e0cfd958272cc157e5f63aa2354cc4819d905fc0ddfac76c74d2999efee22e"

cache="${here}/.cache"
mkdir -p "${cache}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

schema="${cache}/eksctl-schema.json"
if ! { [[ -f "${schema}" ]] && echo "${SCHEMA_SHA256}  ${schema}" | sha256sum --check --status; }; then
  log "downloading the eksctl JSON schema"
  curl -fsSL --retry 3 -o "${schema}" "${SCHEMA_URL}"
  echo "${SCHEMA_SHA256}  ${schema}" | sha256sum --check --strict
fi

# Fake values: not real identifiers, and recognisable as such.
export AWS_REGION="us-east-2"
export AWS_ACCOUNT_ID="000000000000"
export SCKN_VPC_ID="vpc-00000000000000000"
export SCKN_SUBNET_IDS="subnet-00000000000000001,subnet-00000000000000002,subnet-00000000000000003"
export SCKN_CLUSTER_ROLE_ARN="arn:aws:iam::000000000000:role/example-cluster"
export SCKN_AUTO_NODE_ROLE_NAME="example-auto-node"
export SCKN_KARPENTER_NODE_ROLE_NAME="example-karpenter-node"
export SCKN_KARPENTER_NODE_ROLE_ARN="arn:aws:iam::000000000000:role/example-karpenter-node"
export SCKN_KARPENTER_CONTROLLER_ROLE_ARN="arn:aws:iam::000000000000:role/example-karpenter-controller"

fail=0
check() { # <description> <yq expression that must be true> <file>
  if [[ "$(yq eval "$2" "$3")" != true ]]; then
    echo "  FAIL: $1 ($2 in $3)" >&2
    fail=1
  fi
}

for target in auto karpenter; do
  log "target ${target}"
  d="${work}/${target}"
  E2E_TARGET="${target}" "${here}/render.sh" "${d}"

  # Every rendered file is YAML, and has no hole left by envsubst.
  for f in "${d}"/*.yaml; do
    yq eval '.' "${f}" >/dev/null
    # shellcheck disable=SC2016 # a literal ${, not an expansion
    if grep -q '\${' "${f}"; then
      echo "  FAIL: unrendered variable in ${f}" >&2
      fail=1
    fi
  done

  yq -o=json eval '.' "${d}/eksctl.yaml" >"${d}/eksctl.json"
  echo "eksctl config against the eksctl schema"
  python3 -I "${here}/validate.py" "${schema}" "${d}/eksctl.json" || fail=1
  # The validation must be able to fail: a misspelled field is an error.
  jq '.autoModeConfigg = {} | .iam.serviceRoleARNN = "x"' "${d}/eksctl.json" >"${d}/eksctl-broken.json"
  python3 -I "${here}/validate.py" "${schema}" "${d}/eksctl-broken.json" --expect-invalid 2>/dev/null || {
    echo "  FAIL: the schema accepted a config with unknown fields" >&2
    fail=1
  }

  echo "SCNodePool against the CRD"
  yq -o=json eval '.spec.versions[0].schema.openAPIV3Schema' config/crd/bases/karpenter.sparecores.com_scnodepools.yaml >"${d}/scnodepool-schema.json"
  yq -o=json eval '.' "${d}/scnodepool.yaml" >"${d}/scnodepool.json"
  python3 -I "${here}/validate.py" "${d}/scnodepool-schema.json" "${d}/scnodepool.json" || fail=1

  if [[ "${target}" == auto ]]; then
    echo "infrastructure NodePool against the EKS Auto Mode NodePool CRD"
    yq -o=json eval '.spec.versions[0].schema.openAPIV3Schema' test/crd/fixtures/eks-auto-mode/karpenter.sh_nodepools.yaml >"${d}/nodepool-schema.json"
    yq -o=json eval '.' "${d}/infra-nodepool.yaml" >"${d}/infra-nodepool.json"
    python3 -I "${here}/validate.py" "${d}/nodepool-schema.json" "${d}/infra-nodepool.json" || fail=1
  fi
done

log "structural checks"
a="${work}/auto"
k="${work}/karpenter"
check "auto: cluster name" '.metadata.name == "sckn-ci-auto"' "${a}/eksctl.yaml"
check "auto: region" '.metadata.region == "us-east-2"' "${a}/eksctl.yaml"
check "auto: Auto Mode with only the system pool" '.autoModeConfig.enabled == true and (.autoModeConfig.nodePools | length) == 1 and .autoModeConfig.nodePools[0] == "system"' "${a}/eksctl.yaml"
check "auto: node role ARN of the Auto Mode role" '.autoModeConfig.nodeRoleARN == "arn:aws:iam::000000000000:role/example-auto-node"' "${a}/eksctl.yaml"
check "auto: pre-created cluster role" '.iam.serviceRoleARN == "arn:aws:iam::000000000000:role/example-cluster"' "${a}/eksctl.yaml"
check "auto: existing VPC and three subnets" '.vpc.id == "vpc-00000000000000000" and (.vpc.subnets.public | length) == 3' "${a}/eksctl.yaml"
check "auto: no node groups" '(.managedNodeGroups == null) and (.nodeGroups == null)' "${a}/eksctl.yaml"
check "auto: access entries" '.accessConfig.authenticationMode == "API"' "${a}/eksctl.yaml"
check "auto: NodeClass sckn-ci with the node role, 20Gi and the three subnets" '.kind == "NodeClass" and .metadata.name == "sckn-ci" and .spec.role == "example-auto-node" and .spec.ephemeralStorage.size == "20Gi" and (.spec.subnetSelectorTerms | length) == 3' "${a}/nodeclass.yaml"
check "auto: the NodeClass is not called default" '.metadata.name != "default"' "${a}/nodeclass.yaml"
check "auto: infrastructure NodePool uses sckn-ci" '.metadata.name == "sckn-ci-infra" and .spec.template.spec.nodeClassRef.name == "sckn-ci" and .spec.template.spec.nodeClassRef.group == "eks.amazonaws.com"' "${a}/infra-nodepool.yaml"
check "auto: SCNodePool refers to the EKS NodeClass" '.spec.nodePoolTemplate.spec.template.spec.nodeClassRef.group == "eks.amazonaws.com" and .spec.nodePoolTemplate.spec.template.spec.nodeClassRef.kind == "NodeClass"' "${a}/scnodepool.yaml"
check "karpenter: cluster name" '.metadata.name == "sckn-ci-karpenter"' "${k}/eksctl.yaml"
check "karpenter: one t4g.medium managed node group with the Karpenter node role" '(.managedNodeGroups | length) == 1 and .managedNodeGroups[0].instanceType == "t4g.medium" and .managedNodeGroups[0].iam.instanceRoleARN == "arn:aws:iam::000000000000:role/example-karpenter-node"' "${k}/eksctl.yaml"
check "karpenter: pod identity for kube-system/karpenter" '.iam.podIdentityAssociations[0].namespace == "kube-system" and .iam.podIdentityAssociations[0].serviceAccountName == "karpenter" and .iam.podIdentityAssociations[0].roleARN == "arn:aws:iam::000000000000:role/example-karpenter-controller"' "${k}/eksctl.yaml"
check "karpenter: the pod identity agent addon" '[.addons[].name] | contains(["eks-pod-identity-agent"])' "${k}/eksctl.yaml"
check "karpenter: no Auto Mode" '.autoModeConfig == null' "${k}/eksctl.yaml"
check "karpenter: EC2NodeClass sckn-ci" '.kind == "EC2NodeClass" and .metadata.name == "sckn-ci" and .spec.role == "example-karpenter-node" and (.spec.subnetSelectorTerms | length) == 3' "${k}/nodeclass.yaml"
check "karpenter: SCNodePool refers to the EC2NodeClass" '.spec.nodePoolTemplate.spec.template.spec.nodeClassRef.group == "karpenter.k8s.aws" and .spec.nodePoolTemplate.spec.template.spec.nodeClassRef.kind == "EC2NodeClass"' "${k}/scnodepool.yaml"
check "both: the SCNodePool limits cpu to 16 and allows removals" '.spec.nodePoolTemplate.spec.limits.cpu == "16" and .spec.churn.maxRemovalsPerRefresh == 100 and .spec.nodePoolTemplate.spec.disruption.budgets[0].nodes == "100%"' "${a}/scnodepool.yaml"
check "both: the workload is 3 x 900m with hostname anti-affinity" '.spec.replicas == 3 and .spec.template.spec.containers[0].resources.requests.cpu == "900m" and .spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].topologyKey == "kubernetes.io/hostname"' "${a}/pause.yaml"
for f in "${here}"/manifests/values-*.yaml; do
  yq eval '.' "${f}" >/dev/null
done

log "no account identifiers in the lane's files"
# Twelve digits that are not the placeholder account, and ids of real-looking
# resources (hex ids with 8 or 17 digits).
if grep -rEn '(^|[^0-9A-Za-z])[1-9][0-9]{11}([^0-9A-Za-z]|$)|(vpc|subnet|sg|i)-[0-9a-f]{8,17}' \
  test/e2e-aws .github/workflows/e2e-aws.yml .github/workflows/e2e-aws-sweeper.yml |
  grep -v 'validate.sh:' >&2; then
  echo "  FAIL: something that looks like an AWS identifier (listed above)" >&2
  fail=1
fi

if ((fail)); then
  echo "e2e-aws validation: FAIL" >&2
  exit 1
fi
echo "e2e-aws validation: PASS"
