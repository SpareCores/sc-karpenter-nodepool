# 02 — Keeper client and selection engine

Depends on: 01. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §3.4 steps 3–6, §3.5, §7; `docs/reference/selectors.md` (all).

## Goal

Pure, fully unit-tested code that turns an `SCNodePool` spec plus Keeper data
into a selection (instance types, families, band report, exclusion counters),
including churn control. No Kubernetes client in this plan.

## Scope

### `internal/keeper`

- `Client` with `Servers(ctx, Query) ([]Server, error)`,
  `BenchmarkConfigs(ctx)`, `Regions(ctx)`, `Zones(ctx)` (Keeper
  `/table/region`, `/table/zone`), `Health(ctx)` (`/healthcheck`: database hash
  and update time).
- `Query` holds vendor, `vendor_regions` (chunked by 3 automatically and
  merged), `best_price_allocation`, optional benchmark id + canonical config
  string, and the push-down parameters of overview §7.1 (only those; looser or
  equal to the client-side checks). `limit=-1`, default status filter.
- `Server` decodes the sc-crawler 0.10 schema (`accelerator_*`,
  `accelerator_type`, `series`, …), ignores unknown fields.
- Retries with backoff; HTTP 429 honours `Retry-After`; optional API key
  header; per-refresh request de-duplication (memoise identical queries within
  one `Fetcher` instance).
- Canonical benchmark config lookup: match the user's config object against
  `/benchmark_configs` by parsed value; unknown → typed error
  `ErrUnknownBenchmarkConfig`. Require a config when the benchmark has more than
  one.
- `keepertest` package: an `httptest` server that replays
  `test/data/sc-snapshot/` using `manifest.json` (match on path + vendor +
  vendor_regions + allocation + benchmark id/config; ignore push-down
  parameters, since the client re-applies every filter). Unknown queries →
  HTTP 404 with a clear message.

### `internal/selection`

- `Candidate` built from `Server` rows of all queries joined on
  `api_reference`: specs, prices (on-demand / spot for the regions), and
  scores per (benchmark id, config).
- Derived values: accelerator type derivation rule (selectors.md
  "Accelerators"), hyperthreading (`vcpus > cpu_cores`), memory GiB,
  memory per vCPU, price per vCPU, normalisations (`PerVcpu`, `PerPrice` with
  an optional per-type managed fee supplied by the caller).
- Filters for every selector in selectors.md, AND-ed; missing value →
  excluded and counted under `missingData` (benchmarks follow `unbenchmarked`).
  Exact semantics as documented (globs, inclusive bounds, `countMin` rule,
  architectures mapping, lower-is-better `min`/`max` validation).
- Ranking: family grouping, median objective, score band, `families.min` /
  `.max`, band report (reference, lowest kept, delta %, widened, trimmed).
- `Evaluate(spec, candidates, opts) Result` where `opts` carries
  provider-supplied pieces from plan 03 as plain inputs (eligibility predicate,
  managed-fee map, default price basis, architecture constraint from the
  template). Result: sorted instance types, families with ranks, exclusion
  counters per step (`providerIneligible`, `templateConstraints`,
  `filters.<name>`, `missingData`, `ranking`).
- `churn.Apply(prev State, next Result, cfg, specChanged bool) (types, State)`
  implementing overview §3.5 (removal margin needs, per type, the worst
  relative miss of a failing threshold — return it from `Evaluate`). State is
  serialisable into `status.pendingRemovals`.

## Tests (all offline, against `keepertest` and the committed snapshot)

- Client: chunking, retries, `Retry-After`, canonical config, unknown config.
- One table test per selector (positive, negative, missing data), on real
  snapshot rows for at least aws, azure, gcp and hcloud.
- Ranking: the selectors.md worked example ("fastest single-core, ≤ $0.50/h,
  band 12%, families 3–6") on the aws snapshot — assert properties (every kept
  family within the band or added by `min`; no family above `max`; all
  qualifying sizes kept), not exact names, so a snapshot refresh does not break
  it. Band widening and trimming cases.
- Churn: additions immediate; removal after N refreshes; margin; cap;
  spec change bypasses hysteresis but not the cap.

## Gates

CONVENTIONS gates 1–3. Coverage of `internal/selection` ≥ 85% (report it).
