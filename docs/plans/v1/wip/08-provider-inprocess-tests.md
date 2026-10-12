# 08 — In-process provider tests (L4: AWS, Azure)

Depends on: 04. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §8.4.

## Goal

Prove, against the real provider code, that every requirement key we emit is
stamped on NodeClaims (no drift loop) and that the providers launch only from
our instance-type lists.

## Scope

- A separate Go module `test/l4/` (own `go.mod`, so the provider dependency
  trees never enter the main module), importing
  `github.com/aws/karpenter-provider-aws` and
  `github.com/Azure/karpenter-provider-azure` at the versions pinned in
  overview §5.2 and their `pkg/test` environments with in-memory cloud fakes.
- For each provider: render NodePools with our `internal/nodepool` (import the
  main module via a `replace` to the repository root) for representative
  `SCNodePool`s; run the provider's `GetInstanceTypes` and `Create` against
  them; assert (1) launched types ⊆ our list, (2) every NodePool requirement
  key is present on the returned NodeClaim labels, (3) Auto Mode-style and
  provider-specific restrictions hold.
- `make test-l4`; CI job on a nightly schedule and `workflow_dispatch`.

## Gates

`hack/dev.sh make test-l4` green; CONVENTIONS gates 1–3 unaffected.
