# Implementation plans (v1)

Sequenced, self-contained work orders for sc-karpenter-nodepool v1. The base
document is [`wip/00-overview.md`](wip/00-overview.md): features, the
`SCNodePool` API, supported selectors, provider discovery and the test
strategy. Every later plan assumes it.

Layout:

- [`done/`](done/) — completed plans
- [`wip/`](wip/) — plans in progress or not yet started

Execution protocol:

1. Hand the model exactly one plan file (plus the overview and, once it exists,
   `CONVENTIONS.md`).
2. The model implements until all gates of the plan are green, does NOT
   commit, and writes a report.
3. A coordinator (separate session) reviews the diff, re-runs the gates
   independently, and commits.
4. Only then does the next plan start — plans assume all predecessors are
   committed.
5. When a plan finishes, move it from `wip/` to `done/`.

| # | file | status | milestone | depends on |
|---|---|---|---|---|
| 00 | `wip/00-overview.md` | review | features, API, selectors, provider discovery, testing strategy | — |
| 01 | — | not written | project scaffold, CRD types, conventions | 00 |
| 02 | — | not written | AWS e2e workflow + sweeper | 01 |
| 03 | — | not written | Keeper client, selector engine, ranking, churn | 01 |
| 04 | — | not written | provider adapters | 03 |
| 05 | — | not written | controller: reconcile, apply, status, health, metrics | 04 |
| 06 | — | not written | CRD matrix + KWOK e2e lanes in CI | 05 |
| 07 | — | not written | packaging, release, how-to guides | 05 |
| 08 | — | not written | in-process provider tests (AWS, Azure) | 05 |
