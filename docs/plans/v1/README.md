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
| 00 | `wip/00-overview.md` | reference | features, API, selectors, provider discovery, testing strategy | — |
| 01 | `wip/01-scaffold.md` | todo | dev container, kubebuilder project, `SCNodePool` API + CRD, CI | 00 |
| 02 | `wip/02-keeper-and-selection.md` | todo | Keeper client, snapshot replay, selectors, ranking, churn | 01 |
| 03 | `wip/03-provider-adapters.md` | todo | provider adapters, eligibility, region detection, requirement rules | 02 |
| 04 | `wip/04-controller.md` | todo | reconcile, NodePool render/validate/apply, status, health, metrics | 03 |
| 05 | `wip/05-local-test-lanes.md` | todo | L2 CRD admission matrix + L3 KWOK e2e (core version matrix) in CI | 04 |
| 06 | `wip/06-aws-e2e.md` | todo | L5 AWS e2e workflow + sweeper (written and validated, not run) | 05 |
| 07 | `wip/07-packaging.md` | todo | Helm chart, release workflow, how-to guides, tutorial | 04 |
| 08 | `wip/08-provider-inprocess-tests.md` | todo | L4 in-process AWS/Azure provider tests | 04 |

Shared rules for every plan: [`CONVENTIONS.md`](CONVENTIONS.md).
