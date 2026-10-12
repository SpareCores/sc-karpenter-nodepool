# Conventions for plan-executing models

You are executing one implementation plan for **sc-karpenter-nodepool**, a
Kubernetes controller (Go, kubebuilder / controller-runtime) that turns an
`SCNodePool` into a Karpenter `NodePool` using Spare Cores data. Read this file
fully before starting. Every plan assumes you did.

## Ground rules

1. **Do NOT `git commit`, `git push`, or rewrite history.** Leave all changes in
   the working tree. A coordinator reviews and commits.
2. Do not start on a dirty tree unless the plan says so. If `git status` shows
   unrelated changes, stop and report.
3. Work until **all gates are green**. Never report success with failing or
   skipped-because-broken tests. If stuck after several distinct attempts, stop
   and write an honest report.
4. Never weaken a test to make it pass. Fixing a test's bug is fine.
5. The design is fixed by `docs/plans/v1/wip/00-overview.md` and
   `docs/reference/selectors.md`. Do not edit them. If reality contradicts
   them, implement what the plan says and record the contradiction in your
   report.
6. **No sensitive data in the repository**: no cloud account IDs, ARNs,
   cluster names, endpoints, e-mail addresses, credentials or anything taken
   from a real cluster or a developer machine. Test data comes only from the
   committed snapshot (`test/data/sc-snapshot/`) or is synthetic.
7. **Run everything in the dev container** (below). Do not install tools on
   the host. The host only needs Docker.

## Orientation (read in this order)

- `docs/plans/v1/wip/00-overview.md` — the design. Each plan names its sections.
- `docs/reference/selectors.md` — exact selector semantics.
- `docs/explanation/DECISIONS.md` — decisions (ADRs).
- `docs/plans/v1/README.md` — plan sequence and status.

## Repository layout (created by plan 01, extended by later plans)

```
api/v1alpha1/                 SCNodePool types (kubebuilder markers)
cmd/main.go                   manager entrypoint
internal/keeper/              Keeper API client (+ keepertest: snapshot replay server)
internal/selection/           candidates, selectors, ranking, churn (pure functions)
internal/providers/           provider adapters (detection, eligibility, regions, rules)
internal/nodepool/            NodePool rendering and validation
internal/controller/          reconcilers
config/                       generated CRDs, RBAC, manager manifests (kustomize)
charts/sc-karpenter-nodepool/ Helm chart (plan 07)
hack/devcontainer/            dev container image
hack/dev.sh                   run a command in the dev container
hack/sc-snapshot/fetch.py     snapshot fetcher (existing)
test/data/sc-snapshot/        committed Keeper responses (existing; refreshed only by its workflow)
test/crd/, test/kwok/         L2 / L3 lanes (plan 05)
test/e2e-aws/                 L5 scripts (plan 06)
```

Go module path: `github.com/SpareCores/sc-karpenter-nodepool`. License:
MPL-2.0 (the repository `LICENSE`); add the standard MPL-2.0 header to new Go
files.

## Dev container

`hack/devcontainer/Dockerfile` (plan 01) contains every tool: Go (the version
in `go.mod`), controller-gen, setup-envtest with envtest assets,
golangci-lint, kind, kubectl, helm, python3, actionlint, shellcheck, yq, jq.
Tool versions are pinned in the Dockerfile.

`hack/dev.sh <command...>` builds the image if needed and runs the command in
it as the calling user, with the repository mounted at `/src`, a named volume
for Go module and build caches, `--network host`, and the Docker socket mounted
(needed for kind). Examples:

```bash
hack/dev.sh make test
hack/dev.sh make lint
hack/dev.sh go test ./internal/selection/...
```

## Gates (definition of done)

Run in this order; all must pass:

1. `hack/dev.sh make generate manifests` produces no diff (generated code and
   CRDs are committed).
2. `hack/dev.sh make lint` (golangci-lint, `go vet`) — clean.
3. `hack/dev.sh make test` — unit tests and envtest suites, zero failures, no
   network access (Keeper calls go to the snapshot replay server).
4. From plan 05 on: `hack/dev.sh make test-crd` and `hack/dev.sh make test-kwok`.
5. Plan-specific gates listed in the plan.
6. `docs/plans/v1/README.md`: set the plan's status to `review`.

## Code style

- Go, controller-runtime. Small packages with pure functions where possible
  (`internal/selection`, `internal/nodepool` have no Kubernetes client).
- Comments explain non-obvious intent and trade-offs, never narrate code.
- Errors: wrap with `fmt.Errorf("...: %w", err)`; no panics in the manager.
- Tests: table-driven, co-located `_test.go`; envtest for API/reconciler
  behaviour; golden files under `testdata/` where output is large.
- Kubernetes objects of other projects (NodePool, NodeClaim, NodeClass) are
  handled as `unstructured.Unstructured`, so the controller works with every
  provider's CRD version. Karpenter's Go module (`sigs.k8s.io/karpenter`,
  v1.14.x) may be imported for constants and validation helpers (label rules,
  requirement compatibility), not for typed NodePool round-trips.
- Configuration: flags with env var fallbacks (`SCKN_*`), documented where
  read.

## Reporting back

End with a concise report: files added or changed; design decisions where the
plan was ambiguous; exact gate results (command + pass/fail + test counts);
anything deliberately deferred, one line each.
