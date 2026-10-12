# 07 — Packaging, release, user docs

Depends on: 04 (independent of 05/06). Read `docs/plans/v1/CONVENTIONS.md` first.

## Scope

- Helm chart `charts/sc-karpenter-nodepool/`: CRD (from `config/crd`, kept in
  sync by a `make chart-crds` target and a CI check), Deployment
  (non-root, read-only rootfs, resources, probes, priority class optional),
  ServiceAccount, RBAC, metrics Service + optional ServiceMonitor, values for
  every manager flag, Keeper API key from an existing Secret, tolerations /
  nodeSelector / affinity (the controller must be schedulable outside the
  pools it generates). `helm lint` and `helm template` tests (golden output).
- Release workflow `.github/workflows/release.yml` on `v*` tags: multi-arch
  image (amd64, arm64) to `ghcr.io/sparecores/sc-karpenter-nodepool`, chart
  pushed as OCI to `ghcr.io/sparecores/charts`, GitHub release with notes.
  Image and chart versions follow the tag.
- Docs (Diátaxis):
  - `docs/how-to-guides/install.md` (Helm, per provider notes: Auto Mode,
    NAP, self-managed; RBAC for NodeClass groups),
  - `docs/how-to-guides/write-an-scnodepool.md` (common recipes: fastest
    single-core with a band, cheapest throughput per vCPU, no SMT, GPU memory),
  - `docs/how-to-guides/testing.md` (dev container, gates, L2/L3/L5, snapshot
    refresh),
  - `docs/reference/api.md` (generated from the CRD or written; every field),
  - `docs/tutorials/kind-quickstart.md` (kind + KWOK + snapshot replay),
  - update the README files of the quadrants and the repository `README.md`.

## Gates

CONVENTIONS gates 1–3; `helm lint`; chart golden tests; `actionlint`; docs
links resolve (simple script).
