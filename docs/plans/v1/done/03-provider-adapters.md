# 03 — Provider adapters

Depends on: 02. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §5 (facts), §6.1–§6.5 (rules), §3.4 step 7.

## Goal

One adapter per row of overview §6.1, as data plus small functions, and the
region detection of §6.3. Pure logic plus a thin reader over the Kubernetes
API (unstructured objects); no NodePool writing yet.

## Scope

### `internal/providers`

- `Adapter` interface (names are suggestions):
  - `ID()`, `Vendor()` (Keeper vendor id), `Matches(group, kind, servedVersion)`.
  - `Eligible(candidate) (bool, reason)` — overview §6.2.
  - `RegionsFromNodeClass(nodeClass *unstructured.Unstructured, zones ZoneIndex) ([]string, bool)` — §6.3 table.
  - `ToKeeperRegion(k8sRegion, regions RegionIndex) (regionID, error)` — §6.3 mapping table.
  - `RequirementRules()` — default capacity type, whether spot is allowed,
    whether both capacity types may be mixed, single-architecture constraint,
    forced zones (OVH), keys that must never be emitted (`kubernetes.io/os`
    on Auto Mode), §6.4–§6.5.
  - `ManagedFee(region) map[instanceType]float64` (Auto Mode only).
- Registry: detection by `nodeClassRef.group` + kind; UpCloud: refuse the
  served version `v1alpha2` (upcloud-tools); unknown → `ErrUnsupportedProvider`.
- Auto Mode eligibility and fees: download the AWS public price list
  (`https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEKS/current/<region>/index.json`),
  extract `eksproducttype=AutoMode` products (instance type → hourly fee),
  cache per refresh. Extend `hack/sc-snapshot/fetch.py` to also store the
  extracted `{instanceType: fee}` map for the aws test region (as
  `aws/eks-automode-fees.json.gz`, recorded in the manifest), and add a
  replay for it in `keepertest` (or a sibling test server). Do not commit a
  refreshed snapshot yourself: run the fetcher only into a temporary
  directory to test it; the coordinator refreshes the committed snapshot via
  the workflow.
- Azure static exclusions (§5.4 Azure list), Vultr `vbm-*`/`vcg-*`, OVH
  GPU/`bm-*`/`d2-2`, itsh/UpCloud/OVH/Vultr amd64-only, GCP TPU and
  unpriced types.

### Region detection (`internal/providers/regions.go` or similar)

- Implements overview §6.3 in full: NodeClass → NodeClaims of that NodeClass
  (label `<group>/<lowercase kind>`) → nodes (of that NodeClass, else all);
  zone → region via the SC zone table; result + source; conflict detection
  (NodeClass wins, else majority) producing a list of seen values for the
  `RegionConflict` condition; `RegionPending` when nothing is found;
  `RegionUnknown` when the region is not in Spare Cores.
- Reads through a minimal interface (list NodeClaims/Nodes by label, get the
  NodeClass) so it is unit-testable with fake objects.

## Tests

- Detection per adapter, including UpCloud version refusal and unknown groups.
- Eligibility per adapter on real snapshot rows (counts plausible, e.g. Azure
  drops constrained `-N` sizes and `DC*`/`EC*`; Auto Mode keeps only types
  with a fee entry, none with < 2 vCPUs).
- Region detection: one fake NodeClass per adapter using the field paths of
  §6.3 (values synthetic, e.g. `us-east-1a`/`use1-az1`, `fsn1`, `ewr`);
  NodeClaim and node fallbacks; conflict and pending cases; mapping to Keeper
  region ids including GCP's numeric ids and Hetzner aliases (from the
  snapshot's region/zone tables).
- Requirement rules table test (capacity defaults, forced OVH zones, Auto Mode
  never emits `kubernetes.io/os`, single-arch for cloudpilot Alibaba, single
  capacity type for official Alibaba).

## Gates

CONVENTIONS gates 1–3; `python3 hack/sc-snapshot/fetch.py --out /tmp/x` works
inside the dev container (network allowed for this one manual check only).
