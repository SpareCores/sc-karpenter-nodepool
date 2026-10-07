# Decisions (ADRs)

Short architecture decision records. Status is one of **accepted**,
**proposed** (waiting for the maintainer), or **superseded**.

## ADR-001: Own NodePools only (accepted, 2026-10-05)

The controller creates and owns its own `karpenter.sh/v1` NodePools. It does
**not** patch user NodePools, does **not** use `NodeOverlay`, does **not** run a
mutating webhook and does **not** ship a custom Karpenter build or provider.

Why:

- It is the only approach that works unchanged on every Karpenter flavour,
  including the managed ones (EKS Auto Mode, AKS NAP) whose controllers and
  feature gates cannot be changed. `NodeOverlay` is alpha and unavailable on
  EKS Auto Mode.
- No fights with GitOps tools over a shared object, and no write access to
  objects the user owns.
- Narrowing `node.kubernetes.io/instance-type` is the one lever that every
  surveyed provider honours (see
  [`../plans/v1/wip/00-overview.md`](../plans/v1/wip/00-overview.md), §5).

Rejected: patching user NodePools (GitOps conflicts, `requirements` is an
atomic list), `NodeOverlay` price encoding (alpha, not on Auto Mode, ignored at
launch by every small-cloud provider), pod-mutating webhooks (in the scheduling
path), a custom cloud provider (not usable with managed Karpenter).

## ADR-002: Spare Cores Keeper API as the data source (accepted, 2026-10-06)

The controller reads Spare Cores data from the public Keeper API
(`https://keeper.sparecores.net`), fetching broad result sets
(`limit=-1`, per vendor and region) and filtering/ranking client-side.

Why: no extra infrastructure, always current data, small footprint. Filters
with an exact Keeper equivalent are pushed down to shrink responses (Keeper
gained the missing upper-bound, ratio and "no GPU" filters for this project in
[sc-keeper#101](https://github.com/SpareCores/sc-keeper/pull/101)); the
controller re-applies every filter client-side, which also covers what Keeper
cannot express (CPU flag any-of/none-of, several benchmarks at once, vCPU
bounds above 256).

Rejected for v1: loading the full SQL dump into the controller (about 1.3 GB
as SQLite). Possible later: a compact per-vendor catalog published by Spare
Cores.

## ADR-003: One `SCNodePool` produces exactly one NodePool (accepted, 2026-10-06)

Kind `SCNodePool`, group `karpenter.sparecores.com`, cluster-scoped (like
`NodePool`). The non-selection parts of the NodePool (nodeClassRef, taints,
labels, disruption, limits, weight) come from an embedded
`spec.nodePoolTemplate`. Users who want tiers create several `SCNodePool`s with
different `weight`s.

Why: one object maps to one object, so ownership, status, drift and deletion
are easy to reason about. Karpenter's own weight ordering already provides
tiering.

## ADR-004: Emit only universally stamped requirement keys (accepted, 2026-10-06)

Generated NodePools constrain only `node.kubernetes.io/instance-type`,
`karpenter.sh/capacity-type` and `kubernetes.io/arch`, plus
`topology.kubernetes.io/zone` (or `topology.k8s.aws/zone-id`) where the user
asks for zones or the provider requires it. Provider family/size/category
labels are never emitted.

Why: Karpenter core treats a NodePool requirement on a *well-known* key that
the provider does not stamp on the NodeClaim as drift (`RequirementsDrifted`),
which produces an endless replace loop (reproduced in kind, see the overview,
§8.2). Provider family labels also do not round-trip with Spare Cores `family`
on most providers.

## ADR-005: Data status filter (accepted, 2026-10-06)

Candidate servers are those with Spare Cores status `ACTIVE` or
`PLANNED_FOR_RETIREMENT` (Keeper's `only_orderable`, the default). INACTIVE
data was verified to be correct (for example Hetzner `cx33` is not orderable).

## ADR-006: Churn control is part of v1 (accepted, 2026-10-06)

Removing a type from a NodePool drifts and replaces every node of that type.
v1 therefore ships hysteresis (removal margin, removal only after N consecutive
refreshes, cap on removals per refresh) and a default refresh interval of 24h.

## ADR-007: Implementation stack — kubebuilder (Go) (accepted, 2026-10-06)

Both can implement the controller. The comparison, specific to this project:

| Aspect | kopf (Python) | kubebuilder / controller-runtime (Go) |
|---|---|---|
| Reuse of Karpenter code | None: NodePool types, restricted-label rules, requirement validation and drift-compatibility checks must be re-implemented and kept in sync by hand | Import `sigs.k8s.io/karpenter/pkg/apis/v1` and `pkg/scheduling` directly: the generator can validate its output with the exact functions Karpenter runs (`IsRestrictedLabel`, label-value validation, `Requirements.Compatible`) |
| Provider test fakes | Not usable | AWS and Azure providers export in-process test environments (`pkg/test`) that run the real provider instance-type filtering and label stamping against our NodePools — the only realistic drift-loop guard without a cloud |
| CRD generation | Hand-written CRD YAML (or generated from pydantic models) | `controller-gen` from Go types + kubebuilder markers: OpenAPI schema, CEL rules, defaults, printer columns, deepcopy |
| Reconcile model | Event handlers (create/update/resume) + per-object timers; state kept in object annotations (`kopf.zalando.org/last-handled-configuration`); multi-replica coordination via kopf "peering" CRDs | Level-triggered reconcile loop with work queue, rate limiting, `Owns(&NodePool{})` watches (re-apply when someone edits the generated pool), built-in leader election, metrics and health probes |
| Server-side apply | Possible via raw `apply-patch+yaml` patches with the `kubernetes` client | First-class (`client.Apply`, field managers) |
| Data processing | Very convenient (Keeper JSON, ranking, family aggregation) | Fine with typed structs; slightly more code |
| Team familiarity | Matches the Spare Cores Python stack (crawler, Keeper, inspector) | Matches the Karpenter/Kubernetes ecosystem and its contributors |
| Footprint | ~150 MB image, ~100 MB RSS | ~40 MB static distroless image, ~30 MB RSS |
| Project health risk | kopf is mostly a single-maintainer project with a slow release cadence | controller-runtime is a Kubernetes SIG project, monthly releases; dependency churn follows Kubernetes minors |
| Local test tooling | `KopfRunner` against a real cluster (kind) | `envtest` (real kube-apiserver + etcd binaries, no cluster) plus kind |
| Prototype speed | Faster for a first demo | Slower start; scaffolding helps |

Version skew applies to both: providers pin Karpenter core v1.2 to v1.14, so
the generator needs an explicit compatibility table (for example `Gte`/`Lte`
only from v1.9; restricted domains differ) whichever language is used.

Decision: **kubebuilder (Go)**. The deciding factors are reusing Karpenter's
own validation code and the AWS/Azure in-process provider fakes for testing.

## ADR-008: Support tiers and test strategy (accepted, 2026-10-07)

- Provider support tiers as proposed in the v1 overview (§5.2): Tier 1 (AWS
  self-managed, EKS Auto Mode) gets real-cloud e2e; Tier 2 (Azure, AKS NAP,
  GCP) and Tier 3 (Alibaba, Hetzner, UpCloud, OVH, Vultr) are covered by the
  local lanes. upcloud-tools, alisonjenkins and PixellUp are unsupported.
- Test layers L1 (unit/golden), L2 (CRD admission matrix) and L3 (KWOK e2e
  with real core Karpenter) run on every PR. L4 (in-process AWS/Azure provider
  tests) is a follow-up; L5 (real AWS e2e) is manual.
- L3 runs against every Karpenter core version the supported providers pin
  (v1.2, v1.8, v1.12, v1.14), not only the latest.
- L3 catalogs are generated in CI from the live Spare Cores data rather than
  pinned fixtures, so data changes surface early; assertions are therefore
  relative to the generated catalogs.
- Real-cloud testing is AWS only for v1 (no Hetzner/UpCloud smoke test yet).
