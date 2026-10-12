# 01 — Scaffold: dev container, kubebuilder project, SCNodePool API, CI

Depends on: 00 (design). Read `docs/plans/v1/CONVENTIONS.md` first.

## Goal

A buildable, testable kubebuilder project with the complete `SCNodePool` API
(types, validation, generated CRD), an inert reconciler, the dev container,
and a CI workflow running the gates on every PR.

## Scope

1. **Dev container** — `hack/devcontainer/Dockerfile` and `hack/dev.sh` as
   described in CONVENTIONS ("Dev container"). Base it on the official
   `golang` image matching `go.mod` (Go ≥ 1.25; Karpenter v1.14 requires it).
   Pin every tool version. `hack/dev.sh` must work for a non-root user (pass
   `-u $(id -u):$(id -g)`, set `HOME` and Go cache dirs inside the cache
   volume).
2. **Project** — kubebuilder v4 layout (generated inside the dev container),
   module `github.com/SpareCores/sc-karpenter-nodepool`, group
   `karpenter.sparecores.com`, version `v1alpha1`, kind `SCNodePool`,
   **cluster-scoped**, with status subresource. Remove kubebuilder sample
   scaffolding that is not used (webhooks, sample controllers).
3. **API types** (`api/v1alpha1/scnodepool_types.go`) — the full spec and
   status from overview §3.1–§3.3 and every selector in
   `docs/reference/selectors.md` (sections "The `spec.selector` and
   `spec.ranking` API" through "Data-quality policy"). Field names exactly as
   documented. Notes:
   - `nodePoolTemplate.metadata`: labels and annotations maps.
   - `nodePoolTemplate.spec`: **`runtime.RawExtension` with
     `x-kubernetes-preserve-unknown-fields`** and `x-kubernetes-embedded-resource: false`
     — deliberately untyped, so every provider's NodePool schema version
     (Auto Mode adds fields and enum values) is accepted; the controller
     validates it at runtime by server-side dry-run (plan 04). Required.
   - Quantities that are decimals in YAML (`price.maxHourly`,
     `maxHourlyPerVcpu`, GiB/GB/GHz/Gbps fields) use `resource.Quantity` or
     string-validated decimals; choose one approach consistently and document
     it in the type comments.
   - Enums via kubebuilder markers (`OnDemand|Spot`, `None|PerVcpu|PerPrice`,
     `Score|ScorePerPrice|Price`, `Exclude|IncludeIfFamilyQualifies|Include`,
     `GPU|TPU`, `Delete|Orphan`, `Shared|Burstable|Dedicated`, `amd64|arm64`).
   - CEL rules: `ranking` needs `band` or `families.max`;
     `families.min <= families.max`; a benchmark entry has `min` or `max`;
     every `*.min <= *.max` pair; `refresh.interval` ≥ 1h; `churn` values ≥ 0.
   - Defaults: `refresh.interval: 24h`, churn `5/2/3`, `dryRun: false`,
     `deletionPolicy: Delete`, `unbenchmarked: Exclude`.
   - Status: conditions (`[]metav1.Condition`), `observedGeneration`,
     `provider`, `regions`, `regionSource`, `nodePoolName`, `dataTimestamp`,
     `lastRefreshTime`, `selected` (count, band, families, instanceTypes),
     `excluded` (counters map), `pendingRemovals`.
   - Printer columns: NodePool, Provider, Types, Ready, Last refresh, Age.
4. **Reconciler stub** — `internal/controller/scnodepool_controller.go`:
   watches `SCNodePool`, sets `status.observedGeneration` and a `Ready`
   condition `Unknown` with reason `NotImplemented`. RBAC markers for
   `scnodepools` (+status) only.
5. **Makefile** — targets `generate`, `manifests`, `fmt`, `vet`, `lint`,
   `test` (unit + envtest, using setup-envtest assets baked into the image),
   `build`, `docker-build` (multi-arch capable via buildx, image
   `ghcr.io/sparecores/sc-karpenter-nodepool`), `dev-image`.
6. **Tests** — envtest suite that installs the generated CRD and checks:
   a full example `SCNodePool` (overview §3.1 plus the selectors.md example)
   is accepted; each CEL rule rejects an invalid object; defaults are applied;
   `nodePoolTemplate.spec` with unknown fields (e.g. an Auto Mode-only budget
   reason `NodeNotHealthy`) is preserved verbatim.
7. **CI** — `.github/workflows/ci.yml` on pull requests and pushes to `main`:
   build the dev image (cache it with the GitHub Actions cache), then run
   gates 1–3 inside it. Keep `refresh-sc-snapshot.yml` untouched.
8. **Examples** — `config/samples/` with the overview §3.1 object and a
   minimal one.

## Out of scope

Keeper client, selection, providers, NodePool writing (plans 02–04).

## Gates

CONVENTIONS gates 1–3, plus: `actionlint` and `shellcheck hack/*.sh` clean
(inside the dev container); `hack/dev.sh make docker-build` succeeds for the
host architecture.
