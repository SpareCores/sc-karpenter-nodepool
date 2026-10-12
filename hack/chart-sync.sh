#!/usr/bin/env bash
# Copies the generated CRD and the manager's RBAC rules into the Helm chart, so
# the chart never drifts from config/ (CI runs it and fails on a diff):
#
#   hack/dev.sh make chart-crds chart-rbac
#
# crds:  config/crd/bases/*.yaml -> charts/sc-karpenter-nodepool/templates/crd.yaml
#        The CRD is not changed except for what Helm needs: the install switch,
#        the chart labels and the "keep on uninstall" annotation.
# rbac:  config/rbac/role.yaml (rules) -> charts/sc-karpenter-nodepool/files/manager-rules.yaml
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${root}/charts/sc-karpenter-nodepool"

crds() {
  local src out
  src="${root}/config/crd/bases/karpenter.sparecores.com_scnodepools.yaml"
  out="${chart}/templates/crd.yaml"
  {
    echo '{{- /* Generated from config/crd/bases by hack/chart-sync.sh (make chart-crds); do not edit. */}}'
    echo '{{- if .Values.crds.install }}'
    awk '
      /^---$/ { next }
      $0 == "  annotations:" && !a {
        print
        print "    {{- if .Values.crds.keep }}"
        print "    helm.sh/resource-policy: keep"
        print "    {{- end }}"
        a = 1
        next
      }
      /^  name: / && !n {
        print "  labels:"
        print "    {{- include \"sckn.labels\" . | nindent 4 }}"
        n = 1
      }
      { print }
    ' "${src}"
    echo '{{- end }}'
  } >"${out}"
}

rbac() {
  yq '.rules' "${root}/config/rbac/role.yaml" >"${chart}/files/manager-rules.yaml"
}

case "${1:-all}" in
  crds) crds ;;
  rbac) rbac ;;
  all) crds; rbac ;;
  *) echo "usage: $0 [crds|rbac|all]" >&2; exit 2 ;;
esac
