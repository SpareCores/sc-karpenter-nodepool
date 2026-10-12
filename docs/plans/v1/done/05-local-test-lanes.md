# 05 — Local test lanes: CRD admission matrix (L2) and KWOK e2e (L3)

Depends on: 04. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §8.1–§8.3, §8.6, §5.1.5, §5.2.

## Goal

`make test-crd` and `make test-kwok` run locally (dev container + kind) and in
CI on every PR.

## Scope

### L2 — `test/crd/`, `make test-crd`

- `test/crd/providers.yaml`: provider → release tag → URL of its shipped
  `karpenter.sh_nodepools.yaml` (AWS, Azure, cloudpilot GCP, both Alibaba,
  stubbi/echohello/itsh Hetzner, kubekanvas UpCloud, OVH, Vultr; Vultr and OVH
  ship none — use core's CRD of the version they pin). Cache downloads under
  `test/crd/.cache/` (git-ignored).
- An EKS Auto Mode fixture may be added later by the coordinator; the lane
  must pick up any `test/crd/fixtures/*/karpenter.sh_nodepools.yaml` present.
- For each CRD: install into one kind cluster (replace between providers),
  then `kubectl apply --dry-run=server` (1) the golden NodePools rendered by
  plan 04 for that provider and (2) the negative cases of overview §8.3
  (restricted keys, empty `In`, >100 requirements, bad `minValues`), asserting
  accept/reject per provider. Fast: one cluster per run.

### L3 — `test/kwok/`, `make test-kwok`

- Build the KWOK provider from a Karpenter core tag
  (`CGO_ENABLED=0 go build ./kwok` in a clone of `kubernetes-sigs/karpenter`),
  `FROM scratch` image, `kind load`. Core versions as a matrix: `v1.2.x`,
  `v1.8.x`, `v1.12.x`, `v1.14.x` (latest patch of each; variable
  `KWOK_CORE_VERSION`). Install kwok-controller v0.8.0 (`kwok.yaml`,
  `stage-fast.yaml`, toleration for `CriticalAddonsOnly`) and the kwok chart
  with per-version values in `test/kwok/values/<version>.yaml` (feature gates
  differ; core HEAD's chart needs all five gates set).
- Catalog generator `test/kwok/catalog/` (Go): from the committed snapshot,
  one KWOK instance-type file per vendor, with offerings per zone of the test
  region × capacity type, the provider's label keys/value formats (overview
  §5.3/§5.4) and sanitised label values.
- Run our controller (built image) with `--node-class-ref-override` pointing
  at a `KWOKNodeClass`, Keeper replaced by the `keepertest` replay server
  (run it as a small binary/container).
- Scenarios per vendor (assert relative properties, never fixed names):
  subset honoured (every NodeClaim's instance type, zone, capacity type and
  arch is allowed); narrowing → `RequirementsDrifted` → converged; unknown
  instance-type names → pods Pending and our `NodePoolSchedulable=False`;
  drift-loop negative test (multi-valued requirement on an unstamped
  well-known key → our detector fires); custom-key positive test (stable).
  Verify early whether core v1.2/v1.8 also register offering keys as
  well-known; adapt or skip affected scenarios per version with a recorded
  reason.
- Upload logs and generated catalogs as CI artifacts on failure.

### CI

Extend `.github/workflows/ci.yml`: an L2 job, and an L3 matrix job over the
four core versions (parallel), both inside the dev container.

## Gates

CONVENTIONS gates 1–4. Report per-version L3 runtimes.
