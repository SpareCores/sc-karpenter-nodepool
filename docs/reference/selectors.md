# Selectors

How an `SCNodePool` selects instance types from Spare Cores data: every
selector field, which Spare Cores field it reads, units, semantics, coverage per
vendor, and the ranking and data-quality rules applied on top.

## Table of Contents

- [Terminology](#terminology)
- [How selection works](#how-selection-works)
- [The `spec.selector` and `spec.ranking` API](#the-specselector-and-specranking-api)
- [Selector reference](#selector-reference)
  - [Scope](#scope)
  - [CPU](#cpu)
  - [CPU features](#cpu-features)
  - [CPU caches](#cpu-caches)
  - [Memory](#memory)
  - [GPU](#gpu)
  - [Price](#price)
  - [Benchmarks](#benchmarks)
  - [Workload profiles](#workload-profiles)
  - [Ranking](#ranking)
  - [Data-quality policy](#data-quality-policy)
- [Benchmark catalog](#benchmark-catalog)
- [Coverage by vendor](#coverage-by-vendor)
- [Not supported (yet)](#not-supported-yet)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [References](#references)

## Terminology

- **Spare Cores (SC)**: the open dataset of cloud server specs, prices and
  benchmark results, crawled by `sc-crawler` and served by the Keeper API.
- **Keeper**: the public Spare Cores API, `https://keeper.sparecores.net`.
  The controller reads `GET /servers` and `GET /benchmark_configs`.
- **Server row**: one instance type of one vendor in SC (for example
  `aws` / `m7i.large`). Its `api_reference` is exactly the value Karpenter uses
  for `node.kubernetes.io/instance-type` on every supported provider.
- **Candidate**: a server row that survived the provider eligibility filter
  and is offered in at least one of the target regions.
- **Benchmark**: a measured (or compound) score in SC, identified by a
  `benchmark_id` plus a `config` (a JSON object, for example
  `{"cores": "single"}`).
- **Workload profile**: a compound benchmark
  (`workload_profile:<name>`) that combines several normalised benchmarks into
  one score, where 1.0 is the fleet-median server.
- **Price basis**: which SC price a filter or ranking uses: the cheapest
  on-demand or spot hourly price of the server in the target regions, in USD.

## How selection works

The controller turns one `SCNodePool` into one set of instance types, in this
order:

1. **Provider and regions.** The provider adapter is chosen from
   `spec.nodePoolTemplate.spec.template.spec.nodeClassRef.group`. Regions come
   from `spec.location.regions`, or are auto-detected (see the overview).
2. **Fetch.** For each benchmark used by `spec.selector.benchmarks`,
   `spec.selector.workloadProfiles` or `spec.ranking`, one Keeper query:
   `GET /servers?vendor=<v>&vendor_regions=<v>~<r>&benchmark_id=<id>&benchmark_config=<canonical>&best_price_allocation=<basis>&limit=-1`
   (regions in chunks of three). Without benchmarks, a single query without the
   benchmark parameters. Rows are joined on `api_reference`.
   Keeper's default status filter applies: `ACTIVE` or
   `PLANNED_FOR_RETIREMENT`.
3. **Provider eligibility.** The adapter removes types the provider cannot
   launch (for example EKS Auto Mode's instance list and size rule, Azure's
   static SKU exclusions, Vultr bare metal).
4. **Template constraints.** `kubernetes.io/arch` and
   `karpenter.sh/capacity-type` requirements in the template restrict
   candidates (arch) and pick the default price basis (capacity type).
5. **Selectors.** Every set field of `spec.selector` is a filter; all filters
   are AND-ed. Fields that are not set do not filter.
6. **Data-quality policy.** Rows missing a value that a filter needs are
   handled by `spec.selector.unbenchmarked` (benchmarks) or excluded (all other
   fields).
7. **Ranking** (optional). Rank families by the objective, keep the families
   inside the score band, widened or trimmed to the configured family count;
   every qualifying size of a kept family stays.
8. **Churn control.** Removals are delayed and capped (see the overview, §3.5).
9. **Emit.** `node.kubernetes.io/instance-type In [<api_reference>...]`,
   sorted.

Filters with an exact Keeper equivalent are also sent to Keeper to shrink
responses (see the overview plan, §7.1). The controller re-applies every filter
to the returned rows, so semantics and units are always the ones documented
here, whatever Keeper supports.

## The `spec.selector` and `spec.ranking` API

All fields are optional. Quantities of memory are GiB (2^30 bytes), prices are
USD per hour, scores are in the benchmark's own unit.

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: web-fast
spec:
  # nodePoolTemplate, location, refresh, churn: see the overview plan
  selector:
    instanceTypes: {include: ["*"], exclude: ["*.metal*"]}   # globs on api_reference
    families: {include: [], exclude: ["t*"]}                 # globs on SC family
    vcpus: {min: 2, max: 16}
    architectures: [amd64, arm64]
    cpu:
      manufacturers: [AMD, AWS, Intel]
      allocations: [Dedicated]
      flags:
        allOf: [avx2]
        anyOf: []
        noneOf: []
      caches:
        l2PerCoreMinKiB: 2048
        l3MinMiB: 32
    memory:
      minGiB: 4
      maxGiB: 64
      perVcpu: {minGiB: 2, maxGiB: 8}
    gpu:
      count: {min: 0, max: 0}           # CPU-only
      memoryPerGpu: {minGiB: 16}
      memoryTotal: {minGiB: 24}
      manufacturers: [NVIDIA]
      models: [L4, L40S]
    price:
      basis: OnDemand                   # OnDemand | Spot; default from capacity-type
      maxHourly: "0.40"
      maxHourlyPerVcpu: "0.05"
      includeManagedFee: true           # EKS Auto Mode only; default true there
    benchmarks:
      - id: static_web:rps-extrapolated
        config: {connections_per_vcpus: 8.0, size: "1k"}
        min: 20000
        normalize: None                 # None | PerVcpu | PerPrice
      - id: passmark:memory_latency
        max: 60                         # lower is better
    workloadProfiles:
      - name: web
        min: 1.5
    unbenchmarked: Exclude              # Exclude | IncludeIfFamilyQualifies | Include
  ranking:
    score:
      workloadProfile: web              # or: benchmark: {id, config}
    objective: Score                    # Score | ScorePerPrice | Price
    normalize: None                     # None | PerVcpu
    band:
      withinPercent: 12                 # families within 12% of the best family
    families:
      min: 3                            # widen the band until at least 3 families
      max: 6                            # never more than 6 families
```

## Selector reference

Coverage columns give the share of orderable (ACTIVE or
PLANNED_FOR_RETIREMENT) SC servers with a value, per vendor, from the
2026-10-05 data dump. Order: aws / azure / gcp / alicloud / hcloud / ovh /
upcloud / vultr.

### Scope

| Field | SC source | Semantics | Coverage |
|---|---|---|---|
| `instanceTypes.include` / `.exclude` | `server.api_reference` | Shell globs (`*`, `?`, `[...]`), case-sensitive. A type is kept if it matches any `include` (or `include` is empty) and no `exclude`. | 100 everywhere |
| `families.include` / `.exclude` | `server.family` | Same glob rules on the SC family string (for example `m7i`, `Dadv6`, `ecs.g7`, `Cloud Native`). SC families equal Karpenter's family label only on AWS and GCP. | 99–100 |

Use these to pin or ban types explicitly; they compose with every other filter.

### CPU

| Field | SC source | Semantics | Coverage |
|---|---|---|---|
| `vcpus.min` / `.max` | `server.vcpus` | Inclusive bounds. Sent to Keeper only when ≤ 256 (Keeper's limit); larger bounds are applied client-side only. | 100 everywhere |
| `architectures` | `server.cpu_architecture` | Kubernetes names: `amd64` (SC `x86_64`), `arm64` (SC `arm64`). SC `i386`, `arm64_mac` and `x86_64_mac` are never candidates. If the template has a `kubernetes.io/arch` requirement, the two are intersected. | 100 everywhere |
| `cpu.manufacturers` | `server.cpu_manufacturer` | Exact, case-sensitive SC values: `Intel`, `AMD`, `AWS`, `Ampere`, `Alibaba`, `Microsoft`, `Hygon`. | 100 / 76 / 63 / 97 / 100 / 89 / 99 / 100 |
| `cpu.allocations` | `server.cpu_allocation` | `Shared`, `Burstable`, `Dedicated`. | 100 everywhere |

`cpu_manufacturer` is missing mostly where SC has not run its inspector yet;
rows without a value are excluded when this filter is set.

### CPU features

| Field | SC source | Semantics | Coverage |
|---|---|---|---|
| `cpu.flags.allOf` | `server.cpu_flags` | Every listed flag must be present. | 84 / 76 / 66 / 49 / 100 / 89 / 99 / 95 |
| `cpu.flags.anyOf` | same | At least one listed flag must be present. | same |
| `cpu.flags.noneOf` | same | None of the listed flags may be present. | same |

Flags use Linux `/proc/cpuinfo` names, lowercase. x86 and ARM vocabularies
differ: for example `avx2`, `avx512f`, `avx512_vnni`, `avx512_bf16`,
`amx_tile`, `sha_ni` on x86; `asimd`, `sve`, `sve2`, `i8mm`, `bf16` on ARM.
Karpenter has no label for CPU flags at all, which makes this one of the main
reasons to use Spare Cores. Servers the SC inspector has not measured have an
empty flag list, so they fail `allOf`/`anyOf` (and pass `noneOf` only if
`unbenchmarked: Include`; see [data quality](#data-quality-policy)).

### CPU caches

| Field | SC source (KiB) | Semantics | Coverage |
|---|---|---|---|
| `cpu.caches.l1dPerCoreMinKiB` | `cpu_l1d_cache` | L1 data cache per physical core, ≥ | 84 / 78 / 66 / 48 / 82 / 89 / 99 / 95 |
| `cpu.caches.l1iPerCoreMinKiB` | `cpu_l1i_cache` | L1 instruction cache per physical core, ≥ | same |
| `cpu.caches.l2PerCoreMinKiB` | `cpu_l2_cache` | L2 cache per physical core, ≥ | same |
| `cpu.caches.l3MinMiB` | `cpu_l3_cache` / 1024 | Largest single L3 domain visible to the VM, ≥ | 83 / 78 / 66 / 48 / 82 / 89 / 99 / 95 |
| `cpu.caches.l3TotalMinMiB` | `cpu_l3_cache_total` / 1024 | Sum of all L3 domains visible to the VM, ≥ | same |

Only lower bounds exist: nobody needs less cache. The values come from `lscpu`
inside a running VM (the SC inspector), so coverage equals inspector coverage.
Read them with care:

- **L1 and L2 are per physical core** and comparable across vendors (for
  example `m7i`: 48 KiB L1d, 2 MiB L2; `c7a`: 32 KiB, 1 MiB; `m8g`: 64 KiB,
  2 MiB). The `*_total` values (per core × cores) are deliberately not exposed:
  they only restate the core count.
- **L3 is shared**, and what a VM sees is not what it gets. A 2-vCPU
  `m7i.large` reports the full 105 MiB of its Sapphire Rapids socket, shared
  with every other tenant on the host. AMD exposes one L3 per core complex
  (`c7a`: 32 MiB per domain, 768 MiB in total on `c7a.48xlarge`), Graviton one
  per socket (`m8g`: 36 MiB). So `l3MinMiB` compares "how large a cache can one
  thread hit", not a dedicated amount, and a larger instance of the same
  family does not get more `l3MinMiB`.
- For workloads where cache behaviour really matters, prefer the measured
  effect: `membench:latency` and `membench:bandwidth_read` per working-set size
  (`{"size_kb": N}`, 16 KiB to 2 GiB) show where the latency cliff of each
  type is (coverage is lower: 213 AWS types).

### Memory

| Field | SC source | Semantics | Coverage |
|---|---|---|---|
| `memory.minGiB` / `.maxGiB` | `server.memory_amount` (MiB) | `memory_amount / 1024` compared inclusively. | 100 everywhere |
| `memory.perVcpu.minGiB` / `.maxGiB` | `memory_amount / 1024 / vcpus` | Inclusive bounds on GiB per vCPU (for example 2 = compute-optimised, 4 = general purpose, 8 = memory-optimised). | 100 everywhere |

Hetzner stores memory as GB × 1000 MiB (a 4 GB server is 4000 MiB, about 3.9
GiB). Prefer slightly lower bounds (for example `minGiB: 3.8`) on Hetzner.

### GPU

| Field | SC source | Semantics | Coverage (GPU servers only) |
|---|---|---|---|
| `gpu.count.min` / `.max` | `server.gpu_count` | Inclusive; a float (fractional vGPU slices exist, for example 0.25). `max: 0` selects CPU-only servers. | 100 everywhere |
| `gpu.memoryPerGpu.minGiB` | `server.gpu_memory_min` (MiB) | Smallest GPU's memory. | aws 100, azure 55, others 95–100 |
| `gpu.memoryTotal.minGiB` | `server.gpu_memory_total` (MiB) | Sum over GPUs. | same |
| `gpu.manufacturers` | `server.gpu_manufacturer` | `NVIDIA`, `AMD`, `Habana`, `Google`. | aws 100, azure 19, alicloud 74, others 100 |
| `gpu.models` | `server.gpu_model` | Exact SC model strings; **not normalised across vendors** (for example `L40S`, `A10G`, `RTX Pro 6000`, `RTX PRO 6000 Blackwell Server Edition`). | aws 100, azure 55, alicloud 74, others 100 |

GPU model, count and memory are also available as Karpenter labels on AWS and
Azure; SC adds measured GPU performance (`llm_speed`, `nvbandwidth`, see
[benchmarks](#benchmarks)). Vultr's only active GPU plan is stored with
`gpu_count` 0; the Vultr adapter excludes `vcg-*` plans by name.

### Price

| Field | SC source | Semantics |
|---|---|---|
| `price.basis` | `server_price.allocation` | `OnDemand` or `Spot`. Default: `Spot` if the template's `karpenter.sh/capacity-type` allows only spot, otherwise `OnDemand`. On-demand is the safer default for ranking: spot prices in SC are of uneven age. |
| `price.maxHourly` | Keeper `min_price` for the basis, restricted to the target regions | Inclusive upper bound in USD per hour. Prices of EUR vendors (hcloud, ovh, upcloud) are converted by Keeper. **Also caps the node size**: pods larger than the biggest affordable type never fit the pool. |
| `price.maxHourlyPerVcpu` | the same price divided by `server.vcpus` | Inclusive upper bound in USD per vCPU-hour. Caps what you pay for a unit of CPU without capping node size. Evaluated client-side only. |
| `price.includeManagedFee` | AWS public price list (`AmazonEKS`, `eksproducttype=AutoMode`, per region and instance type) | On EKS Auto Mode, add the per-instance management fee (about 12% of the on-demand price, charged for spot too) before the price filters, `PerPrice` normalisation and ranking. Default `true` on Auto Mode, ignored elsewhere. |

Servers without a price for the basis in any target region are not candidates.

Coverage of on-demand prices: 99 / 100 / 98 / 65 / 100 / 94 / 100 / 100.
Spot prices exist only for aws (98), azure (100), gcp (97), alicloud (41) and
a few upcloud servers. hcloud, ovh and vultr have no spot.

### Benchmarks

Each entry of `selector.benchmarks` is one filter; entries are AND-ed.

| Field | Semantics |
|---|---|
| `id` | A `benchmark_id` from the [catalog](#benchmark-catalog). |
| `config` | A JSON object. Required when the benchmark has more than one config. Validated against `GET /benchmark_configs` by comparing parsed objects; the canonical string returned by Keeper is what is sent (Keeper matches the string exactly, including key order and `8.0`-style floats). |
| `min` | Inclusive lower bound on the (normalised) score. Use for higher-is-better benchmarks. |
| `max` | Inclusive upper bound. Use for lower-is-better benchmarks (latencies). Setting `min` on a lower-is-better benchmark (or `max` on a higher-is-better one) is rejected. |
| `normalize` | `None` (raw score), `PerVcpu` (score / vCPUs: throughput per vCPU, comparable across sizes), `PerPrice` (score / hourly price on the price basis, including the managed fee where enabled). |

Picking the right metric matters more than the threshold:

| Question | Metric |
|---|---|
| How fast is one thread (latency, serial steps, CI)? | `geekbench:score {"cores": "single"}`, `passmark:cpu_single_threaded_test`, `stress_ng:best1` |
| How much throughput per vCPU or per dollar? | `stress_ng:bestn` or `passmark:cpu_mark` with `PerVcpu` or `PerPrice` |
| Memory-bound analytics? | `membench:bandwidth_read {"scope": "RAM"}`, `passmark:memory_mark`, `workload_profile:data_analysis` |
| A specific application? | `redis:*`, `static_web:*`, `openssl`, `compression_text:*`, `pgbench:*`, `llm_speed:*` |
| GPU data movement? | `nvbandwidth:*` |

Geekbench multi-core stops scaling after about 16 vCPUs, so a raw
`geekbench:score {"cores": "multi"}` threshold excludes large instances that
are fast in aggregate; `stress_ng:bestn` scales linearly. Single-core scores
are nearly constant across sizes of a family, so `PerPrice` on a single-core
score always favours the smallest size; prefer [ranking by family](#ranking).

### Workload profiles

`selector.workloadProfiles` is sugar for benchmark filters on
`workload_profile:<name>` (no config). Names: `web`, `compute`, `cache`,
`data_analysis`, `llm`, `cicd`. Fields: `name`, `min`, `normalize` (same
meaning as for benchmarks). Scores are unitless; 1.0 is the fleet-median
server, so `min: 1.5` reads "at least 1.5× the median server".

| Profile | What it combines (weights) |
|---|---|
| `web` | static web rps 1k and 64k at 8 conn/vCPU (0.30, 0.20), throughput 256k (0.20), openssl AES-256-CBC (0.20), brotli compression (0.05), string sorting (0.05) |
| `compute` | stress-ng multi (0.15) and single (0.10), PassMark CPU mark (0.20), memory read 64 MB (0.10), floating point (0.15), extended instructions (0.15), integer (0.10), physics (0.05) |
| `cache` | redis SET rps pipeline 1 (0.50) and 16 (0.20), PassMark memory mark (0.10), memory read 16 MB (0.10), single thread (0.10) |
| `data_analysis` | PassMark CPU mark (0.70), compression (0.10), memory read 64 MB (0.10), memory mark (0.10) |
| `llm` | llama.cpp tokens/s on small models (required) and 7B/70B models (penalised), memory bandwidth, extended instructions, floating point |
| `cicd` | Geekbench clang multi (0.50) and single (0.10), stress-ng multi (0.20), integer, compression, brotli, string sorting (0.05 each) |

Recipes are defined in
[`sc_crawler/workload_profiles.py`](https://github.com/SpareCores/sc-crawler/blob/85b0ef97d75db97266501e05fb1895ee5baddfa6/src/sc_crawler/workload_profiles.py#L125).

### Ranking

Ranking is optional. Without it, every candidate that passes the filters is
emitted. Ranking runs **after** all filters (including price), so it always
works on affordable, eligible types.

| Field | Semantics |
|---|---|
| `score.benchmark` (`id`, `config`) or `score.workloadProfile` | What to rank by. Exactly one. Lower-is-better benchmarks rank ascending. |
| `objective` | `Score` (fastest first), `ScorePerPrice` (most score per dollar first), `Price` (cheapest first). |
| `normalize` | `None` or `PerVcpu`, applied to the score before the objective. |
| `band.withinPercent` | Keep the families whose objective is within this percentage of the best family's: at least `best × (1 − p)` for `Score` and `ScorePerPrice` (at most `best × (1 + p)` for lower-is-better scores and for `Price`). Optional. |
| `families.min` | If fewer families are inside the band, extend down the ranking until there are this many (default 1). This is the capacity floor: it guarantees alternatives when a family is out of capacity. |
| `families.max` | Never keep more than this many families, even if more are inside the band (default: no limit). |

At least one of `band` and `families.max` must be set; `families.min` must not
exceed `families.max`.

Algorithm:

1. Group the candidates by family. A family's value is the median objective of
   its qualifying, scored sizes.
2. Sort families by value, best first; the first one is the reference.
3. Keep the families inside the band (all families if there is no band).
4. Fewer than `families.min`: add the next families in rank order until the
   minimum is reached (or the candidates run out). More than `families.max`:
   keep the best `families.max`.
5. Every qualifying size of a kept family is emitted.

`status.selected.band` reports what was applied: the reference family and
value, the lowest kept family and its distance from the reference in percent,
and whether `families.min` widened or `families.max` trimmed the band.

Example — "the fastest single-core machines, but not too few for capacity
fallback, and nothing above $0.50/h" (AWS us-east-1, on-demand, 2026-10-05 data):

```yaml
selector:
  price: {maxHourly: "0.50"}
ranking:
  score: {benchmark: {id: geekbench:score, config: {cores: single}}}
  objective: Score
  band: {withinPercent: 12}
  families: {min: 3, max: 6}
```

| Family | Sizes under $0.50/h | Median Geekbench single | Kept |
|---|---|---|---|
| m8azn | medium, large, xlarge | 3109 | reference |
| r8a | medium, large, xlarge | 2817 | yes (−9%) |
| m8a | medium, large, xlarge, 2xlarge | 2815 | yes (−9%) |
| c8a | large, xlarge, 2xlarge | 2755 | yes (−11%) |
| c9g | large, xlarge, 2xlarge | 2339 | no (−25%, outside the band; the band already has ≥ 3 families) |

Result: 4 families, 13 instance types. Without the price cap, `x8aedz` and
`hpc8a` would also be inside the band.

Why families and not instance types:

- Fallback happens per pod: only types big enough for a pod are candidates. Keeping
  every size of N families gives every pod size about N interchangeable
  choices; a cap on instance types would cut sizes instead and make large pods
  unschedulable.
- Ranking individual types by score per price always favours the smallest size
  of a family (single-core scores are nearly constant across sizes).
- Inside a NodePool every Karpenter provider picks the cheapest fitting type by
  itself. The band decides which speeds are acceptable; Karpenter then picks the
  cheapest acceptable type.

Order between families is not expressible inside one NodePool. For a strict
preference ("try the fastest first, fall back to slower only on capacity
errors") create two `SCNodePool`s, for example a 5% band with template
`weight: 100` and a 15% band with `weight: 50`: Karpenter tries the
higher-weight pool first and moves on once its offerings are unavailable.

### Data-quality policy

`selector.unbenchmarked` decides what happens to candidates that lack a score
needed by a benchmark filter, workload profile or the ranking score:

| Value | Behaviour |
|---|---|
| `Exclude` (default) | Drop them. Safe, but drops the newest families, which SC benchmarks last (for example on AWS: `c8ib`, `m9g`, `r9g`, `p5*`, `p6-*`). |
| `IncludeIfFamilyQualifies` | Keep an unscored size if at least one scored size of the same family passes every benchmark filter (and, with ranking, the family made the cut). |
| `Include` | Keep them, ignoring benchmark filters for that type. |

Missing values for any other selector field (CPU manufacturer, flags, GPU
memory) always exclude the candidate when that field is set. The number of
candidates dropped for missing data is reported in
`status.excluded.missingData`.

## Benchmark catalog

From the 2026-10-05 data dump. "↓" marks lower-is-better benchmarks. Config
dimensions are listed as `key ∈ values`; the exact accepted configs are what
`GET /benchmark_configs` returns.

| Family | Benchmark ids | Unit | Config dimensions |
|---|---|---|---|
| stress-ng | `stress_ng:best1` (single core), `stress_ng:bestn` (all cores, the "SCore") | bogo ops/s | none |
| stress-ng (raw) | `stress_ng:div16`, `stress_ng:cpu_all` | ops/s | `cores` (not listed by `/benchmark_configs`; avoid) |
| Geekbench 6 | `geekbench:score` and subscores `asset_compression`, `background_blur`, `clang`, `file_compression`, `hdr`, `horizon_detection`, `html5_browser`, `navigation`, `object_detection`, `object_remover`, `pdf_renderer`, `photo_filter`, `photo_library`, `ray_tracer`, `structure_from_motion`, `text_processing` | score (2500 = baseline) | `cores ∈ {single, multi}` |
| PassMark | `passmark:cpu_mark`, `memory_mark`, `cpu_single_threaded_test`, `cpu_integer_maths_test`, `cpu_floating_point_maths_test`, `cpu_extended_instructions_test`, `cpu_encryption_test`, `cpu_compression_test`, `cpu_physics_test`, `cpu_prime_numbers_test`, `cpu_string_sorting_test`, `database_operations`, `memory_read_cached`, `memory_read_uncached`, `memory_write`; `passmark:memory_latency` ↓ | various (MB/s, Mops/s, ns) | none |
| membench | `membench:bandwidth_read`, `bandwidth_write`, `bandwidth_copy` (MB/s); `membench:latency` ↓ (ns) | | `{"scope": "RAM"}` or `size_kb ∈ 16 … 2097152` |
| lmbench | `bw_mem` | MB/s | `operation ∈ {rd, wr, rdwr}` × `size` (MB) |
| Redis | `redis:rps`, `redis:rps-extrapolated`; `redis:latency` ↓ | ops/s, ms | `operation: SET` × `pipeline ∈ {1, 4, 16, 64, 256, 512}` |
| Static web (nginx) | `static_web:rps`, `rps-extrapolated`, `throughput`, `throughput-extrapolated`; `static_web:latency` ↓ | rps, B/s, s | `connections_per_vcpus ∈ {0.5 … 32}` × `size ∈ {1k, 16k, 64k, 256k, 512k}` |
| OpenSSL | `openssl` | B/s | `algo` (sha256, sha512, sha3-*, shake*, blake2b512, AES-256-CBC, ARIA-256-CBC, CAMELLIA-256-CBC, SM4-CBC) × `block_size ∈ {16 … 16384}` |
| Compression | `compression_text:compress`, `decompress` (B/s); `compression_text:ratio` ↓ | | `algo` (lz4, gzip, lzma, zpaq, zstd, bzip2, bzip3, brotli) × `compression_level` × `cores`, optional `block_size` |
| LLM (llama.cpp) | `llm_speed:prompt_processing`, `llm_speed:text_generation` | tokens/s | `model` (SmolLM-135M, qwen1.5-0.5b, gemma-2b, llama-7b, phi-4, Llama-3.3-70B; all Q4_K_M) × `tokens` |
| PostgreSQL | `pgbench:heavy_read_only` (`concurrency`), `pgbench:heavy_read_only:peak`, `:single` | TPM | see left |
| GPU interconnect | `nvbandwidth:all:{duplex, gpu_to_host, host_to_gpu}`, `nvbandwidth:slot:{gpu_to_host, host_to_gpu}`, `nvbandwidth:p2p:{duplex, gather, single}` (GB/s); `nvbandwidth:slot:latency` ↓, `nvbandwidth:p2p:latency` ↓ (ns); `nvbandwidth:efficiency:sm_ce_ratio` | | none |
| Compound | `workload_profile:{web, compute, cache, data_analysis, llm, cicd}` | unitless | none |

`bogomips` is in the data but is not a performance measure; it is rejected.

## Coverage by vendor

Orderable servers with any benchmark score (the population that has CPU flags,
caches and measured memory too):

| Vendor | Orderable servers | With benchmarks | With on-demand price | With spot price |
|---|---|---|---|---|
| aws | 1,402 | 84% | 99% | 98% |
| azure | 1,305 (+133 retiring) | 76% | 100% | 100% |
| gcp | 592 (+6 retiring) | 66% | 98% | 97% |
| alicloud | 1,029 | 49% | 65% | 41% |
| hcloud | 22 | 100% | 100% | — |
| ovh | 72 | 89% | 94% | — |
| upcloud | 94 | 99% | 100% | 20% |
| vultr | 131 | 95% | 100% | — |

Benchmark-specific gaps worth knowing: `pgbench` has no azure and alicloud
data; `membench` covers only 213 aws servers; GPU benchmarks cover 50 of 71 aws
GPU types and only 4 azure ones.

## Not supported (yet)

Deliberately not in v1 (see the overview plan for the rationale):

- CPU family / model regex, hyperthreading, clock speed, nested
  virtualisation (`hw_virt`)
- Memory generation, speed, ECC (mostly AWS-only data)
- Local storage size/type; network bandwidth (AWS, Alibaba and OVH data only)
- Price per GiB
- Thresholds relative to a named instance type ("≥ 1.2× `m7i.large`")
- Boot time (`average_time_to_start`)

## Troubleshooting

### The NodePool has no instance types / `Ready=False, reason=NoCandidates`

Check `status.excluded`: it counts candidates dropped per step (provider
eligibility, each filter, missing data, ranking). The most common causes are a
benchmark threshold in the wrong unit, `unbenchmarked: Exclude` on a vendor with
low coverage (alicloud, gcp), and `price.maxHourly` combined with a region list
that lacks prices.

### `Ready=False, reason=UnknownBenchmarkConfig`

The `(id, config)` pair is not in `GET /benchmark_configs`. Configs are compared
as parsed JSON, so key order does not matter, but values must match (for
example `8.0` and `8` are equal, `"8"` is not).

## FAQ

- **Why are `instance-type` lists so long?** Ranking keeps every size of a
  chosen family so Karpenter can pick the cheapest size that fits the pending
  pods, and so spot has enough diversity.
- **Why can I not prefer one type over another inside a pool?** No Karpenter
  provider lets a NodePool order types; providers choose by their own price.
  Use several `SCNodePool`s with different weights.
- **Does a stricter selector replace running nodes?** Yes, eventually: types
  removed from the list drift. Churn control delays and caps removals.

## References

- Keeper `/servers` implementation:
  [`sc_keeper/api.py`](https://github.com/SpareCores/sc-keeper/blob/7443abe9704cac0ae5b63a8a93dd93ff9acdf92e/src/sc_keeper/api.py#L414)
- Keeper benchmark subquery (exact config match; MAX over configs only when no config is given):
  [`sc_keeper/queries.py`](https://github.com/SpareCores/sc-keeper/blob/7443abe9704cac0ae5b63a8a93dd93ff9acdf92e/src/sc_keeper/queries.py#L133)
- `/benchmark_configs`:
  [`sc_keeper/api.py`](https://github.com/SpareCores/sc-keeper/blob/7443abe9704cac0ae5b63a8a93dd93ff9acdf92e/src/sc_keeper/api.py#L2312)
- Server fields and units:
  [`sc_crawler/table_bases.py`](https://github.com/SpareCores/sc-crawler/blob/85b0ef97d75db97266501e05fb1895ee5baddfa6/src/sc_crawler/table_bases.py#L632)
- Keeper API docs: https://keeper.sparecores.net/docs
- v1 overview: [`../plans/v1/wip/00-overview.md`](../plans/v1/wip/00-overview.md)
