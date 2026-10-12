# Write an SCNodePool

An `SCNodePool` describes the instance types you want, in terms of hardware,
price and measured performance; the controller turns it into a regular Karpenter
`NodePool` with exactly those types and keeps it current as Spare Cores data
changes. This guide shows how to build one and gives recipes for common goals.
Every selector field is explained in the [selector reference](../reference/selectors.md);
every field of the object is in the [API reference](../reference/api.md).

The recipes were evaluated against the committed Spare Cores snapshot of
2026-10-12 (AWS, `us-east-1`, on-demand prices). Live data changes the numbers
and, for close races, the families.

## The shape of an SCNodePool

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: web-fast            # also the name of the generated NodePool
spec:
  nodePoolTemplate:         # a karpenter.sh/v1 NodePool: everything except the instance types
    spec:
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
      disruption: {consolidationPolicy: WhenEmptyOrUnderutilized, consolidateAfter: 1m}
      limits: {cpu: "256"}
  selector: {}              # which instance types qualify
  ranking: {}               # optional: keep only the best families among them
```

1. **The template** is the NodePool you would write by hand, minus the
   instance-type list. Its `template.spec.nodeClassRef` is required and decides
   the provider (the controller supports the NodeClass kinds listed in the
   [install guide](install.md#provider-notes)). Labels, taints, weight,
   `expireAfter`, disruption settings and limits are copied unchanged. Your own
   requirements are kept too: `kubernetes.io/arch` restricts the candidates,
   `karpenter.sh/capacity-type` picks the default price basis (spot-only
   templates are priced on spot), and an `instance-type In [...]` list of your
   own is intersected with the selection. Where the template has no capacity
   type, the controller writes `on-demand`; where it has no architecture, it
   writes the architectures of the selected types. Zones are never chosen for
   you: pin them with a `topology.kubernetes.io/zone` requirement in the
   template.
2. **The selector** is a set of filters, all optional and AND-ed. A type must
   pass every one.
3. **The ranking** is optional. It groups the types that passed the selector by
   family, scores the families, and keeps those near the best. Every size of a
   kept family stays, so Karpenter can pick the cheapest size that fits a pod.

The regions are detected from the cluster; there is nothing to configure. If
`consolidateAfter` is missing from the template, the controller sets `0s`, which
the NodePool CRD of every current provider requires.

## Check before you apply

Set `dryRun: true` to compute and report the selection without creating or
touching a NodePool:

```bash
kubectl apply -f my-pool.yaml
kubectl get scnodepool my-pool
kubectl get scnodepool my-pool -o json | jq '.status | {count: .selected.count, band: .selected.band,
  families: [.selected.families[]?.name], excluded}'
```

`status.excluded` counts the candidates dropped by each step (`providerIneligible`,
`templateConstraints`, each filter by name, `missingData`, `ranking`), which
tells you which filter is too strict. When it looks right, set `dryRun: false`
(or remove it). Details of the status are in the
[controller reference](../reference/controller.md#status-conditions).

## Recipe: the fastest single-core machines, with a band

"The fastest CPUs for latency-sensitive services, but with enough families to
fall back on when one runs out of capacity, and nothing above $0.50 an hour."

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: fastest-single-core
spec:
  nodePoolTemplate:
    spec:
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
          requirements:
            - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
      disruption: {consolidationPolicy: WhenEmptyOrUnderutilized, consolidateAfter: 1m}
  selector:
    vcpus: {min: 2, max: 16}
    instanceTypes: {exclude: ["*.metal*"]}
    cpu: {allocations: [Dedicated]}
    price: {maxHourly: "0.50"}
  ranking:
    score: {benchmark: {id: geekbench:score, config: {cores: single}}}
    objective: Score
    band: {withinPercent: 10}
    families: {min: 3, max: 6}
```

Result: 7 instance types in 3 families (`m8azn`, `r8a`, `m8a`). `band.withinPercent: 10`
keeps the families whose median single-core score is within 10% of the best;
`families.min: 3` is the capacity floor (the band is widened until three families
are in), `families.max: 6` the ceiling. `status.selected.band` shows the reference
family, the lowest kept one and whether the band was widened or trimmed. With
`withinPercent: 12`, `c8a` joins. See
[ranking](../reference/selectors.md#ranking) for the algorithm.

Why a score of the *family*, not the instance type: single-core scores barely
change between sizes of a family, and ranking sizes would only ever favour the
smallest one.

## Recipe: the cheapest throughput

"Most compute per dollar, any architecture, no burstable CPUs."

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: cheap-throughput
spec:
  nodePoolTemplate:
    spec:
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
          requirements:
            - {key: karpenter.sh/capacity-type, operator: In, values: [spot, on-demand]}
      disruption: {consolidationPolicy: WhenEmptyOrUnderutilized, consolidateAfter: 1m}
  selector:
    vcpus: {min: 2, max: 32}
    instanceTypes: {exclude: ["*.metal*"]}
    cpu: {allocations: [Dedicated]}
    memory: {perVcpu: {minGiB: 2}}
    price: {maxHourlyPerVcpu: "0.06"}
  ranking:
    score: {benchmark: {id: stress_ng:bestn}}
    objective: ScorePerPrice
    band: {withinPercent: 15}
    families: {min: 4, max: 8}
```

`stress_ng:bestn` is the all-core throughput; `ScorePerPrice` ranks families by
score per dollar. `price.maxHourlyPerVcpu` caps what a unit of CPU may cost
without capping the node size (`price.maxHourly` would also limit large pods).
`cpu.allocations: [Dedicated]` keeps out burstable types, whose per-dollar
numbers look good until their credits run out. Result: 19 types in the families
`c8a`, `c7a`, `a1` and `m7a` (the floor of four families widened the band).
Prices use the on-demand basis unless the template allows only spot; set
`price.basis: Spot` to rank on spot prices explicitly.

## Recipe: no hyper-threading

"One vCPU per physical core, for workloads that suffer from SMT siblings or are
licensed per core."

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: no-smt
spec:
  nodePoolTemplate:
    spec:
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
      disruption: {consolidationPolicy: WhenEmptyOrUnderutilized, consolidateAfter: 1m}
  selector:
    vcpus: {min: 4, max: 32}
    instanceTypes: {exclude: ["*.metal*"]}
    cpu:
      hyperthreading: false
      allocations: [Dedicated]
    price: {maxHourly: "1.00"}
```

`cpu.hyperthreading: false` keeps the types with as many physical cores as vCPUs
(Graviton, the AMD `*a` generations, ...); `true` would select the SMT ones.
Result: 127 types of about 50 families. Physical core counts come from the Spare
Cores inspector, so types it has not measured are excluded. Add a `ranking`
block to narrow the list to the fastest families.

## Recipe: GPUs by memory

"NVIDIA GPUs with at least 40 GiB per device, one to four of them, for a model
that does not fit a smaller card."

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: gpu-40g
spec:
  nodePoolTemplate:
    spec:
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
          taints: [{key: nvidia.com/gpu, value: "true", effect: NoSchedule}]
      disruption: {consolidationPolicy: WhenEmpty, consolidateAfter: 10m}
  selector:
    accelerator:
      types: [GPU]
      manufacturers: [NVIDIA]
      count: {min: 1, max: 4}
      memoryPerDevice: {minGiB: 40}
    price: {maxHourly: "12"}
```

`memoryPerDevice` is the memory of the smallest accelerator of the type, and
`memoryTotal` the sum over all of them. Result: 11 types (`g6e`, `g7e`, `p5`).
The taint keeps ordinary pods off the expensive nodes; pods that want a GPU
tolerate it. Install the device plugin and driver your provider's image needs;
the controller only chooses the instance types. A `max: 0` count selects the
opposite: types without any accelerator.

## Recipe: an ordered preference with two pools

A NodePool cannot order its types, but Karpenter tries NodePools in descending
`weight` order. For "the fastest first, anything reasonably fast when that
capacity is not available":

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: api-fastest
spec:
  nodePoolTemplate:
    spec:
      weight: 100
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
      disruption: {consolidationPolicy: WhenEmptyOrUnderutilized, consolidateAfter: 1m}
  selector:
    vcpus: {min: 2, max: 16}
    price: {maxHourly: "0.50"}
  ranking:
    score: {workloadProfile: web}
    band: {withinPercent: 5}
    families: {max: 3}
---
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: api-fallback
spec:
  nodePoolTemplate:
    spec:
      weight: 50
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
      disruption: {consolidationPolicy: WhenEmptyOrUnderutilized, consolidateAfter: 1m}
  selector:
    vcpus: {min: 2, max: 16}
    price: {maxHourly: "0.50"}
  ranking:
    score: {workloadProfile: web}
    band: {withinPercent: 20}
    families: {min: 4, max: 10}
```

Here `workloadProfile: web` ranks by Spare Cores' composite web-serving score
instead of one benchmark. The first pool gets 6 types in 2 families, the second
14 types in 5; pods go to the first until it cannot launch, then to the second.

## Tips

- **Start with a price cap and a ranking.** Without a ranking every candidate
  that passes the filters is emitted, which can be hundreds of types.
- **Keep several families.** A narrow list invites capacity errors and, on spot,
  interruptions; `families.min` exists for that. Spot-to-spot consolidation in
  Karpenter needs at least 15 cheaper candidate types.
- **Benchmarks vs. specifications.** Karpenter can already filter by vCPU and
  memory; use Spare Cores data for what it cannot: measured speed
  (`benchmarks`, `workloadProfiles`, `ranking`), CPU flags (`cpu.flags`), cache
  sizes, hyper-threading, GPU memory.
- **Missing data.** New instance types lack benchmarks for a while. By default
  they are excluded (`unbenchmarked: Exclude`); `IncludeIfFamilyQualifies` keeps
  an unscored size if another size of its family qualifies.
- **Changing a selection.** Types that stop qualifying are removed from the
  NodePool only after they failed `churn.removeAfterRefreshes` refreshes and at
  most `churn.maxRemovalsPerRefresh` at a time, because removing a type drifts
  and replaces its running nodes. Additions apply immediately. A change of the
  spec bypasses the waiting but not the cap.
- **Refresh now.** `kubectl annotate scnodepool my-pool
  karpenter.sparecores.com/refresh-requested="$(date +%s)" --overwrite`
  re-evaluates immediately; otherwise it happens every `refresh.interval`
  (default 24h).
- **Deleting.** Deleting an SCNodePool deletes its NodePool and Karpenter drains
  the nodes. `deletionPolicy: Orphan` keeps the NodePool.

## If it does not work

`kubectl get scnodepool` shows `Ready`; `kubectl describe scnodepool <name>`
shows the conditions and Events. The reasons are listed in the
[controller reference](../reference/controller.md#ready-reasons); the common ones:

| Reason | What to do |
|---|---|
| `NoCandidates` | A filter is too strict or in the wrong unit. Read `status.excluded`. |
| `RegionPending` | The controller found no region yet: the NodeClass is not ready, or no node carries a `topology.kubernetes.io/region` label. It retries; annotate for an immediate retry. |
| `UnknownBenchmarkConfig` | The benchmark `id` and `config` pair does not exist; see the [benchmark catalog](../reference/selectors.md#benchmark-catalog). |
| `DataStale` | Keeper or the AWS price list is unreachable. The last NodePool stays in place. |
| `UnsupportedProvider` | The `nodeClassRef` group and kind are not supported, or the NodeClass CRD is not installed. |
| `NameConflict` | A NodePool with the name of the SCNodePool exists and was not created by the controller. Rename one of them. |
