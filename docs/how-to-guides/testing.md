# Test and develop

Everything runs in the dev container, so the only tool you need on your machine
is Docker. This guide covers the container, the gates every change must pass,
the test lanes, and refreshing the test data. The strategy behind the lanes is in
the [v1 overview](../plans/v1/wip/00-overview.md#8-testing).

## The dev container

`hack/devcontainer/Dockerfile` pins every tool: Go, controller-gen, envtest
(kube-apiserver and etcd binaries), golangci-lint, kind, kubectl, helm, python3,
actionlint, shellcheck, yq and jq. `hack/dev.sh <command...>` builds the image on
first use (and again when the Dockerfile changes) and runs the command in it as
you, with the repository at `/src`, a named volume for Go caches, host networking
and the Docker socket (kind needs it):

```bash
hack/dev.sh make test
hack/dev.sh go test ./internal/selection/...
hack/dev.sh bash                      # a shell in the container
```

Never install the tools on the host; versions are bumped in the Dockerfile only.
Settings of the lanes pass through from your environment when set:
`KWOK_CORE_VERSION`, `KWOK_VENDORS`, `KWOK_SETUP_ONLY`, `KEEP_CLUSTER`,
`REUSE_CLUSTER`, `KIND_CLUSTER_NAME`. `make help` lists all targets.

## The gates

Run in this order before you propose a change; CI (`.github/workflows/ci.yml`)
runs the same:

| # | Command | What it checks |
|---|---|---|
| 1 | `hack/dev.sh make generate manifests chart-crds chart-rbac docs-api` | Generated code, the CRD, RBAC, the chart copies of CRD and RBAC and `docs/reference/api.md` are up to date: afterwards `git status` shows no change you did not make. |
| 2 | `hack/dev.sh make lint` | `go vet` (also of the lane code) and golangci-lint. |
| 3 | `hack/dev.sh make test` | Unit tests and the envtest suites (a real kube-apiserver and etcd, no cluster); no network access: Keeper calls go to the snapshot replay server. |
| 4 | `hack/dev.sh make test-crd` | L2: every provider's NodePool CRD against the generated NodePools. |
| 5 | `hack/dev.sh make test-kwok` | L3: real core Karpenter in kind. |
| 6 | `hack/dev.sh make test-chart` | Helm chart: `helm lint --strict`, the values schema, golden `helm template` output, the chart's CRD and RBAC equal to `config/`, every manager flag settable from the chart. |
| 7 | `hack/dev.sh make test-docs` | Relative links and anchors of all Markdown files resolve. |
| 8 | `hack/dev.sh make test-e2e-aws` | The configuration of the real-AWS lane (L5): the eksctl configs and manifests rendered with fake values and validated offline, no AWS access. |
| 9 | `hack/dev.sh bash -c 'actionlint && shellcheck -x hack/*.sh test/*.sh test/*/*.sh'` | Workflows and shell scripts. |

Tests are table-driven and sit next to the code; large outputs are golden files
under `testdata/` (`go test ./internal/nodepool -update` rewrites the NodePool
goldens, `hack/dev.sh make test-chart UPDATE=1` the chart goldens: review the
diff before you keep it).

## The test layers

| Layer | Command | Runs | What it proves |
|---|---|---|---|
| L1 unit and envtest | `make test` | every PR | Selection, ranking, churn, requirement building, adapters and the reconciler, against the committed Spare Cores snapshot replayed through a fake Keeper. |
| L2 CRD matrix | `make test-crd` | every PR | Server-side dry-run of the generated NodePools (and negative cases) against the NodePool CRD each supported provider ships, one kind cluster, under a minute plus downloads. Logs in `test/crd/.out/`. |
| L3 KWOK | `make test-kwok` | every PR, once per core version | Real scheduling, provisioning, drift and the drift-loop detector with core Karpenter's KWOK provider and per-vendor catalogs built from the snapshot, in kind. About 10 minutes per core version. Logs and catalogs in `test/kwok/.out/<version>/`. |
| L4 provider tests | `make test-l4` | nightly (GitHub workflow), and on demand | The real AWS and Azure provider code launches only from our instance-type lists and stamps every requirement key we emit on its NodeClaims (no drift loop). In-process, with the providers' in-memory cloud fakes and envtest, under a minute plus module downloads. Log in `test/l4/.out/`. See [L4 in detail](#l4-in-detail). |
| L5 real AWS | manual (GitHub workflow) | | The controller on EKS Auto Mode and on EKS with self-managed Karpenter in a real account: selection, provisioning and drift. About an hour and $0.6-0.8 per run. See [L5 in detail](#l5-in-detail). |

### L3 in detail

```bash
hack/dev.sh make test-kwok                       # core v1.14
hack/dev.sh make test-kwok KWOK_CORE_VERSION=v1.8
hack/dev.sh make test-kwok-matrix                # v1.2, v1.8, v1.12, v1.14 in turn
KWOK_VENDORS=aws,hcloud hack/dev.sh make test-kwok
KEEP_CLUSTER=1 hack/dev.sh make test-kwok        # keep the cluster for poking around
```

The lane builds the controller, a KWOK build of the chosen core Karpenter tag
(cloned into `test/kwok/.cache/`) and the Keeper replay server, creates a kind
cluster, deploys the controller through the kustomize manifests of
`config/default` (with the test-mode `--node-class-ref-override`), and runs
`test/kwok/e2e`. The core versions are listed in `test/kwok/core-versions.txt`.
Downloads and builds are cached in `test/kwok/.cache/` (git-ignored).

`make kwok-up` runs the same set-up and stops with a working cluster, which
the [kind quickstart](../tutorials/kind-quickstart.md) uses; `make kwok-down` deletes
it. For debugging a failing scenario, `KEEP_CLUSTER=1` plus
`kubectl --kubeconfig test/kwok/.cache/sckn-kwok.kubeconfig` (inside
`hack/dev.sh`) is the quickest way in; `REUSE_CLUSTER=1` reuses it on the next run.

On hosts whose Docker root is on ZFS, kubelet cannot start in a kind node; the
lanes detect that and mount the node's data directories from tmpfs.

### L4 in detail

```bash
hack/dev.sh make test-l4
L4_LOG=1 hack/dev.sh bash -c 'cd test/l4 && go test -count=1 -v -run TestDriftControls ./azure/'
```

`test/l4` is a Go module of its own (it imports `aws/karpenter-provider-aws`
v1.14.1 and `Azure/karpenter-provider-azure` v1.14.3, and the controller's
packages through a `replace` to the repository root), so the providers'
dependency trees stay out of `go.mod`. Each provider package starts the
provider's own `pkg/test` environment: an envtest API server with the
provider's CRDs, in-memory EC2/SSM/IAM/pricing (AWS) or Azure API fakes, and the
real `CloudProvider`. For synthetic `SCNodePool`s over instance types of the
fakes' catalogs, the tests render the NodePool with `internal/nodepool` and the
adapter's rules, apply it server-side, run core's NodePool hash, validation and
readiness controllers, schedule pods with core's provisioner, and check that:

- the types the provider asks the cloud for (EC2 `CreateFleet` overrides,
  Azure VM sizes) and the launched NodeClaims are within the NodePool's list;
- every requirement key of the NodePool is a label of each NodeClaim, core's
  requirement-drift check passes, and core's drift controller (including the
  provider's `IsDrifted`) finds nothing; removing the launched type does drift
  it (a positive control), adding a type or refreshing the data does not;
- provider restrictions match the adapters: capacity types and architectures
  of the offerings (no `reserved` on Azure), AWS's exotic-type filter, zone
  pinning by zone ID (AWS) and zone (both), the EKS Auto Mode adapter's keys on
  the open-source provider, and Azure's static SKU exclusions against the
  adapter's eligibility filter, SKU by SKU.

Nothing reaches a cloud: the test binaries drop cloud credentials from the
environment, disable the AWS instance metadata service, and route every
non-local HTTP request to a local proxy that refuses it and fails the run.
`L4_LOG=1` turns on the controllers' logs.

### L5 in detail

L5 runs only in GitHub Actions, in the `aws-e2e` environment (deployments from
`main`; its repository variables name the account's VPC, subnets and the
pre-created IAM roles), and costs money. Nothing in this repository, and no
command of this guide, calls AWS from your machine. The workflow
`.github/workflows/e2e-aws.yml` (manual trigger; inputs `targets`:
`both|auto|karpenter`, and `capacityType`: `on-demand|spot`):

1. builds the controller image of the commit (`sha-<commit>`, linux/amd64 and
   arm64) and pushes it to the public package on ghcr;
2. per target, in parallel: removes leftovers of a previous run, creates the
   cluster with the pinned eksctl (`test/e2e-aws/eksctl-auto.yaml`,
   `eksctl-karpenter.yaml`, rendered with `envsubst` from the repository
   variables), sets up Auto Mode's custom NodeClass `sckn-ci` and an
   infrastructure NodePool, or Karpenter v1.14.1 with Pod Identity and an
   `EC2NodeClass`, and installs the controller from the Helm chart;
3. `test-select.sh` applies an SCNodePool that selects eight Graviton types
   (`test/e2e-aws/expected-instance-types.txt`), checks the NodePool's list and
   the nodes Karpenter launches for three 900m pods that repel each other;
   `test-drift.sh` excludes the types in use and expects the NodePool update,
   `Drifted=True` and convergence within 15 minutes;
4. always: diagnostics on failure (redacted, uploaded as an artifact), then
   `teardown.sh` deletes the workload, the pools and the cluster, and removes
   orphaned instances, launch templates and instance profiles.

`.github/workflows/e2e-aws-sweeper.yml` (`workflow_dispatch`; its three-hourly
schedule is commented out until the maintainer enables it) deletes `sckn-ci-*`
clusters older than three hours and their orphans.

What you can check offline, in the dev container, is the lane's configuration:
`hack/dev.sh make test-e2e-aws` renders the templates with fake values,
validates the eksctl configs against eksctl's JSON schema of the pinned release
(downloaded from GitHub with a checksum, cached in `test/e2e-aws/.cache/`) and
the SCNodePool and NodePool against their CRDs, and checks the names and roles
the scripts rely on. `go test ./internal/providers -run TestE2EAWSSelection`
checks, against the committed snapshot, that the e2e SCNodePool selects exactly
the expected list for both AWS adapters.

## Develop and try changes

```bash
hack/dev.sh make build                 # bin/manager
hack/dev.sh make docker-build          # image ghcr.io/sparecores/sc-karpenter-nodepool:dev
hack/dev.sh make kwok-up               # a cluster with your checkout running
```

To lint and render the chart while you edit it:
`hack/dev.sh helm lint charts/sc-karpenter-nodepool` and
`hack/dev.sh helm template x charts/sc-karpenter-nodepool`. The chart's CRD and
RBAC rules are copies: change the Go markers, run `make manifests chart-crds
chart-rbac`, never edit `templates/crd.yaml` or `files/manager-rules.yaml`.
`docs/reference/api.md` is generated from the CRD (`make docs-api`).

## Refresh the Spare Cores snapshot

All lanes read `test/data/sc-snapshot/`, a fixed set of raw Keeper responses
(`manifest.json` records what, when and from which Keeper database). It changes
only on purpose:

- on GitHub: run the workflow **Refresh Spare Cores snapshot** (Actions, manual
  trigger). It runs `hack/sc-snapshot/fetch.py`, replaces the directory, and
  commits it to the branch it ran on if any data changed;
- locally: `hack/dev.sh python3 hack/sc-snapshot/fetch.py --out test/data/sc-snapshot`
  (needs network access to Keeper and the AWS price list).

After a refresh, run the gates again: golden files and the numbers in the docs
(the recipes in [write an SCNodePool](write-an-scnodepool.md), the
[tutorial](../tutorials/kind-quickstart.md)) are derived from the snapshot and may
change.

## Release

A tag `v<major>.<minor>.<patch>[-<prerelease>]` triggers
`.github/workflows/release.yml`: it re-runs the gates, builds the multi-arch
(amd64, arm64) image to `ghcr.io/sparecores/sc-karpenter-nodepool`, packages the
chart with the tag as chart and app version and pushes it to
`oci://ghcr.io/sparecores/charts`, and creates the GitHub release with generated
notes. Image tags and chart versions carry no leading `v`. Pre-release tags create
a pre-release and do not move `latest`. To try the image build locally:
`hack/dev.sh make docker-buildx IMG=<registry>/<name>:<tag>` pushes a multi-arch
image to a registry of your choice.
