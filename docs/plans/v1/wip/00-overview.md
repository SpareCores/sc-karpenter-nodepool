# 00 — v1 overview: features, selectors, provider discovery, testing

Status: **draft for review**. This is the base document every later v1 plan
builds on. It fixes what v1 does (§1–§4), records how every Karpenter provider
we target behaves and how the controller must adapt to it (§5–§6), and how we
test it (§8). Decisions are recorded as ADRs in
[`docs/explanation/DECISIONS.md`](../../../explanation/DECISIONS.md); open
questions are in §10.

Facts about providers are pinned to the commits listed in §5.2 (snapshot
2026-10-05/06). Facts about Spare Cores data come from the 2026-10-05 data dump.

## Table of Contents

- [1. Goal and non-goals](#1-goal-and-non-goals)
- [2. v1 features](#2-v1-features)
- [3. The `SCNodePool` API](#3-the-scnodepool-api)
- [4. Supported Spare Cores selectors](#4-supported-spare-cores-selectors)
- [5. Provider discovery](#5-provider-discovery)
- [6. Provider adapter rules](#6-provider-adapter-rules)
- [7. Keeper API: usage and known gaps](#7-keeper-api-usage-and-known-gaps)
- [8. Testing](#8-testing)
- [9. Plan sequence](#9-plan-sequence)
- [10. Open questions](#10-open-questions)

---

## 1. Goal and non-goals

Karpenter picks instance types from specifications (vCPU, memory, family
labels) and price. It has no notion of measured performance. Spare Cores
measures every instance type of eight clouds with the same benchmarks
(stress-ng, Geekbench, PassMark, memory bandwidth/latency, Redis, nginx,
OpenSSL, compression, PostgreSQL, llama.cpp, GPU bandwidth) and knows hardware
facts Karpenter does not expose (CPU flags, measured memory, GPU details).

`sc-karpenter-nodepool` is a Kubernetes controller that turns a declarative
selection (`SCNodePool`) into a regular Karpenter `NodePool` whose
`node.kubernetes.io/instance-type` requirement lists exactly the instance types
that satisfy the selection, and keeps it up to date as Spare Cores data
changes.

Non-goals (ADR-001): patching user NodePools, `NodeOverlay`, mutating
webhooks, a custom Karpenter or provider build, ordering types inside one
NodePool, workload-usage-driven tiering (a possible v2 "performance class
recommender").

## 2. v1 features

| # | Feature | Notes |
|---|---|---|
| F1 | `SCNodePool` CRD (`karpenter.sparecores.com/v1alpha1`, cluster-scoped) | One `SCNodePool` → one NodePool (ADR-003) |
| F2 | Embedded `nodePoolTemplate` | Full `karpenter.sh/v1` NodePool spec; everything except the instance-type list is copied verbatim |
| F3 | Selector engine | All selectors marked v1 in §4, evaluated client-side over Keeper rows |
| F4 | Family-level ranking with a score band | Keep the families within `ranking.band.withinPercent` of the best, bounded by `ranking.families.min` (capacity floor) and `.max`; every qualifying size of a kept family stays |
| F5 | Provider adapters | AWS, EKS Auto Mode, Azure, AKS NAP, GCP, Alibaba (2), Hetzner (3), UpCloud, OVH, Vultr (§5, §6) |
| F6 | Region resolution | Explicit `spec.location.regions`, with auto-detection fallback (§6.3) |
| F7 | Safe NodePool generation | Universal requirement keys only (ADR-004); server-side dry-run before apply; never an empty list; CRD-version-aware validation |
| F8 | Churn control | Hysteresis, removal cap, 24h default refresh (ADR-006) |
| F9 | Status and observability | Conditions, selected types with scores and prices, exclusion counters, Events with diffs, Prometheus metrics |
| F10 | Generated-pool health | Detect NodePools no Karpenter controller manages (wrong `nodeClassRef`), `ValidationSucceeded=False`, and requirement-drift loops |
| F11 | Dry run | Compute and report in status without touching any NodePool |
| F12 | Test-mode `nodeClassRef` override | Lets the KWOK test lane run the real generator output against core Karpenter (§8.2) |

## 3. The `SCNodePool` API

### 3.1 Example

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: web-fast                     # also the name of the generated NodePool
spec:
  nodePoolTemplate:
    metadata:                        # labels/annotations of the NodePool object
      labels: {team: web}
    spec:                            # karpenter.sh/v1 NodePoolSpec, verbatim
      weight: 50
      template:
        metadata:
          labels: {sparecores.com/tier: fast}
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
          requirements:              # optional user constraints, kept verbatim
            - {key: kubernetes.io/arch, operator: In, values: [amd64, arm64]}
            - {key: karpenter.sh/capacity-type, operator: In, values: [spot, on-demand]}
          expireAfter: 720h
      disruption:
        consolidationPolicy: WhenEmptyOrUnderutilized
        consolidateAfter: 1m
      limits: {cpu: "256"}
  location:
    regions: [us-east-1]             # optional; auto-detected if empty
    zones: []                        # optional; provider zone label values, verbatim
  selector:                          # see §4 and docs/reference/selectors.md
    vcpus: {min: 2, max: 32}
    cpu: {flags: {allOf: [avx2]}}
    workloadProfiles: [{name: web, min: 1.5}]
  ranking:
    score: {workloadProfile: web}
    objective: Score
    band: {withinPercent: 12}
    families: {min: 3, max: 6}
  refresh:
    interval: 24h
  churn:
    removeMarginPercent: 5
    removeAfterRefreshes: 2
    maxRemovalsPerRefresh: 3
  dryRun: false
  deletionPolicy: Delete             # Delete | Orphan (keep the NodePool)
```

### 3.2 Spec fields (non-selector)

| Field | Default | Semantics |
|---|---|---|
| `nodePoolTemplate.metadata` | — | Labels and annotations of the generated NodePool object (not of the nodes). |
| `nodePoolTemplate.spec` | required | A `karpenter.sh/v1` `NodePoolSpec`. `template.spec.nodeClassRef` is required and selects the provider adapter (§6.1). User requirements are kept verbatim; a user `node.kubernetes.io/instance-type In` list is intersected with the selection. `kubernetes.io/arch` and `karpenter.sh/capacity-type` requirements constrain the selection (§3.4). |
| `location.regions` | auto-detect | Kubernetes region label values (`us-east-1`, `westeurope`, `us-central1`, `fsn1`, `GRA11`, `ewr`, …); mapped to Spare Cores region ids by the adapter (§6.3). |
| `location.zones` | none | Zone label values as the provider writes them; emitted verbatim as a zone requirement (§6.4). |
| `selector`, `ranking` | — | §4 and [`docs/reference/selectors.md`](../../../reference/selectors.md). |
| `refresh.interval` | `24h` | Re-evaluation period. Spec changes (new `metadata.generation`) re-evaluate immediately; so does the annotation `karpenter.sparecores.com/refresh-requested: <any new value>`. |
| `churn.*` | `5`, `2`, `3` | §3.5. |
| `dryRun` | `false` | Compute and report in `status`; never create or update the NodePool. |
| `deletionPolicy` | `Delete` | `Delete`: the NodePool is owned (ownerReference) and garbage-collected with the `SCNodePool` — Karpenter then drains its nodes. `Orphan`: remove the ownerReference and keep the NodePool. |

Controller-level settings (flags / Helm values, not per object): Keeper URL,
optional Keeper API key Secret, HTTP timeouts, max concurrent refreshes, the
test-mode `nodeClassRef` override (F12).

### 3.3 Status

```yaml
status:
  observedGeneration: 3
  provider: aws                       # adapter id (§6.1)
  regions: [us-east-1]                # resolved Kubernetes region values
  nodePoolName: web-fast
  dataTimestamp: "2026-10-05T10:02:57Z"   # Keeper data freshness
  lastRefreshTime: "2026-10-06T08:00:00Z"
  conditions:
    - type: Ready                     # summary
    - type: DataAvailable             # Keeper reachable, benchmarks valid
    - type: SelectionNonEmpty
    - type: NodePoolSynced            # applied (or skipped in dryRun)
    - type: NodePoolManaged           # a Karpenter controller set conditions on it
    - type: NodePoolValid             # mirrors the NodePool's ValidationSucceeded
    - type: NodePoolSchedulable       # False on Karpenter NoCompatibleInstanceTypes events
  selected:
    count: 42
    band: {reference: c7a, referenceValue: 2.4, lowestKept: m7a, lowestKeptDeltaPercent: 10.8, widened: false, trimmed: false}
    families: [{name: c7a, rank: 1, objective: 2.4, sizes: 9}, ...]
    instanceTypes: [{name: c7a.large, family: c7a, score: 2.1, price: 0.1027}, ...]
  excluded:                           # counts per pipeline step
    providerIneligible: 294
    templateConstraints: 12
    filters: {vcpus: 300, workloadProfiles: 211}
    missingData: 74
    ranking: 120
  pendingRemovals: [{name: m5.large, since: "2026-10-05T08:00:00Z", refreshes: 1}]
```

Printer columns: NodePool, Provider, Types, Ready, Last refresh.

### 3.4 Generation pipeline and the emitted NodePool

1. Resolve the adapter from `nodeClassRef.group` (and the NodeClass CRD version
   where two providers share a group; §6.1).
2. Resolve regions (§6.3) and map them to Spare Cores `vendor~region_id`.
3. Fetch Keeper rows (§7.1) and apply the adapter's eligibility filter (§6.2).
4. Apply template constraints: `kubernetes.io/arch` restricts candidates;
   `karpenter.sh/capacity-type` picks the default price basis.
5. Apply selectors, the data-quality policy and ranking
   ([`selectors.md`](../../../reference/selectors.md)).
6. Apply churn control (§3.5).
7. Build requirements (ADR-004):
   - `node.kubernetes.io/instance-type In [<api_reference>...]`, sorted, after
     intersecting with a user-provided list;
   - `karpenter.sh/capacity-type`: the user's requirement if present, otherwise
     the adapter default (always explicit; several providers loop or fail
     without it, §5);
   - `kubernetes.io/arch`: the user's requirement if present, otherwise
     `In [<architectures of the selected types>]` — except on EKS Auto Mode
     and providers where §6 says otherwise;
   - zone: `spec.location.zones` if set, otherwise only where the adapter
     forces it (OVH);
   - every other user requirement verbatim.
8. Validate (§3.6), then server-side apply with field manager
   `sc-karpenter-nodepool`, an ownerReference to the `SCNodePool`
   (`deletionPolicy: Delete`), and labels
   `app.kubernetes.io/managed-by: sc-karpenter-nodepool`,
   `karpenter.sparecores.com/scnodepool: <name>`.
   Volatile data (data timestamp, selection hash) goes into the NodePool's
   `metadata.annotations`, never into `spec.template` (§5.1.5).
9. Update status; emit an Event with the added/removed types.

Example output for §3.1:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: web-fast
  labels:
    team: web
    app.kubernetes.io/managed-by: sc-karpenter-nodepool
    karpenter.sparecores.com/scnodepool: web-fast
  annotations:
    karpenter.sparecores.com/data-timestamp: "2026-10-05T10:02:57Z"
  ownerReferences: [{apiVersion: karpenter.sparecores.com/v1alpha1, kind: SCNodePool, name: web-fast, controller: true, blockOwnerDeletion: true}]
spec:
  weight: 50
  template:
    metadata: {labels: {sparecores.com/tier: fast}}
    spec:
      nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
      expireAfter: 720h
      requirements:
        - {key: kubernetes.io/arch, operator: In, values: [amd64, arm64]}
        - {key: karpenter.sh/capacity-type, operator: In, values: [spot, on-demand]}
        - {key: node.kubernetes.io/instance-type, operator: In, values: [c7a.large, c7a.xlarge, ...]}
  disruption: {consolidationPolicy: WhenEmptyOrUnderutilized, consolidateAfter: 1m}
  limits: {cpu: "256"}
```

### 3.5 Churn control

Adding a type to the list never disrupts anything; removing one drifts every
running node of that type (§5.1.5). On each refresh:

- additions are applied immediately;
- a type is removed only if it fails the selection by more than
  `removeMarginPercent` on the failing threshold (or is no longer offered at
  all) for `removeAfterRefreshes` consecutive refreshes;
- at most `maxRemovalsPerRefresh` types are removed per refresh (types no
  longer offered by the provider are exempt);
- spec changes bypass hysteresis (the user asked for the new selection), but
  not the removal cap.

### 3.6 Safety rails

- Never apply an empty `instance-type` list (an `In` with no values is
  rejected by the NodePool CRD anyway). If the selection is empty, Keeper is
  unreachable, or a benchmark/config is unknown, keep the last applied
  NodePool and set `Ready=False` with a reason (`NoCandidates`, `DataStale`,
  `UnknownBenchmarkConfig`). On the very first evaluation there is no previous
  NodePool, so none is created. The same applies when the selection does not
  intersect a user-provided `instance-type` list in the template.
- A non-empty list can still be unusable at runtime: names the provider does not
  offer in the cluster's region or for the NodeClass (image arch, quota, a
  provider filter we do not mirror). Karpenter then silently skips the NodePool
  for scheduling: its `Ready` condition stays `True`, pods that require it stay
  Pending, and the only signal is a Warning Event on the NodePool with reason
  `NoCompatibleInstanceTypes` ("NodePool requirements filtered out all
  compatible available instance types"), emitted on every scheduling attempt
  (deduplicated per minute) by every core version we target
  ([scheduler.go](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/pkg/controllers/provisioning/scheduling/scheduler.go#L179-L192)).
  The controller watches Events on its NodePools and sets
  `NodePoolSchedulable=False` (with the event message) while they keep coming,
  and back to `True` once a NodeClaim of the pool launches. The provider
  eligibility filters (§6.2) keep this rare; it is reported, not auto-fixed,
  since the controller cannot see the provider's instance-type list.
- Running out of capacity is different: if every allowed type is temporarily
  unavailable (ICE), launches fail and Karpenter retries; the NodePool stays
  schedulable. Narrow lists make this likelier, which is one more reason to keep
  every size of a selected family (§4, ranking).
- Refuse to adopt an existing NodePool of the same name that is not labelled
  as ours (`reason=NameConflict`).
- Refuse to change `nodeClassRef.group` or `kind` on an existing NodePool
  (immutable in the CRD); report `reason=NodeClassRefImmutable`.
- Validate before applying: at most 100 requirements; every value matches
  `^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$` and is at most 63 characters;
  no `karpenter.sh/*` key other than `capacity-type`; no
  `kubernetes.io/hostname`; `minValues ≤ len(values)`; operators supported by
  the provider's core version (§5.1.3). Then a server-side dry-run apply, which
  enforces the provider's own CRD CEL rules.
- Default `disruption.consolidateAfter` to `0s` if missing (required by the
  v1.14.1 NodePool CRD every current provider ships).
- Watch the generated NodePool: no status conditions after 2 minutes means no
  Karpenter controller manages its `nodeClassRef` (a wrong group/kind is
  silently ignored by Karpenter) → `NodePoolManaged=False`.
  `ValidationSucceeded=False` → `NodePoolValid=False`. More than 3
  `RequirementsDrifted` replacements of the same NodeClaim lineage in 10
  minutes → Warning Event (drift-loop detector, §5.1.5).

## 4. Supported Spare Cores selectors

All v1 selectors, as the user writes them under `spec.selector` /
`spec.ranking`. Every selector is optional; set selectors are AND-ed. Full
semantics, coverage and caveats:
[`docs/reference/selectors.md`](../../../reference/selectors.md).

| Category | Field | Spare Cores data | Example | Meaning |
|---|---|---|---|---|
| Scope | `instanceTypes.include` / `.exclude` | `api_reference` | `exclude: ["*.metal*"]` | Glob allow/deny on instance-type names |
| Scope | `families.include` / `.exclude` | `family` | `include: ["c7*", "m7*"]` | Glob allow/deny on SC family |
| CPU | `vcpus.min` / `.max` | `vcpus` | `{min: 2, max: 32}` | vCPU bounds |
| CPU | `architectures` | `cpu_architecture` | `[arm64]` | `amd64` / `arm64` |
| CPU | `cpu.manufacturers` | `cpu_manufacturer` | `[AMD, AWS]` | CPU vendor |
| CPU | `cpu.allocations` | `cpu_allocation` | `[Dedicated]` | `Shared` / `Burstable` / `Dedicated` |
| CPU features | `cpu.flags.allOf` / `anyOf` / `noneOf` | `cpu_flags` | `allOf: [avx512f, amx_tile]` | Instruction-set requirements |
| CPU caches | `cpu.caches.{l1d,l1i,l2}PerCoreMinKiB`, `cpu.caches.l3MinMiB`, `cpu.caches.l3TotalMinMiB` | `cpu_l1d_cache`, `cpu_l1i_cache`, `cpu_l2_cache`, `cpu_l3_cache`, `cpu_l3_cache_total` | `{l2PerCoreMinKiB: 2048, l3MinMiB: 32}` | Cache sizes as seen by the VM (L3 is shared) |
| Memory | `memory.minGiB` / `.maxGiB` | `memory_amount` | `{minGiB: 8}` | Memory bounds |
| Memory | `memory.perVcpu.minGiB` / `.maxGiB` | `memory_amount / vcpus` | `{minGiB: 4}` | Memory-to-CPU ratio |
| GPU | `gpu.count.min` / `.max` | `gpu_count` | `{max: 0}` | GPU count; `max: 0` = CPU-only |
| GPU | `gpu.memoryPerGpu.minGiB` | `gpu_memory_min` | `{minGiB: 40}` | Memory of the smallest GPU |
| GPU | `gpu.memoryTotal.minGiB` | `gpu_memory_total` | `{minGiB: 80}` | Sum of GPU memory |
| GPU | `gpu.manufacturers`, `gpu.models` | `gpu_manufacturer`, `gpu_model` | `models: [L40S, H100]` | Exact SC values |
| Price | `price.basis` | `server_price.allocation` | `OnDemand` | Which price filters/ranking use |
| Price | `price.maxHourly` | region-scoped `min_price` (USD) | `"0.50"` | Upper bound per hour (also caps node size) |
| Price | `price.maxHourlyPerVcpu` | price / `vcpus` | `"0.05"` | Upper bound per vCPU-hour |
| Price | `price.includeManagedFee` | AWS price list (EKS Auto Mode fee) | `true` | Add the Auto Mode fee (Auto Mode only) |
| Benchmarks | `benchmarks[]: {id, config, min, max, normalize}` | `benchmark_score` | `{id: geekbench:score, config: {cores: single}, min: 2000}` | Measured-performance thresholds; `normalize: None / PerVcpu / PerPrice` |
| Composite | `workloadProfiles[]: {name, min, normalize}` | `workload_profile:*` | `{name: data_analysis, min: 1.2}` | `web`, `compute`, `cache`, `data_analysis`, `llm`, `cicd` |
| Ranking | `ranking: {score, objective, normalize, band, families}` | any benchmark or profile | `{objective: Score, band: {withinPercent: 12}, families: {min: 3, max: 6}}` | Families within 12% of the best, at least 3, at most 6; all sizes kept |
| Data quality | `unbenchmarked` | — | `IncludeIfFamilyQualifies` | `Exclude` (default) / `IncludeIfFamilyQualifies` / `Include` |

Not in v1: CPU family/model regex, hyperthreading, clock speed,
nested virtualisation, memory generation/speed/ECC, local storage, network
bandwidth, price per GiB, thresholds relative to a named instance type,
boot time.

## 5. Provider discovery

### 5.1 Rules shared by every provider (Karpenter core)

Permalinks: core `kubernetes-sigs/karpenter` at
[`9d3669a`](https://github.com/kubernetes-sigs/karpenter/tree/9d3669a86c2e462b37695ddd8fda4d1419092de2)
(newer than v1.14.1) unless noted.

#### 5.1.1 Labels and keys

- Well-known keys every provider uses: `node.kubernetes.io/instance-type`,
  `kubernetes.io/arch` (`amd64`, `arm64`), `kubernetes.io/os`,
  `topology.kubernetes.io/zone`, `topology.kubernetes.io/region`,
  `karpenter.sh/capacity-type` (`on-demand`, `spot`, `reserved`),
  `karpenter.sh/nodepool`
  ([labels.go](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/pkg/apis/v1/labels.go#L205-L213)).
- Forbidden in NodePool requirements: the whole `karpenter.sh` domain except
  `karpenter.sh/capacity-type`, and `kubernetes.io/hostname`
  ([CRD CEL](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/pkg/apis/crds/karpenter.sh_nodepools.yaml#L283-L345)).
- Each provider registers its own well-known keys and may restrict its own
  domain to an allowlist (AWS and Azure via CEL in the NodePool CRD they ship;
  GCP only at runtime).
- A key that is not well-known (for example `sparecores.com/tier`) is a
  *custom* label: it does not filter instance types; core stamps it on the
  node.
- Core stamps `<nodeClassRef.group>/<lowercase kind>` on NodeClaims (for
  example `karpenter.k8s.aws/ec2nodeclass`), which identifies the provider on
  existing nodes.

#### 5.1.2 NodePool CRD limits

| Limit | Value |
|---|---|
| Requirements per NodePool | max 100 |
| Values per requirement | no maximum; each value ≤ 63 characters, label-value regex — validated at runtime only, **no CRD rejects bad values** (verified on 12 provider CRDs) |
| `In` | at least one value |
| `minValues` | 1–50, ≤ number of values |
| `weight` | 1–100 (unset = 0); higher first, ties by name descending ([nodepool.go](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/pkg/utils/nodepool/nodepool.go#L161-L170)) |
| `nodeClassRef.group` / `kind` | immutable |
| `disruption.consolidateAfter` | **required** in the v1.14.1 CRD shipped by every current provider (defaulted only on core HEAD) |
| Budget reason `Unhealthy` | core HEAD only; do not use |
| Object size | etcd ~1.5 MiB; the largest possible list (all ~1,400 AWS types) is ~30 KB |

Runtime validation failures set `ValidationSucceeded=False` on the NodePool
and Karpenter ignores it; the API server does not reject the object.

#### 5.1.3 Core version differences

Providers pin very different core versions (§5.2). Differences that matter:

| | Core ≤ v1.8 | Core ≥ v1.9 |
|---|---|---|
| Restricted label domains | `kubernetes.io`, `k8s.io`, `karpenter.sh` (exceptions: `node.kubernetes.io`, `node-restriction.kubernetes.io`) | only `karpenter.sh` |
| `Gte` / `Lte` operators | not available | available |
| NodeClaim instance-type truncation | v1.2: 60 cheapest; v1.7–v1.8: 600 | 600 cheapest ([nodeclaimtemplate.go](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/pkg/controllers/provisioning/scheduling/nodeclaimtemplate.go#L48-L50)) |
| CRD CEL (oldest CRDs: cloudpilot Alibaba v1.2, CAPI) | hostname and `karpenter.sh/*` keys not rejected | rejected |

The generator only emits `In` and universally known keys, so the practical
impact is limited to validation.

#### 5.1.4 How the launched type is chosen

- Core orders compatible types by the provider's price, truncates (600), and
  writes them as an `In` requirement on the NodeClaim. The set is serialised
  **sorted alphabetically**, so price order is lost
  ([requirement.go](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/pkg/scheduling/requirement.go#L160-L168)).
- The provider's `Create()` then picks: most re-rank by their own price; some
  take `Values[0]` (alphabetical) — see §5.3.
- Consequence: **narrowing the set is the only portable lever.** Preference
  between types is expressible only with several NodePools and `weight`.
- Spot-to-spot consolidation needs at least 15 cheaper candidate types; AWS
  warns below 5 types when flexible to spot. Do not generate very narrow lists
  unless asked to.

#### 5.1.5 Drift

- **Static drift (`NodePoolDrifted`)**: a hash of `spec.template` *minus
  requirements* (labels, annotations, taints, startupTaints, expireAfter,
  terminationGracePeriod, nodeClassRef). Any change replaces every node in the
  pool. Never put volatile data into `spec.template`.
- **`RequirementsDrifted`**: nodes whose labels no longer satisfy the
  requirements. Removing a type drifts that type's nodes; adding drifts
  nothing.
- **Drift loop (reproduced in kind):** drift is checked as
  `nodeClaimLabels.Compatible(nodePoolRequirements)` *without* allowing
  undefined keys
  ([drift.go](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/pkg/controllers/nodeclaim/disruption/drift.go#L172-L182)),
  while core stamps only *custom* keys itself and leaves well-known keys to the
  provider
  ([nodeclaimtemplate.go](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/pkg/controllers/provisioning/scheduling/nodeclaimtemplate.go#L96-L106)).
  A multi-valued `In` on a well-known key the provider does not stamp on the
  NodeClaim therefore drifts every new node immediately: a new NodeClaim
  every ~21 s, forever. Confirmed on OVH by source review (region and
  `karpenter.ovhcloud.sh/*` keys). This is why ADR-004 limits the keys.
- Deleting or renaming a NodePool drains its nodes (NodeClaims are owned by it).
  Generated names are stable (= `SCNodePool` name).

#### 5.1.6 `nodeClassRef` matching

Each Karpenter controller only manages NodePools whose `nodeClassRef`
group/kind matches its own NodeClass
([nodepool.go](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/pkg/utils/nodepool/nodepool.go#L37-L41)).
A NodePool with another group/kind gets no status at all and its pods stay
Pending (verified). Several providers resolve the NodeClass by name only.

### 5.2 Providers in scope

Spare Cores vendors with data: aws, azure, gcp, alicloud, hcloud, ovh,
upcloud, vultr. Every Karpenter provider for them, as of 2026-10-05:

| Vendor | Provider (repository @ commit) | Release / core | Cluster type | Maturity | v1 support |
|---|---|---|---|---|---|
| aws | [aws/karpenter-provider-aws @ `30c236ca`](https://github.com/aws/karpenter-provider-aws/tree/30c236cafacb3a23fb3a50370d09d71750b56b29) | v1.14.1 (LTS) / core v1.14.1+ | EKS, self-managed | GA | **Tier 1** |
| aws | EKS Auto Mode (managed, closed source) | managed / NodePool CRD equivalent to core v1.14 (§5.4) | EKS | GA | **Tier 1** |
| azure | [Azure/karpenter-provider-azure @ `8878fa43`](https://github.com/Azure/karpenter-provider-azure/tree/8878fa43373fbad8974b432fc63fa9b3af921604) | v1.14.3 / core v1.14.1 | AKS self-hosted | GA | Tier 2 |
| azure | AKS Node Auto Provisioning (managed, same code) | lags OSS | AKS | GA | Tier 2 |
| gcp | [cloudpilot-ai/karpenter-provider-gcp @ `8b30c3aa`](https://github.com/cloudpilot-ai/karpenter-provider-gcp/tree/8b30c3aa03080804b2b9cd199dff656aca7f08b4) | v0.6.1 / core v1.14.1 | GKE Standard | beta | Tier 2 |
| alicloud | [AliyunContainerService/karpenter-provider-alibabacloud @ `67a27fd1`](https://github.com/AliyunContainerService/karpenter-provider-alibabacloud/tree/67a27fd141a777527ac8ce0c66f1e98ab12b04d3) (official ACK) | v0.2.5 / core v1.8.0 | ACK, self-managed | alpha | Tier 3 |
| alicloud | [cloudpilot-ai/karpenter-provider-alibabacloud @ `c215c632`](https://github.com/cloudpilot-ai/karpenter-provider-alibabacloud/tree/c215c63258d6ee40310bae495047563837e9caab) | v0.2.2 / core v1.2.0 | ACK, self-managed | usable, maintenance mode | Tier 3 |
| hcloud | [stubbi/karpenter-provider-hetzner @ `a20777ac`](https://github.com/stubbi/karpenter-provider-hetzner/tree/a20777acb552216996db0fe6ccd1e7216aa59b2c) (= paperclipinc) | v3.0.1 / core v1.14.1 | self-managed (Talos, kubeadm, k3s) | usable | Tier 3 |
| hcloud | [itsh-cloud/karpenter-provider-hcloud @ `579b057f`](https://github.com/itsh-cloud/karpenter-provider-hcloud/tree/579b057fa159821703f1d60f52cbd4643011fb3a) | v0.2.0 / core v1.14.0 | self-managed kubeadm (Debian), x86 only | alpha | Tier 3 |
| hcloud | [echohello-dev/karpenter-provider-hetzner @ `2676f8f0`](https://github.com/echohello-dev/karpenter-provider-hetzner/tree/2676f8f0cdadaca2b0812a9f03b9bf7df9517b06) | v2026.9.2 / core v1.14.1 | self-managed (Talos, Ubuntu) | alpha | Tier 3 |
| upcloud | [kubekanvas/karpenter-provider-upcloud @ `18fbce1e`](https://github.com/kubekanvas/karpenter-provider-upcloud/tree/18fbce1e6649a4849bfc7db836eba561609cbc37) | v0.1.1 / core v1.14.0 | self-managed on UpCloud servers | alpha | Tier 3 |
| ovh | [antonin-a/karpenter-provider-ovhcloud @ `216d20a8`](https://github.com/antonin-a/karpenter-provider-ovhcloud/tree/216d20a8e76179c4252e72cf62245e01165214cf) | v0.2.2 / core v1.8.2 | OVH MKS only | alpha | Tier 3 |
| vultr | [crob1140/karpenter-provider-vultr @ `cfa9da51`](https://github.com/crob1140/karpenter-provider-vultr/tree/cfa9da51c7433e52836bebbfe457a89b69a6fc7c) | no release / core v1.12.1 | self-managed kubeadm | alpha | Tier 3 |
| upcloud | [upcloud-tools/karpenter-provider-upcloud @ `332fc7c6`](https://github.com/upcloud-tools/karpenter-provider-upcloud/tree/332fc7c6f9ceed4ae768ef054377a1245d57e2d6) | v1.1.0 / **forked core** | UKS | beta | **Unsupported** |
| hcloud | [alisonjenkins/karpenter-provider-hetzner @ `66802e53`](https://github.com/alisonjenkins/karpenter-provider-hetzner/tree/66802e53651d89254bd7a8f72bd5a6c948bb1b8f) | none / core v1.11.1 | k3s | prototype | **Unsupported** |
| hcloud | [PixellUp/karpenter-provider-hetzner @ `f49fee2d`](https://github.com/PixellUp/karpenter-provider-hetzner/tree/f49fee2d7b647b7da792251244f44adc78224809) | stale fork of stubbi | — | stale | **Unsupported** |

Support tiers (accepted, ADR-008): Tier 1 = adapter + KWOK + real-cloud e2e;
Tier 2 = adapter + KWOK + CRD matrix (+ in-process provider tests for Azure);
Tier 3 = adapter + KWOK + CRD matrix, best effort.

Why the unsupported ones are out:

- **upcloud-tools**: built on a fork of core (upstream main snapshot plus an
  unmerged provisioning patch), takes the alphabetically first type of the
  NodeClaim (`req.Values[0]`), single zone only, EUPL-1.2.
- **alisonjenkins**: `Values[0]` pick, zero prices, no labels on NodeClaims,
  ignores the chosen zone, drift is a no-op in production.
- **PixellUp**: 36 commits behind stubbi, no own work.

No provider exists for any other combination (searched GitHub repos and code
for each vendor; the only new one since 2026-09 is crob1140's Vultr provider).

### 5.3 Summary matrix

| Provider | `nodeClassRef` group / kind | Instance-type value = SC | Region label → SC | Zone label value | Capacity types | Arch | `Create()` picks | Prices |
|---|---|---|---|---|---|---|---|---|
| AWS | `karpenter.k8s.aws` / `EC2NodeClass` | `api_reference` | `region_id` | account-specific name `us-east-1a`; stable `topology.k8s.aws/zone-id` = SC `zone_id` | on-demand, spot, reserved | amd64, arm64 | CreateFleet over ≤60 cheapest | live API + embedded |
| EKS Auto Mode | `eks.amazonaws.com` / `NodeClass` | `api_reference` | `region_id` | name `us-east-1a`; stable `topology.k8s.aws/zone-id` = SC `zone_id` | on-demand, spot, reserved | amd64, arm64 | managed (CreateFleet) | EC2 + fee |
| Azure / NAP | `karpenter.azure.com` / `AKSNodeClass` | `api_reference` (case-sensitive) | `api_reference` (`westeurope`) | `<region>-<n>` or `0` (regional) | on-demand, spot | amd64, arm64 | single cheapest offering, ICE → next reconcile | Retail Prices API |
| GCP | `karpenter.k8s.gcp` / `GCENodeClass` | `api_reference` (**not** numeric `server_id`) | `api_reference` (`us-central1`; `region_id` is numeric) | `us-central1-a` | on-demand, spot | amd64, arm64 | sequential tries in price order | third-party CSV |
| Alibaba (ACS) | `karpenter.alibabacloud.com` / `ECSNodeClass` | `api_reference` (`ecs.g7.large`) | `region_id` | `cn-hangzhou-k` = SC `zone_id` | on-demand, spot (one per pool!) | amd64, arm64 | API order, `RunInstances` per type | **none (0.0)** |
| Alibaba (cloudpilot) | `karpenter.k8s.alibabacloud` / `ECSNodeClass` | `api_reference` | `region_id` | SC `zone_id` | on-demand, spot | amd64, arm64 | APG `lowest-price` over 20 cheapest | live (CNY) + embedded |
| Hetzner (stubbi) | `karpenter.hetzner.cloud` / `HCloudNodeClass` | `api_reference` (`ccx33`; **not** numeric `server_id`) | none | location `fsn1` = SC region `aliases[0]` | on-demand only | amd64, arm64 | cheapest offering | live, per location |
| Hetzner (echohello) | `karpenter.hetzner.cloud` / `HCloudNodeClass` | `api_reference` | none | location `fsn1` | on-demand only | amd64, arm64 | cheapest, 1 try | live, location-uniform |
| Hetzner (itsh) | `karpenter.itsh.dev` / `HCloudNodeClass` | `api_reference` (x86, non-deprecated) | location `fsn1` = `aliases[0]` | datacenter `fsn1-dc14` = SC region `api_reference` | on-demand only | amd64 only | cheapest, up to 8 fallbacks | live, per location |
| UpCloud (kubekanvas) | `karpenter.k8s.upcloud` / `UpCloudNodeClass` (v1alpha1) | `api_reference` (mixed case `CLOUDNATIVE-2xCPU-4GB`) | = zone (`de-fra1`) | `de-fra1` = SC `zone_id` | on-demand (spot only for `GPU-SPOT-*`) | amd64 | cheapest offering | live, per zone |
| OVH | `karpenter.ovhcloud.sh` / `OVHNodeClass` | `api_reference` (`b3-8`) | not set by provider (CCM sets uppercase `GRA11`) | `lower(region)-a/b/c` = SC `zone_id` | on-demand | amd64 | cheapest by fake price | **formula** |
| Vultr | `karpenter.vultr.com` / `VultrNodeClass` | `api_reference` (`vc2-2c-4gb`) | `region_id` (`ewr`) | = region (`ewr`) | on-demand | amd64 | cheapest by monthly cost | live, catalog-wide |

Every provider honours a narrowed `node.kubernetes.io/instance-type` list.
Spare Cores `api_reference` values have no duplicates per vendor (also
case-insensitively), so the mapping is always exact.

### 5.4 Per-provider details

#### AWS (self-managed Karpenter)

- NodeClass `karpenter.k8s.aws/v1` `EC2NodeClass` (required:
  `amiSelectorTerms`, `subnetSelectorTerms`, `securityGroupSelectorTerms`,
  `role` or `instanceProfile`). `status.subnets[]` has `zone` and `zoneID`.
- Provider labels (`karpenter.k8s.aws/instance-{family,size,category,generation,cpu,memory,cpu-manufacturer,hypervisor,gpu-*,accelerator-*,...}`,
  `topology.k8s.aws/zone-id`): registered as well-known
  ([labels.go](https://github.com/aws/karpenter-provider-aws/blob/30c236cafacb3a23fb3a50370d09d71750b56b29/pkg/apis/v1/labels.go#L31-L74));
  the AWS-shipped NodePool CRD rejects other `karpenter.k8s.aws/*` keys
  ([CRD](https://github.com/aws/karpenter-provider-aws/blob/30c236cafacb3a23fb3a50370d09d71750b56b29/pkg/apis/crds/karpenter.sh_nodepools.yaml#L230)).
  `instance-family` equals SC `family` for all 1,402 types (not emitted, ADR-004).
- Instance types: live `DescribeInstanceTypes` (hvm, x86_64/arm64). Versus SC
  ACTIVE: SC lacks the Graviton `*g-flex` families (an SC data gap: those types
  can never be selected); SC has types newer than the provider's embedded
  snapshot (fine, discovered at runtime).
- Zones: names (`us-east-1a`) map to different AZs per account; SC `zone_id`
  is the AZ ID (`use1-az2`). Users should give zone names of their own account
  in `spec.location.zones`, or AZ IDs via a `topology.k8s.aws/zone-id`
  requirement in the template (stamped by the provider; EKS Auto Mode too).
- Launch: core 600 → provider exotic filter (drops `metal` and GPU/accelerator
  types unless nothing else is left or `minValues` is set), spot-offering filter
  (drops spot offerings pricier than the cheapest on-demand when both are
  allowed), NodeClass compatibility (for example `a1.*` with AL2023), then one
  `CreateFleet` with the 60 cheapest
  ([instance.go](https://github.com/aws/karpenter-provider-aws/blob/30c236cafacb3a23fb3a50370d09d71750b56b29/pkg/providers/instance/instance.go#L319-L347)).
  Offerings without a price are unavailable.
- Adapter: vendor `aws`; drop SC `*_mac` and `i386`; default capacity type
  `on-demand`.

#### EKS Auto Mode

- NodePools reference `eks.amazonaws.com/v1` `NodeClass` only (never an
  `EC2NodeClass`). The built-in `system` and `general-purpose` pools cannot be
  modified; the `default` NodeClass exists only while a built-in pool is
  enabled — do not name a custom NodeClass `default`. Custom NodeClasses need
  an EKS access entry for their node role.
- Karpenter version, as observed on a live Auto Mode cluster (Kubernetes 1.36,
  EKS platform `eks.11`, 2026-10-07; the version itself is not exposed, as
  Karpenter runs in the managed control plane): the installed
  `nodepools.karpenter.sh` / `nodeclaims.karpenter.sh` CRDs are generated with
  controller-gen v0.20.1 and match core v1.14's NodePool schema —
  `consolidateAfter` required, `Gte`/`Lte` allowed, `Balanced` consolidation
  policy — plus one AWS-only disruption-budget reason, `NodeNotHealthy` (core
  HEAD calls it `Unhealthy`; avoid both). No `NodeOverlay` CRD is installed.
  NodePool status conditions: `NodeClassReady`, `NodeRegistrationHealthy`,
  `Ready`, `ValidationSucceeded`.
- Labels use the `eks.amazonaws.com/` prefix instead of `karpenter.k8s.aws/`.
  The Auto Mode NodePool CRD restricts the `karpenter.k8s.aws`,
  `eks.amazonaws.com` and `sagemaker.amazonaws.com` domains to allowlists (for
  example `eks.amazonaws.com/instance-family` and, still,
  `karpenter.k8s.aws/instance-family` pass; `eks.amazonaws.com/foo` is
  rejected at admission).
- Requirement keys checked with server-side dry-runs (nothing created):
  `topology.k8s.aws/zone-id` and `kubernetes.io/os` are admitted. Auto Mode
  NodeClaims carry both labels (as well as `topology.kubernetes.io/zone`,
  `topology.kubernetes.io/region` and the `eks.amazonaws.com/instance-*`
  family), so neither can cause the drift loop. AWS documents
  `kubernetes.io/os` as unsupported; whether the runtime validation
  (`ValidationSucceeded`) rejects it is unverified — we never emit it anyway.
  Zone pinning by AZ ID (`topology.k8s.aws/zone-id`, account-independent, = SC
  `zone_id`) is therefore available on Auto Mode too.
- Eligible instance types: the AWS public price list publishes one
  `AmazonEKS` product per Auto Mode-eligible instance type and region
  (`eksproducttype: AutoMode`,
  `https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEKS/current/<region>/index.json`,
  ~1.4 MB per region; 1,081 types in us-east-2). This list is authoritative
  per region and also gives the management fee. It matches the documented
  family allowlist plus the "more than 1 vCPU, not nano/micro/small" rule
  ([docs](https://docs.aws.amazon.com/eks/latest/userguide/automode-learn-instances.html)).
  The adapter must drop every type not on the list, otherwise the pool can end
  up with nothing launchable.
- Management fee: per instance-hour, median 12% of the on-demand price (4.8%
  for some types), charged regardless of capacity type, so spot savings shrink.
  Added to prices when `price.includeManagedFee` is on (default on Auto Mode).
- Defaults Auto Mode applies: `expireAfter` 336h, disruption budget 10%,
  weekly AMI drift, NodeClaim `terminationGracePeriod` 24h. Our drift adds to
  that churn — another reason for §3.5.
- Adapter: vendor `aws`; eligibility from the price list (cached per refresh);
  no `kubernetes.io/os`; zones by name or AZ ID; default capacity type
  `on-demand`.

#### Azure (self-hosted) and AKS Node Auto Provisioning

- NodeClass `karpenter.azure.com/v1beta1` `AKSNodeClass` (no required fields).
  Self-hosted and NAP cannot be told apart by group; NAP clusters have no
  Karpenter Deployment and come with `default` / `system-surge` NodePools.
  Never run both in one cluster.
- Instance type = `Standard_*` name, case-sensitive, identical to SC
  `api_reference` (1,272 of 1,305 SC types are in the provider's
  `known_skus.yaml`; the 33 SC-only newest SKUs get the provider's fallback
  price 999 and are effectively last choice).
- Zones: `<lowercase region>-<n>`, or `0` for regional (non-zonal) placement;
  zonal SKUs also get a `0` offering. SC `zone_id` is `1`/`2`/`3` or `0`.
- Capacity types: `on-demand`, `spot` only — `reserved` makes the NodePool
  invalid
  ([labels.go](https://github.com/Azure/karpenter-provider-azure/blob/8878fa43373fbad8974b432fc63fa9b3af921604/pkg/apis/v1beta1/labels.go#L38)).
- Static exclusions the adapter must mirror
  ([instancetypes.go](https://github.com/Azure/karpenter-provider-azure/blob/8878fa43373fbad8974b432fc63fa9b3af921604/pkg/providers/instancetype/instancetypes.go#L565-L690),
  [skus.go](https://github.com/Azure/karpenter-provider-azure/blob/8878fa43373fbad8974b432fc63fa9b3af921604/pkg/providers/instancetype/skus.go#L93-L114)):
  AKS-restricted sizes (A0, A1, A1_v2, B1s, B1ms, F1, F1s, Basic_*,
  `Standard_E64i(s)_v3`); fewer than 2 vCPUs or less than 3.5 GiB; constrained
  vCPU sizes (`-N` in the name); confidential `DC*`/`EC*`; GPU SKUs not in the
  provider's supported list; SKUs retiring within 6 months (SC
  `PLANNED_FOR_RETIREMENT` is a useful proxy). 961 of 1,305 SC types remain.
  Per-NodeClass filters (image family vs GPU, artifact streaming excluding
  arm64, encryption at host, …) are not mirrored; the NodePool simply gets
  fewer usable types.
- On-demand launches also need per-family vCPU quota in the subscription.
- SC `family` (`Dadv6`) is not the provider's `sku-series` (`Dads_v6`); never
  emit family labels.
- NAP specifics: the `default` pool (D family, on-demand) overlaps with ours;
  "when multiple NodePools match, NAP uses the one with the highest weight".
  Users should give generated pools a weight above `default`'s, or set NAP
  default pools to `None`. NAP prefers spot when both are allowed.
- Adapter: vendor `azure`; region = SC `api_reference`; default capacity type
  `on-demand`.

#### GCP (cloudpilot-ai)

- NodeClass `karpenter.k8s.gcp/v1alpha1` `GCENodeClass` (required
  `imageSelectorTerms`). GKE Standard only.
- Instance types: machine type names = SC `api_reference`. **SC `server_id`,
  `region_id` and `zone_id` are numeric for GCP; always map via
  `api_reference`.** The provider emits several instance types with the same
  name for families with configurable local SSD (n1, n2, n2d, c2, c2d), told
  apart by `karpenter.k8s.gcp/instance-local-ssd-count`; without a pin, only
  the count=0 variant launches — the right default for us.
- Types without an on-demand price (from the third-party
  `gcloud-compute.com` CSV) have no offerings: 45 SC types (A3/A4/A4X, TPUs,
  H4D, M4N, X4 metal) are effectively absent.
- Zones are limited to the GKE cluster's `locations`; SC has some odd zones
  (`us-central1-ai1a`) that must be ignored.
- `Create()` tries types one at a time in price order, one GCE insert per
  attempt — keep lists moderate when stockouts are likely. arm64 GPU types
  cannot launch (COS image constraint). No minimum size (`e2-micro` included).
- The GCP-shipped NodePool CRD has no CEL rule for its domain; bad keys only
  show up as `ValidationSucceeded=False`.
- Adapter: vendor `gcp`; region = SC region `api_reference` → numeric
  `region_id` for Keeper; default capacity type `on-demand`.

#### Alibaba Cloud — official (AliyunContainerService)

- NodeClass `karpenter.alibabacloud.com/v1alpha1` `ECSNodeClass`; ACK (via
  `clusterID`) or self-managed. Independent rewrite by the ACK team (not a fork
  of cloudpilot).
- Instance type `ecs.g7.large` = SC `api_reference`; zone = ECS ZoneId = SC
  `zone_id`; region = SC `region_id`. One region per controller.
- **No prices** (every offering 0.0): core ordering, truncation and
  consolidation are degenerate; `Create()` walks types in DescribeInstanceTypes
  order. Our selection is the only cost control.
- Capacity: if the NodeClaim allows `spot`, it always launches spot (no
  on-demand fallback). **Generate one capacity type per pool.**
- `DescribeInstanceTypes` is called without pagination; since 2023-11 Alibaba
  returns only the first 100 types for unpaginated calls, so the provider may
  see only ~100 types per region (verify on a real account).
- Its NodePool CRD carries AWS's `karpenter.k8s.aws` CEL rule (copy-paste),
  harmless for us.
- Adapter: vendor `alicloud`; single capacity type (default `on-demand`).

#### Alibaba Cloud — cloudpilot-ai

- NodeClass `karpenter.k8s.alibabacloud/v1alpha1` `ECSNodeClass`. Core v1.2.0:
  60-type truncation, older CRD (no hostname / `karpenter.sh` restrictions).
- Names, zones and regions as above. Real prices from `price.cloudpilot.ai`
  (values look like CNY; ordering is still correct) with an embedded fallback
  dated 2024-12 that lacks 271 newer SC types — those vanish when the price
  server is unreachable. me-central-1 has no prices at all.
- **On-demand ignores the zone requirement** (random vSwitch zone); pin zones
  through the NodeClass vSwitch selector instead.
- The image is chosen from the first instance type only: **never mix
  architectures in one pool** (emit a single `kubernetes.io/arch` value).
  GPU and `metal` types are dropped when non-exotic candidates exist.
- Terway CNI drops small types with fewer than 11 ENI IPs.
- Adapter: vendor `alicloud`; single architecture per pool.

#### Hetzner — stubbi (= paperclipinc)

- NodeClass `karpenter.hetzner.cloud/v1` `HCloudNodeClass` (required
  `locations`, `imageSelector.family`, `networkID`). Helm chart
  `oci://ghcr.io/stubbi/charts/karpenter-provider-hetzner`.
- Instance type = hcloud server-type name = SC `api_reference` (SC `server_id`
  is numeric). Types are listed live; retired (type, location) pairs are kept
  but unavailable.
- Zone = location (`fsn1`, `nbg1`, `hel1`, `ash`, `hil`, `sin`) = SC region
  `aliases[0]` (SC region `api_reference` is the datacenter `fsn1-dc14`). No
  region label. Zones must be within the NodeClass `locations`.
- **Always emit `karpenter.sh/capacity-type In [on-demand]`**: otherwise
  consolidation replacements request spot and never launch.
- Architectures need a resolved image in the NodeClass
  (`status.resolvedImages`); constrain `kubernetes.io/arch` accordingly.
- Unverified: whether the hcloud CCM overwrites the node zone label with the
  datacenter name.
- Adapter: vendor `hcloud`; capacity type pinned `on-demand`.

#### Hetzner — itsh-cloud

- NodeClass `karpenter.itsh.dev/v1alpha1` `HCloudNodeClass`; kubeadm on Debian,
  x86 only (`cax*` never offered).
- Labels mirror the hcloud CCM: **region = location (`fsn1`), zone = legacy
  datacenter (`fsn1-dc14`)** — the opposite of stubbi. The CCM plans to drop
  the datacenter label, so prefer region over zone constraints.
- Capacity type must be pinned to `on-demand` (README warns of an endless
  consolidation loop otherwise).
- Adapter: vendor `hcloud`; arch `amd64`; capacity `on-demand`.

#### Hetzner — echohello-dev

- Same API surface as stubbi (`karpenter.hetzner.cloud/v1` `HCloudNodeClass`;
  appears derived from it); the adapter is shared with stubbi.
- Prices are real but location-uniform; one spread placement group per cluster
  is capped at 10 servers (the 11th create fails as capacity error).
- `karpenter.hetzner.cloud/server-family` is *not* registered as well-known
  here, so a multi-valued requirement on it would mislabel nodes — another
  reason for ADR-004.

#### UpCloud — kubekanvas

- NodeClass `karpenter.k8s.upcloud/v1alpha1` `UpCloudNodeClass` (required
  `storage.template`; `zones` optional). Self-managed clusters on UpCloud
  servers (not UKS node groups).
- Instance type = plan name, **mixed case, verbatim** (`CLOUDNATIVE-2xCPU-4GB`,
  `GPU-8xCPU-64GB-1xL4`) = SC `api_reference`.
- Zone and region labels both carry the zone id (`de-fra1`) = SC
  `zone_id` = `region_id`. Zones must be a subset of the NodeClass `zones`; an
  unknown zone in the NodeClass makes it NotReady.
- `GPU-SPOT-*` plans are labelled `spot`; SC has no spot plans, so pinning
  `on-demand` is the default.
- Detection: same group as the unsupported upcloud-tools provider, which serves
  `v1alpha2`; check the served NodeClass CRD version and refuse `v1alpha2`.
- Adapter: vendor `upcloud`; arch `amd64`; capacity `on-demand`.

#### OVHcloud — antonin-a

- NodeClass `karpenter.ovhcloud.sh/v1alpha1` `OVHNodeClass`. **OVH MKS only**;
  each NodeClaim becomes its own single-node MKS node pool (~2–5 minutes per
  launch; GPU flavors 40–80 minutes, beyond core's 15-minute registration
  timeout — the provider hides them unless opted in).
- Flavor names = SC `api_reference` (`b3-8`). One region per controller (from
  env, not the NodeClass).
- **Only five keys are safe**: instance-type, zone, capacity-type
  (`on-demand`), arch (`amd64`), os. Region and every `karpenter.ovhcloud.sh/*`
  key cause the drift loop (§5.1.5).
- Offerings advertise zones `-a`, `-b`, `-c` even in single-AZ regions:
  **the adapter always emits `topology.kubernetes.io/zone In [<SC zone_id of the
  region>]`** (`gra11-a`; `eu-west-par-a/b/c`).
- Prices are a formula (0.02 × vCPU + 0.005 × GiB + 0.50 × GPU), so the provider
  ranks gen2 below gen3; our selection is the real cost control.
- Limits: one MKS node pool per node; plan for at most ~100 Karpenter nodes per
  cluster. Instance types are read once at controller start.
- Adapter: vendor `ovh`; Kubernetes region (CCM label, uppercase) = SC
  `region_id`; exclude GPU flavors by default; arch `amd64`; capacity
  `on-demand`.

#### Vultr — crob1140

- NodeClass `karpenter.vultr.com/v1alpha1` `VultrNodeClass` (required
  `region`, `kubernetesVersion`, `clusterEndpoint`); kubeadm clusters with the
  Vultr CCM; VKE unsupported. No released image yet.
- Plan id = SC `api_reference` (`vc2-2c-4gb`). Region and zone labels are both
  the region id (`ewr`) = SC `region_id`; never use SC zone `api_reference`
  (a city name).
- The template NodeClass must leave `spec.plan` empty (otherwise the list
  collapses to one plan).
- Exclude `vbm-*` (bare metal, never listed) and `vcg-*` (GPU, listed but no GPU
  resources advertised; also stored with `gpu_count` 0 in SC).
- amd64 and on-demand only. No ICE fallback cache.
- Adapter: vendor `vultr`; region from the NodeClass `spec.region`.

## 6. Provider adapter rules

The adapter is data plus a few functions. Coders implement one adapter per
row of §5.3.

### 6.1 Detection

| `nodeClassRef.group` | Kind | Adapter |
|---|---|---|
| `karpenter.k8s.aws` | `EC2NodeClass` | `aws` |
| `eks.amazonaws.com` | `NodeClass` | `aws-auto` |
| `karpenter.azure.com` | `AKSNodeClass` | `azure` (self-hosted and NAP) |
| `karpenter.k8s.gcp` | `GCENodeClass` | `gcp` |
| `karpenter.alibabacloud.com` | `ECSNodeClass` | `alicloud-ack` |
| `karpenter.k8s.alibabacloud` | `ECSNodeClass` | `alicloud-cloudpilot` |
| `karpenter.hetzner.cloud` | `HCloudNodeClass` | `hcloud` (stubbi, echohello) |
| `karpenter.itsh.dev` | `HCloudNodeClass` | `hcloud-itsh` |
| `karpenter.k8s.upcloud` | `UpCloudNodeClass` (served `v1alpha1`) | `upcloud` |
| `karpenter.ovhcloud.sh` | `OVHNodeClass` | `ovh` |
| `karpenter.vultr.com` | `VultrNodeClass` | `vultr` |
| anything else | — | `Ready=False, reason=UnsupportedProvider` |

### 6.2 Eligibility filters (applied before selectors)

| Adapter | Drop |
|---|---|
| all | SC architectures `i386`, `arm64_mac`, `x86_64_mac`; rows without a price for the basis in the target regions |
| `aws-auto` | types without an Auto Mode SKU in the region's AWS price list |
| `azure` | the static exclusions in §5.4 |
| `gcp` | rows without an on-demand price (proxy for the provider's price file) |
| `alicloud-*` | rows without an on-demand price |
| `hcloud-itsh`, `upcloud`, `ovh`, `vultr` | `arm64` |
| `ovh` | GPU flavors (unless the user explicitly selects GPUs), `bm-*`, `d2-2` |
| `vultr` | `vbm-*`, `vcg-*` |

### 6.3 Regions

Explicit `spec.location.regions` wins. Otherwise, in order: the NodeClass
(Vultr `spec.region`; Hetzner `spec.locations`), then the distinct
`topology.kubernetes.io/region` labels of the cluster's Nodes. Mapping the
Kubernetes value to Keeper's `vendor~region_id`:

| Adapter | Kubernetes region value | SC lookup |
|---|---|---|
| `aws`, `aws-auto`, `alicloud-*`, `vultr` | `us-east-1`, `cn-hangzhou`, `ewr` | `region_id` |
| `azure` | `westeurope` | `region.api_reference` |
| `gcp` | `us-central1` | `region.api_reference` → numeric `region_id` |
| `hcloud`, `hcloud-itsh` | `fsn1` | `region.aliases` contains it → numeric `region_id` |
| `upcloud` | `de-fra1` | `region_id` |
| `ovh` | `GRA11` (CCM, uppercase) | `region_id` |

The SC region table is read once per refresh from Keeper (`GET /regions`).

### 6.4 Zones and capacity types

| Adapter | Zone requirement emitted | Default capacity type (if the template has none) |
|---|---|---|
| `aws` | only `spec.location.zones` (zone names) | `on-demand` |
| `aws-auto` | only `spec.location.zones` | `on-demand` |
| `azure` | only `spec.location.zones` (`westeurope-1`, `0`) | `on-demand` |
| `gcp` | only `spec.location.zones` | `on-demand` |
| `alicloud-ack` | only `spec.location.zones` | `on-demand`; reject templates allowing both |
| `alicloud-cloudpilot` | only `spec.location.zones` (warn: ignored on-demand) | `on-demand` |
| `hcloud` | only `spec.location.zones` (locations) | `on-demand`, always |
| `hcloud-itsh` | only `spec.location.zones` (datacenters) | `on-demand`, always |
| `upcloud` | only `spec.location.zones` | `on-demand`, always |
| `ovh` | **always**: user zones, or all SC zones of the region | `on-demand`, always |
| `vultr` | none (zone = region) | `on-demand`, always |

"Always" means a template asking for `spot` is rejected with
`reason=CapacityTypeUnsupported`.

### 6.5 Architecture

Emit the template's `kubernetes.io/arch` requirement if present, else the
architectures of the selected types. `alicloud-cloudpilot` must get exactly
one value (reject templates allowing both; users create one `SCNodePool` per
arch). For `hcloud`, intersect with architectures that have a resolved image
in the NodeClass status when readable.

## 7. Keeper API: usage and known gaps

### 7.1 How the controller queries

- `GET /servers` with `vendor`, `vendor_regions` (at most 3 per anonymous
  request; chunk and merge), `best_price_allocation` (`ONDEMAND_ONLY` or
  `SPOT_ONLY` from the price basis), `limit=-1`, Keeper's default status filter
  (`ACTIVE` + `PLANNED_FOR_RETIREMENT`), and — once per benchmark used —
  `benchmark_id` plus the canonical `benchmark_config` string from
  `GET /benchmark_configs`. Rows are joined on `api_reference`.
- Push-down: selectors with an exact Keeper equivalent are sent along to shrink
  responses (Keeper added the missing ones in
  [sc-keeper#101](https://github.com/SpareCores/sc-keeper/pull/101), live since
  2026-10-07). A pushed-down filter must be equal to or looser than the
  client-side one; the controller always re-applies every selector.

  | Selector | Keeper parameter | Notes |
  |---|---|---|
  | `vcpus.min` / `.max` | `vcpus_min` / `vcpus_max` | only values ≤ 256 (Keeper's limit) |
  | `architectures` | `architecture` | `amd64` → `x86_64`, `arm64` → `arm64` |
  | `memory.minGiB` / `.maxGiB` | `memory_min` / `memory_max` | Keeper's "GB" is GiB (× 1024 MiB) |
  | `cpu.caches.l1dPerCoreMinKiB`, `l1iPerCoreMinKiB`, `l2PerCoreMinKiB` | `cpu_l1d_cache_min`, `cpu_l1i_cache_min`, `cpu_l2_cache_min` | KiB per core |
  | `cpu.caches.l3MinMiB`, `l3TotalMinMiB` | `cpu_l3_cache_min`, `cpu_l3_cache_total_min` | MiB |
  | `memory.perVcpu.minGiB` / `.maxGiB` | `memory_per_vcpu_min` / `memory_per_vcpu_max` | |
  | `gpu.count.min` / `.max` | `gpu_min` / `gpu_max` | `gpu_max=0` = CPU-only |
  | `gpu.memoryPerGpu.minGiB`, `gpu.memoryTotal.minGiB` | `gpu_memory_min`, `gpu_memory_total` | |
  | `price.maxHourly` | `price_max` (with `currency=USD`) | applies to the region-scoped price of the basis; without the Auto Mode fee, so looser than the client-side check when the fee is included |
  | benchmark `min` / `max` (`normalize: None`) | `benchmark_score_min` / `benchmark_score_max` | only on that benchmark's query |
  | benchmark `min` (`normalize: PerVcpu`) | `benchmark_score_per_vcpu_min` | |
  | benchmark `min` (`normalize: PerPrice`) | `benchmark_score_per_price_min` | looser than client-side when the fee is included |

  Not pushed down: enum-valued selectors (`cpu.manufacturers`, `cpu.flags`,
  `gpu.manufacturers`, `gpu.models`, allocations) — Keeper rejects values that
  no active server has with HTTP 422, and flags need any-of/none-of — the
  scope globs, and `price.maxHourlyPerVcpu` (no Keeper equivalent). Benchmark thresholds are not pushed down when
  `unbenchmarked` is not `Exclude`, because Keeper drops rows without a score;
  the controller then also runs one query without benchmark parameters to get
  every candidate.
- `GET /benchmark_configs` and `GET /regions` once per refresh.
- `/servers` costs 3 credits; the anonymous limit is 300 credits per minute;
  responses are cacheable for an hour. Requests are shared across `SCNodePool`s
  with identical parameters within a refresh cycle. An optional API key lifts
  the region limit.
- On errors: retry with backoff, then keep the last NodePool and set
  `DataAvailable=False`.

### 7.2 Remaining Keeper limitations (worked around client-side)

| Limitation | Workaround |
|---|---|
| `vcpus_min`/`vcpus_max` capped at 256 (kept for the web frontend) | larger bounds only client-side |
| `cpu_flags` is all-of only; enum parameters reject unknown values | flags and enums only client-side |
| One `benchmark_id` per query | one query per benchmark used, joined on `api_reference` |
| Without `benchmark_config`, Keeper aggregates all configs of a benchmark with `MAX(score)` ([queries.py](https://github.com/SpareCores/sc-keeper/blob/7443abe9704cac0ae5b63a8a93dd93ff9acdf92e/src/sc_keeper/queries.py#L133)); a fallback, as scores of different configs are not comparable | always send a config when the benchmark has more than one; with a config there is exactly one score per server (it is part of the primary key) |
| `benchmark_config` is an exact string match | send the canonical string from `/benchmark_configs` |
| At most 3 regions per anonymous request | chunked queries |
| `only_orderable` hides servers without a price | fine: unpriced servers are not candidates |

SC data issues found on the way: Graviton `*g-flex` types missing (aws); Vultr
`vcg-a40-*` stored with `gpu_count` 0; Azure GPU metadata incomplete (21 GPU
servers without GPU memory, 38 without manufacturer); OVH prices months old;
Hetzner memory stored as GB × 1000.

## 8. Testing

### 8.1 Layers

| # | Layer | When | What it proves | Runtime |
|---|---|---|---|---|
| L1 | Unit and golden tests of the selector engine and adapters | every PR | selection, ranking, churn, requirement building, label-value sanitising, per-adapter key and capacity rules, recorded Keeper fixtures | seconds |
| L2 | CRD admission matrix | every PR | every generated NodePool is admitted by each provider's own pinned NodePool CRD (server-side dry-run in one kind cluster) | < 1 min |
| L3 | KWOK e2e with real core Karpenter | every PR | real scheduling, provisioning, drift and the drift-loop detector against per-vendor catalogs generated from live SC data, for each core version providers pin (v1.2, v1.8, v1.12, v1.14) | ~3–4 min per core version (8 vendors), versions as parallel jobs |
| L4 | In-process provider tests (AWS, Azure) | nightly (follow-up plan) | real provider instance-type filtering and NodeClaim label stamping against our NodePools | 2–5 min |
| L5 | Real AWS e2e (EKS Auto Mode + EKS with Karpenter) | manual | everything, on the real cloud | ~60 min, ≈ $0.6–0.8 |

### 8.2 L3: core Karpenter with KWOK in kind (verified 2026-10-06)

Core Karpenter's KWOK cloud provider runs the real core controllers with fake
nodes (kwok-controller), driven by an instance-type catalog file.

- Build the provider from a core tag: `go build ./kwok` in
  `kubernetes-sigs/karpenter`, wrap it in a `FROM scratch` image,
  `kind load docker-image`. Install `kwok.yaml` + `stage-fast.yaml` from
  kwok v0.8.0 (with a `CriticalAddonsOnly` toleration), the kwok chart's CRDs,
  then the chart from `kwok/charts` with
  `INSTANCE_TYPES_FILE_PATH` pointing at a ConfigMap. Bootstrap from scratch:
  ~55 s plus the Go build.
- Upstream bug to work around: the kwok chart at core HEAD renders feature
  gates its `values.yaml` does not define and crashes
  ([deployment.yaml](https://github.com/kubernetes-sigs/karpenter/blob/9d3669a86c2e462b37695ddd8fda4d1419092de2/kwok/charts/templates/deployment.yaml#L103-L104));
  set all five gates (`staticCapacity`, `capacityBuffer`,
  `spotToSpotConsolidation`, `nodeRepair`, `podDeletionCostManagement`) in
  the values file.
- Catalog format: JSON array of `{name, architecture, operatingSystems,
  resources, offerings: [{Price, Available, Requirements}]}`. Every key in
  offering requirements becomes well-known, so each vendor's label surface
  (names, provider keys, zone formats) can be emulated. A generator in
  `test/kwok/` builds one catalog per vendor **in CI from the live Spare
  Cores data** (Keeper, or the public dump), so the lane also catches data
  changes that break generation. Because the catalogs change between runs,
  assertions are relative to the generated catalog (for example "every
  NodeClaim type is in the NodePool's list", "narrowing drifts exactly the
  removed types"), never fixed type names; the data timestamp and the
  generated catalogs are uploaded as CI artifacts so failures can be
  reproduced.
- **Core version matrix**: the lane runs once per Karpenter core version the
  supported providers pin — v1.2 (cloudpilot Alibaba), v1.8 (official Alibaba,
  OVH), v1.12 (Vultr) and v1.14 (AWS, Azure, GCP, Hetzner, UpCloud; EKS Auto
  Mode is equivalent) — building `kwok` from each tag. The custom
  instance-type file (`INSTANCE_TYPES_FILE_PATH`) exists in all of them; the
  chart values (feature gates) differ per version and are kept per version in
  `test/kwok/`. Whether older versions also register offering keys as
  well-known must be verified when the lane is built.
- KWOK only manages `karpenter.kwok.sh/KWOKNodeClass` NodePools, so the
  controller runs with the test-mode `nodeClassRef` override (F12).
- Verified scenarios: subset honoured (NodeClaims in 2–3 s, all types, zones,
  capacity types and arch from the allowed set); narrowing → `RequirementsDrifted`
  in 3 s, converged in ~22 s; unknown type names → pods Pending, NodePool still
  `Ready`; multi-valued requirement on an unstamped well-known key → drift loop
  (new NodeClaim every ~21 s); custom key → stable; invalid label value (a
  Vultr family with a space) → nodes fail to register. All 8 vendors passed in
  one cluster; swapping catalogs takes ~12 s.
- Limits: KWOK cannot test provider `Create()` logic, provider filters, which
  labels the real provider stamps (KWOK stamps more, so it can hide drift
  loops — negative tests must use multi-valued requirements), NodeClass
  validation, or managed modes. All offerings are always available (no ICE).

### 8.3 L2: CRD admission matrix (verified)

Install each provider's shipped `karpenter.sh_nodepools.yaml` (pinned by
release) into kind and `kubectl apply --dry-run=server` every generated
NodePool plus negative cases. Findings: AWS and the official Alibaba CRD
reject unknown `karpenter.k8s.aws/*` keys; Azure restricts
`karpenter.azure.com/*`; the cloudpilot Alibaba and CAPI CRDs do not reject
`kubernetes.io/hostname` or `karpenter.sh/*`; **no CRD validates label values**
(sanitising is our job).

The EKS Auto Mode NodePool CRD is not published; it is extracted from a live
Auto Mode cluster (`kubectl get crd nodepools.karpenter.sh -o yaml`, schema
only) and committed as a test fixture, to be refreshed when AWS updates Auto
Mode.

### 8.4 L4: in-process provider tests (follow-up)

AWS ([`pkg/test/environment.go`](https://github.com/aws/karpenter-provider-aws/blob/30c236cafacb3a23fb3a50370d09d71750b56b29/pkg/test/environment.go#L118))
and Azure ([`pkg/test/environment.go`](https://github.com/Azure/karpenter-provider-azure/blob/8878fa43373fbad8974b432fc63fa9b3af921604/pkg/test/environment.go#L132))
export test environments wired to in-memory cloud fakes. A Go test in this repo
can run their real `GetInstanceTypes`/`Create()` against generated NodePools
and assert that NodeClaim labels cover every requirement key — the real
drift-loop guard. Requires the Go implementation (ADR-007).

Alternatives evaluated and not planned: the AWS provider binary against moto
(feasible, no pricing API, ~2–3 days); standalone Hetzner/Vultr fakes (need
patched provider `main`); Cluster API + CAPD (core semantics only, heavy).

### 8.5 L5: real AWS e2e

Manually triggered workflow (`.github/workflows/e2e-aws.yml`, to be written by
plan 02) in **us-east-2** (cheapest region for the test instance types per SC
data; default VPC with public subnets, so no NAT gateway).

Static resources, managed in Spare Cores' private infrastructure repository
(Pulumi) and free while idle:

- GitHub environment `aws-e2e` (deployments from `main` only, no required
  reviewers so the scheduled sweeper can run) and the OIDC role
  `sc-karpenter-nodepool-gha`, trusted only for jobs running in that
  environment. The role can create and delete `sckn-ci-*` EKS
  clusters and the eksctl CloudFormation stacks around them, pass the roles
  below, and clean up orphans; it cannot create IAM.
- Roles: `sckn-ci-eks-cluster` (both clusters), `sckn-ci-auto-node`,
  `sckn-ci-karpenter-node`, `sckn-ci-karpenter-controller` (Pod Identity;
  policy ported from Karpenter v1.14.1's getting-started template, without the
  SQS interruption queue). The launching roles carry an explicit Deny for any
  instance type other than small `t3/t3a/t4g` and `c*/m*/r*` up to `xlarge`.
- EKS service-linked roles, a $10/month EKS budget with e-mail alerts (budgets
  without actions are free).
- Repository variables: `AWS_ROLE_ARN`, `AWS_REGION`, `AWS_ACCOUNT_ID`,
  `SCKN_VPC_ID`, `SCKN_SUBNET_IDS`, `SCKN_CLUSTER_ROLE_ARN`,
  `SCKN_AUTO_NODE_ROLE_NAME`, `SCKN_KARPENTER_NODE_ROLE_NAME`,
  `SCKN_KARPENTER_NODE_ROLE_ARN`, `SCKN_KARPENTER_CONTROLLER_ROLE_ARN`.

Run outline (both clusters in parallel matrix jobs, `fail-fast: false`,
workflow-level concurrency group, `timeout-minutes: 120`):

1. Build and push the controller image to ghcr (public package, no pull
   secret needed).
2. Pre-flight: tear down leftovers of a previous run.
3. `eksctl create cluster` (pinned eksctl, fixed cluster names `sckn-ci-auto`
   / `sckn-ci-karpenter`, pre-created roles, default VPC subnets).
   Auto Mode: only the `system` built-in pool; a custom `sckn-ci` NodeClass
   with the same node role and 20 GiB ephemeral storage. Standard: one
   `t4g.medium` managed node group for Karpenter and our controller,
   Karpenter v1.14.1 via Helm, Pod Identity.
4. Deploy the controller; apply an `SCNodePool` that yields a small known set
   (Graviton `*.large` plus `t4g.medium`/`large`, all Auto Mode-eligible),
   `limits.cpu: 16`, budget `nodes: 100%`.
5. Assert the NodePool's instance-type list equals the expected set; scale a
   `pause` Deployment (3 replicas, 900m, hostname anti-affinity); assert every
   node's `node.kubernetes.io/instance-type` is in the set.
6. Drift test: tighten the selection to exclude the running types; expect the
   NodePool update, `Drifted=True`, and convergence within 15 minutes.
7. `if: always()`: delete workload, NodePools, NodeClasses; `eksctl delete
   cluster --wait`; fallback cleanup of tagged instances, launch templates and
   instance profiles.

Capacity type: test pools use **on-demand**. Spot would save about $0.10 per
run but can fail a run for reasons unrelated to our code (interruptions during
the 15-minute drift window — self-managed Karpenter would run without an
interruption queue — and spot capacity shortages for a narrow 5–8 type list).
Spot behaviour is covered by L3. A workflow input can switch to spot for an
occasional exploratory run.

Cost per run: ≈ $0.30–0.40 per cluster (control plane $0.10/h, nodes, public
IPv4, Auto Mode fee), ≈ $0.6–0.8 for both. The main risk is a leaked control
plane ($2.40/day per cluster), covered by teardown-on-always, a sweeper and the
budget alert.

Sweeper: a scheduled workflow (every 3 hours, plus `workflow_dispatch`) in the
`aws-e2e` environment, deleting `sckn-ci-*` clusters older than 3 hours and
their orphans (Auto Mode instances are only visible with
`describe-instances --include-managed-resources`).

## 9. Plan sequence

Planned v1 work orders (written after this overview is reviewed):

| # | Plan | Depends on |
|---|---|---|
| 00 | This overview | — |
| 01 | Project scaffold (kubebuilder, Go), CRD types, conventions | 00 |
| 02 | AWS e2e workflow + sweeper (uses the Pulumi resources of §8.5) | 01 (image build) |
| 03 | Keeper client + selector engine + ranking + churn (L1 tests, recorded fixtures) | 01 |
| 04 | Provider adapters (§6) | 03 |
| 05 | Controller: reconcile, NodePool apply, status, generated-pool health, metrics | 04 |
| 06 | Local test lanes: CRD matrix (L2) and KWOK e2e (L3) in CI | 05 |
| 07 | Packaging: Helm chart, image, release workflow, how-to guides | 05 |
| 08 | In-process provider tests (L4) | 05 |

## 10. Open questions

None at the moment. Decisions taken during the review are recorded in
[`docs/explanation/DECISIONS.md`](../../../explanation/DECISIONS.md).
