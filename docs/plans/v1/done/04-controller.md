# 04 — Controller: reconcile, NodePool apply, status, health

Depends on: 03. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §3 (all), §5.1.5, §6.3 failure handling, §7.1.

## Goal

A working controller: an `SCNodePool` produces and maintains exactly one
NodePool, with status, conditions, events and metrics.

## Scope

### `internal/nodepool`

- `Render(scnp, template, selection, adapterRules) (*unstructured.Unstructured, error)`
  per overview §3.4 steps 7–8: copy the template verbatim (unstructured),
  intersect a user `instance-type In` list, add `capacity-type` / `arch` /
  forced zones per adapter rules, never emit forbidden keys, labels
  `app.kubernetes.io/managed-by` + `karpenter.sparecores.com/scnodepool`,
  volatile data only in `metadata.annotations`, default
  `disruption.consolidateAfter: 0s` when missing, ownerReference unless
  `deletionPolicy: Orphan`.
- `Validate(rendered)` per overview §3.6 (≤100 requirements, label-value
  regex ≤63 chars, restricted keys, `minValues`, non-empty `In`), using
  `sigs.k8s.io/karpenter` helpers where they exist.
- Test-mode override (feature F12): controller flag
  `--node-class-ref-override=group/kind/name` replaces `nodeClassRef` in the
  rendered pool while the adapter is still chosen from the template.

### `internal/controller`

- Reconcile flow: resolve adapter → detect regions → fetch (plan 02) →
  evaluate → churn → render → validate → **server-side dry-run** apply → apply
  (field manager `sc-karpenter-nodepool`) → status → Event with the diff
  (added / removed types).
- Safety rails (§3.6): never empty, keep the last applied NodePool on data or
  region errors, `NameConflict`, `NodeClassRefImmutable`,
  `CapacityTypeUnsupported`, `UnsupportedProvider`, `RegionPending`,
  `RegionUnknown`, `UnknownBenchmarkConfig`, `NoCandidates`, `DataStale`.
- Requeue: `refresh.interval` timer; immediate on generation change and on the
  `karpenter.sparecores.com/refresh-requested` annotation changing.
- Watches: `SCNodePool`; owned NodePools (unstructured, `karpenter.sh/v1`);
  NodeClasses of every registered adapter **that is installed** (discover via
  the RESTMapper at start-up and periodically; missing CRDs are fine); Events
  with `involvedObject.kind=NodePool` for our pools.
- Generated-pool health (§3.6): `NodePoolManaged` (no conditions after 2
  min), `NodePoolValid` (mirrors `ValidationSucceeded`), `NodePoolSchedulable`
  (Karpenter `NoCompatibleInstanceTypes` events), drift-loop detector
  (> 3 `RequirementsDrifted` replacements in 10 min → Warning event),
  `RegionConflict`.
- `dryRun: true`: everything except apply; status shows the would-be pool.
- `deletionPolicy`: `Delete` relies on ownerReferences; `Orphan` removes
  ours on deletion via a finalizer.
- Metrics (Prometheus): refresh duration, Keeper requests/errors, selected
  types per SCNodePool, condition states.
- RBAC: SCNodePools (+status, finalizers), NodePools (get/list/watch/create/
  patch/update), NodeClaims and Nodes (get/list/watch), every known NodeClass
  group (get/list/watch), Events (create/patch, list/watch).
- Manager flags: Keeper URL, API key from env/secret file, max concurrent
  reconciles, override flag above, metrics/health addresses.

## Tests

- `internal/nodepool`: golden tests per adapter (rendered YAML under
  `testdata/`), validation negatives.
- envtest (Karpenter core v1.14.1 NodePool/NodeClaim CRDs installed from the
  module cache, plus a minimal fake NodeClass CRD): create → NodePool appears
  with the expected requirements; spec change → update and Event; empty
  selection keeps the previous pool; name conflict; nodeClassRef change
  refused; dryRun; Orphan deletion; region pending/unknown; health conditions
  driven by synthetic NodePool status and Events.

## Gates

CONVENTIONS gates 1–3.
