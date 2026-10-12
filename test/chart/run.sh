#!/usr/bin/env bash
# Helm chart checks (no cluster needed): run them in the dev container,
#
#   hack/dev.sh make test-chart
#
#   UPDATE=1   rewrite the golden files from the current templates
#
# 1. helm lint --strict for every values file of test/chart/values and the
#    schema rejecting bad values;
# 2. helm template of every values file against test/chart/golden;
# 3. the CRD of the chart equals config/crd, the RBAC rules equal
#    config/rbac/role.yaml;
# 4. every flag of cmd/main.go can be set from the chart.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
chart="${root}/charts/sc-karpenter-nodepool"
release="sckn"
namespace="sc-karpenter-nodepool"

fail=0
err() {
  printf 'FAIL: %s\n' "$*" >&2
  fail=1
}

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

template() { # template <helm args...>
  helm template "${release}" "${chart}" --namespace "${namespace}" "$@"
}

echo "==> helm lint"
helm lint --strict "${chart}" --set crds.install=true
for f in "${here}"/values/*.yaml; do
  helm lint --strict "${chart}" -f "${f}"
done

echo "==> values schema"
for bad in "foo=bar" "metrics.port=70000" "keeper.url=keeper" "logging.encoder=xml" "image.digest=latest"; do
  if template --set "${bad}" >/dev/null 2>&1; then
    err "the values schema accepts --set ${bad}"
  fi
done

echo "==> golden output"
mkdir -p "${here}/golden"
for f in "${here}"/values/*.yaml; do
  name="$(basename "${f}" .yaml)"
  template -f "${f}" >"${tmp}/${name}.yaml"
  if [[ "${UPDATE:-}" == "1" ]]; then
    cp "${tmp}/${name}.yaml" "${here}/golden/${name}.yaml"
  elif ! diff -u "${here}/golden/${name}.yaml" "${tmp}/${name}.yaml"; then
    err "rendered output of values/${name}.yaml differs from golden/${name}.yaml (UPDATE=1 to accept)"
  fi
done
if [[ "${UPDATE:-}" != "1" ]]; then
  for g in "${here}"/golden/*.yaml; do
    [[ -f "${here}/values/$(basename "${g}")" ]] || err "golden/$(basename "${g}") has no values file"
  done
fi

echo "==> chart CRD and RBAC are in sync with config/"
# Apart from the chart labels and the keep annotation the rendered CRD is the
# generated one.
template -s templates/crd.yaml | grep -v "^# Source:" >"${tmp}/crd.yaml"
yq 'del(.metadata.labels) | del(.metadata.annotations["helm.sh/resource-policy"])' "${tmp}/crd.yaml" >"${tmp}/crd-plain.yaml"
yq '.' "${root}/config/crd/bases/karpenter.sparecores.com_scnodepools.yaml" >"${tmp}/crd-config.yaml"
diff -u "${tmp}/crd-config.yaml" "${tmp}/crd-plain.yaml" || err "the CRD of the chart differs from config/crd (make chart-crds)"
[[ "$(yq '.metadata.annotations["helm.sh/resource-policy"]' "${tmp}/crd.yaml")" == "keep" ]] || err "the CRD is not kept on uninstall"
template -s templates/crd.yaml --set crds.keep=false | yq -e '.metadata.annotations | has("helm.sh/resource-policy") | not' >/dev/null ||
  err "crds.keep=false still sets the keep annotation"
[[ -z "$(template --set crds.install=false -s templates/crd.yaml 2>/dev/null || true)" ]] || err "crds.install=false still renders the CRD"
diff -u <(yq '.rules' "${root}/config/rbac/role.yaml") "${chart}/files/manager-rules.yaml" ||
  err "files/manager-rules.yaml differs from config/rbac/role.yaml (make chart-rbac)"
# The default ClusterRole carries every rule of the generated one.
template -s templates/rbac.yaml --set crds.install=false |
  yq 'select(.kind == "ClusterRole" and .metadata.name == "'"${release}-sc-karpenter-nodepool-manager"'") | .rules' >"${tmp}/rules.yaml"
diff -u <(yq -o=json -I=0 '.rules' "${root}/config/rbac/role.yaml") <(yq -o=json -I=0 '.' "${tmp}/rules.yaml") ||
  err "the rendered ClusterRole differs from config/rbac/role.yaml"
# Restricting the NodeClass groups removes the others.
template -s templates/rbac.yaml --set 'rbac.nodeClassGroups={karpenter.k8s.aws}' |
  yq 'select(.kind == "ClusterRole" and .metadata.name == "'"${release}-sc-karpenter-nodepool-manager"'") | .rules[].apiGroups[]' |
  grep -q 'karpenter.azure.com' && err "rbac.nodeClassGroups keeps the rules of other groups"

echo "==> every manager flag is settable from the chart"
mapfile -t flags < <(grep -oE 'flag\.[A-Za-z]+Var\(&[A-Za-z]+, "[a-z-]+"' "${root}/cmd/main.go" | sed -E 's/.*"([a-z-]+)"/\1/')
[[ "${#flags[@]}" -ge 10 ]] || err "found only ${#flags[@]} flags in cmd/main.go"
# The zap flags are bound by controller-runtime's zap.Options.BindFlags.
flags+=(zap-devel zap-encoder zap-log-level zap-stacktrace-level zap-time-encoding)
for fl in "${flags[@]}"; do
  grep -q -- "--${fl}" "${chart}/templates/deployment.yaml" || err "the Deployment does not set --${fl}"
done

if [[ "${fail}" -ne 0 ]]; then
  exit 1
fi
echo "chart checks passed"
