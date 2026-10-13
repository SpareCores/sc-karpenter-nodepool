# Controller reference

How the controller is configured and what it reports: manager flags and
environment variables, the Helm chart values, the objects it creates, the
status conditions and Events, and the metrics. The `SCNodePool` fields are in the
[API reference](api.md).

## Table of Contents

- [Manager flags](#manager-flags)
- [Helm chart values](#helm-chart-values)
- [What the controller creates](#what-the-controller-creates)
- [Status conditions](#status-conditions)
- [Ready reasons](#ready-reasons)
- [Events](#events)
- [Metrics](#metrics)
- [Health endpoints](#health-endpoints)
- [Permissions](#permissions)

## Manager flags

The binary is `/manager`. Every flag falls back to an environment variable
(`SCKN_*`) when it is not given; an explicit flag wins. An unparsable value of a
variable is ignored and the default applies. The Helm chart sets the flags
(table below).

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `--metrics-bind-address` | `SCKN_METRICS_BIND_ADDRESS` | `0` (off) | Address of the metrics endpoint, for example `:8443`. |
| `--metrics-secure` | `SCKN_METRICS_SECURE` | `true` | Serve metrics over HTTPS with authentication and authorisation (self-signed certificate); `false` serves plain HTTP. |
| `--enable-http2` | `SCKN_ENABLE_HTTP2` | `false` | HTTP/2 on the metrics server (off because of the Rapid Reset CVEs). |
| `--health-probe-bind-address` | `SCKN_HEALTH_PROBE_BIND_ADDRESS` | `:8081` | Address of `/healthz` and `/readyz`. |
| `--leader-elect` | `SCKN_LEADER_ELECT` | `false` | Leader election (a Lease named `sc-karpenter-nodepool.sparecores.com` in the controller's namespace); only the leader reconciles. |
| `--keeper-url` | `SCKN_KEEPER_URL` | `https://keeper.sparecores.net` | Base URL of the Spare Cores Keeper API. |
| `--keeper-api-key-file` | `SCKN_KEEPER_API_KEY_FILE` | none | File with the Keeper API key (for example a mounted Secret). Without it, the key is read from `SCKN_KEEPER_API_KEY`; without that, access is anonymous. |
| `--keeper-timeout` | `SCKN_KEEPER_TIMEOUT` | `2m` | Timeout of one Keeper HTTP request. |
| `--keeper-regions-per-request` | `SCKN_KEEPER_REGIONS_PER_REQUEST` | `-1` | Regions per `/servers` request: `-1` means 3 without an API key (Keeper's limit) and unlimited with one. |
| `--data-cache-ttl` | `SCKN_DATA_CACHE_TTL` | `10m` | How long Keeper responses and EKS Auto Mode fees are shared between the refreshes of different SCNodePools. |
| `--aws-price-list-url` | `SCKN_AWS_PRICE_LIST_URL` | `https://pricing.us-east-1.amazonaws.com` | AWS public price list (EKS Auto Mode instance list and fees). |
| `--max-concurrent-reconciles` | `SCKN_MAX_CONCURRENT_RECONCILES` | `4` | SCNodePools refreshed concurrently. |
| `--nodeclass-discovery-interval` | `SCKN_NODECLASS_DISCOVERY_INTERVAL` | `5m` | How often to look for NodeClass CRDs installed after the start, to watch them. |
| `--node-class-ref-override` | `SCKN_NODE_CLASS_REF_OVERRIDE` | none | **Test mode only.** `group/kind/name` that replaces the `nodeClassRef` of every generated NodePool; the provider adapter is still chosen from the template. Used by the KWOK lane. |
| `--zap-devel`, `--zap-encoder`, `--zap-log-level`, `--zap-stacktrace-level`, `--zap-time-encoding` | none | development mode on | Logging, from controller-runtime's zap options. The binary defaults to development mode (console output); the chart turns it off. |

`--version` prints the version (from the git tag the image was built from),
the commit and the Go version, then exits. The same values are logged at
start-up and exported as `sc_karpenter_nodepool_build_info`. Development
builds report `git describe` output (for example `0.1.0-3-gabc1234-dirty`) or
`dev`.

There is no flag to force the detected region or zone. Pin zones with a
`topology.kubernetes.io/zone` requirement in the template.

## Helm chart values

`charts/sc-karpenter-nodepool/values.yaml` documents every value; a JSON schema
(`values.schema.json`) rejects unknown keys and malformed values at
`helm install`.

| Value | Default | Sets |
|---|---|---|
| `replicaCount` | `1` | Replicas of the Deployment |
| `image.repository`, `image.tag`, `image.digest`, `image.pullPolicy`, `imagePullSecrets` | `ghcr.io/sparecores/sc-karpenter-nodepool`, chart `appVersion`, none, `IfNotPresent` | The image; a digest wins over the tag |
| `serviceAccount.create`, `.name`, `.annotations`, `.automount` | `true`, release name | The ServiceAccount |
| `leaderElection.enabled` | `true` | `--leader-elect` |
| `keeper.url` | `https://keeper.sparecores.net` | `--keeper-url` |
| `keeper.timeout` | `2m` | `--keeper-timeout` |
| `keeper.regionsPerRequest` | `-1` | `--keeper-regions-per-request` |
| `keeper.apiKey.existingSecret`, `keeper.apiKey.key` | none, `api-key` | Mounts the Secret and sets `--keeper-api-key-file` |
| `dataCacheTTL` | `10m` | `--data-cache-ttl` |
| `awsPriceListURL` | `https://pricing.us-east-1.amazonaws.com` | `--aws-price-list-url` |
| `maxConcurrentReconciles` | `4` | `--max-concurrent-reconciles` |
| `nodeClassDiscoveryInterval` | `5m` | `--nodeclass-discovery-interval` |
| `nodeClassRefOverride` | none | `--node-class-ref-override` (test mode only) |
| `logging.level`, `.encoder`, `.development`, `.stacktraceLevel`, `.timeEncoding` | `info`, `json`, `false`, `error`, `iso8601` | The `--zap-*` flags |
| `health.port` | `8081` | `--health-probe-bind-address` and the probes |
| `metrics.enabled`, `metrics.port` | `true`, `8443` | `--metrics-bind-address` (`0` when disabled), the Service |
| `metrics.secure` | `true` | `--metrics-secure`, the authn/authz RBAC |
| `metrics.enableHTTP2` | `false` | `--enable-http2` |
| `metrics.service.type`, `.annotations` | `ClusterIP` | The metrics Service |
| `metrics.readers` | `[]` | Subjects bound to the `/metrics` reader ClusterRole |
| `serviceMonitor.enabled`, `.namespace`, `.interval`, `.scrapeTimeout`, `.labels`, `.insecureSkipVerify` | `false`, release namespace, `30s`, none, none, `true` | A Prometheus Operator ServiceMonitor |
| `extraArgs`, `extraEnv`, `extraVolumes`, `extraVolumeMounts` | empty | Appended to the manager container and pod |
| `rbac.create` | `true` | The ClusterRole, ClusterRoleBinding and leader-election Role |
| `rbac.nodeClassGroups` | empty (all) | Restricts the NodeClass rules to these API groups |
| `rbac.userRoles.create` | `true` | The admin, editor and viewer ClusterRoles for SCNodePool users |
| `rbac.extraRules` | empty | Extra rules for the manager ClusterRole |
| `crds.install`, `crds.keep` | `true`, `true` | Installs the CRD; keeps it on `helm uninstall` |
| `podAnnotations`, `podLabels`, `deploymentAnnotations`, `priorityClassName` | empty | Pod metadata and priority |
| `podSecurityContext`, `securityContext` | non-root, `RuntimeDefault` seccomp; read-only root file system, no privilege escalation, all capabilities dropped | Security contexts |
| `resources` | requests 50m CPU, 128Mi; limit 512Mi memory | Resources of the manager |
| `livenessProbe`, `readinessProbe`, `terminationGracePeriodSeconds` | `/healthz`, `/readyz`, `10` | Probes and shutdown |
| `affinity`, `nodeSelector`, `tolerations`, `topologySpreadConstraints` | prefer nodes without `karpenter.sh/nodepool`; `kubernetes.io/os: linux`; none; none | Scheduling |
| `podDisruptionBudget.enabled`, `.minAvailable` | `false`, `1` | A PodDisruptionBudget |
| `nameOverride`, `fullnameOverride` | empty | Names |

The chart's CRD (`templates/crd.yaml`) and the manager rules
(`files/manager-rules.yaml`) are generated from `config/crd` and
`config/rbac/role.yaml` (`make chart-crds chart-rbac`); CI fails when they drift.

## What the controller creates

For each `SCNodePool` `<name>`, one `karpenter.sh/v1` NodePool `<name>`, applied
with server-side apply (field manager `sc-karpenter-nodepool`):

| Item | Value |
|---|---|
| Labels | `app.kubernetes.io/managed-by: sc-karpenter-nodepool`, `karpenter.sparecores.com/scnodepool: <name>`, plus the template's labels |
| Annotations | `karpenter.sparecores.com/data-timestamp` (freshness of the Spare Cores data), `karpenter.sparecores.com/selection-hash`, plus the template's annotations. They are on the NodePool object and never in `spec.template`, so they do not drift nodes. |
| Owner | An ownerReference to the SCNodePool (`deletionPolicy: Delete`), so deleting the SCNodePool deletes the NodePool and Karpenter drains its nodes. |
| Requirements | `node.kubernetes.io/instance-type In [...]` (sorted), `karpenter.sh/capacity-type`, `kubernetes.io/arch`, and the template's own requirements verbatim. Provider-specific keys are never emitted. |

The controller never adopts a NodePool of the same name that it did not create
(`NameConflict`).

Other keys it knows:

| Key | Where | Meaning |
|---|---|---|
| `karpenter.sparecores.com/refresh-requested` | annotation on the SCNodePool | Any new value triggers an immediate refresh (also after `RegionPending` or `DataStale`). |
| `karpenter.sparecores.com/orphan-nodepool` | finalizer on SCNodePools with `deletionPolicy: Orphan` | On deletion the controller removes its ownerReference from the NodePool first, so the NodePool survives. |

A refresh (re-evaluation of the selection) happens when the spec changes, when
the annotation changes, when the template's NodeClass changes, when a NodePool
the controller applied disappears, when `refresh.interval` (default 24h, at least
1h) has passed, once after every controller start, and when a transient failure
(`DataStale`, `RegionPending`, ...) is retried, with a backoff from 30 seconds
up to 10 minutes. Changes of the NodePool's status, Events and NodeClaims only
update the health conditions.

## Status conditions

`status.conditions` of an SCNodePool:

| Type | Meaning |
|---|---|
| `Ready` | Summary: the NodePool is applied and healthy. When `False`, the reason says why and the last applied NodePool stays unchanged. |
| `DataAvailable` | Keeper is reachable and the benchmark and config names are valid. |
| `SelectionNonEmpty` | At least one instance type was selected. |
| `NodePoolSynced` | The NodePool was applied (or skipped in `dryRun`). |
| `NodePoolManaged` | A Karpenter controller reports status conditions on the NodePool. `False` after two minutes without any means no Karpenter manages its `nodeClassRef` (wrong group or kind). |
| `NodePoolValid` | Mirrors the NodePool's `ValidationSucceeded` condition. |
| `NodePoolSchedulable` | `False` while Karpenter emits `NoCompatibleInstanceTypes` Events for the NodePool (the listed types are not launchable in this cluster). |
| `RegionConflict` | Warning: the sources of the region (NodeClass, NodeClaims, nodes) disagree. The NodeClass wins. |

## Ready reasons

Every reason except `Ready` and `DryRun` keeps the last applied NodePool as it
is. On the very first evaluation no NodePool exists yet, so none is created.

| Reason | Meaning |
|---|---|
| `Ready` | The NodePool is applied and healthy. |
| `DryRun` | `dryRun: true`: the selection is computed, nothing is applied. |
| `InvalidTemplate` | `nodePoolTemplate` cannot be parsed, or uses a requirement key the provider forbids. |
| `InvalidSpec` | The selector or ranking is invalid in a way the CRD schema cannot express. |
| `UnsupportedProvider` | No adapter for the template's `nodeClassRef` group and kind (or the NodeClass CRD is not installed). |
| `CapacityTypeUnsupported` | The template asks for capacity types the provider cannot launch (for example `spot` on Hetzner). |
| `ArchitectureUnsupported` | The template or the selection needs architectures the provider cannot launch together. |
| `NameConflict` | A NodePool of the same name exists and is not ours. |
| `NodeClassRefImmutable` | The template changes `nodeClassRef.group` or `kind` of the existing NodePool. |
| `RegionPending` | No source has a region yet (NodeClass not ready, no NodeClaims, no labelled nodes). Retried with backoff. |
| `RegionUnknown` | A detected region does not exist in Spare Cores for the vendor. |
| `DataStale` | Keeper (or the AWS price list) is unreachable. |
| `UnknownBenchmarkConfig` | A benchmark id or config is not in Keeper's `benchmark_configs`. |
| `NoCandidates` | The selection is empty, or does not intersect the template's own `instance-type` list. `status.excluded` shows where candidates were dropped. |
| `ValidationFailed` | The rendered NodePool fails the controller's validation. |
| `NodePoolRejected` | The server-side dry run of the NodePool was rejected by the API server (the provider's own CRD rules). |
| `ApplyFailed` | Applying the NodePool failed. |
| `NodePoolNotManaged` | No Karpenter controller manages the NodePool. |
| `NodePoolInvalid` | Karpenter reports `ValidationSucceeded=False`. |
| `NodePoolUnschedulable` | Karpenter reports `NoCompatibleInstanceTypes`. |

## Events

Events on the SCNodePool (`kubectl describe scnodepool <name>`):

| Type | Reason | When |
|---|---|---|
| Normal | `NodePoolCreated` | The NodePool was created, with its number of types. |
| Normal | `NodePoolUpdated` | The NodePool changed; the message lists the added and removed types. |
| Normal | `NodePoolOrphaned` | `deletionPolicy: Orphan`: the NodePool is kept after the SCNodePool was deleted. |
| Warning | any failing Ready reason | `Ready` turned `False`, or its reason changed. |
| Warning | `RegionConflict` | The region sources disagree (all values listed). |
| Warning | `DriftLoop` | More than three NodeClaims of the NodePool were marked `RequirementsDrifted` within ten minutes although they were launched under the current requirements: a requirement on a well-known key the provider does not stamp on NodeClaims makes Karpenter replace nodes forever. Remove such requirements from the template. |
| Warning | `ZoneRequirementNotHonoured` | The provider ignores zone requirements for some capacity types. |
| Warning | `InvalidInstanceTypes` | Instance-type names that are not valid label values were dropped. |

## Metrics

Served on `--metrics-bind-address` (HTTPS with authentication by default; see
[install](../how-to-guides/install.md#metrics)), next to the standard
controller-runtime and Go metrics.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `sc_karpenter_nodepool_refresh_duration_seconds` | histogram | `result` | Duration of a refresh (Keeper queries, selection, apply) by result (the Ready reason). |
| `sc_karpenter_nodepool_keeper_requests_total` | counter | `endpoint`, `code` | HTTP requests to Keeper; `code` `0` means no response. |
| `sc_karpenter_nodepool_keeper_request_errors_total` | counter | `endpoint` | Keeper requests that failed (transport error or non-2xx). |
| `sc_karpenter_nodepool_selected_instance_types` | gauge | `scnodepool` | Instance types in the (would-be) NodePool of each SCNodePool. |
| `sc_karpenter_nodepool_condition` | gauge | `scnodepool`, `type`, `status` | `1` for the current status of a condition type, `0` for the other statuses. |
| `sc_karpenter_nodepool_build_info` | gauge | `version`, `commit`, `go_version` | Always `1`; identifies the running build. |

A useful alert: `sc_karpenter_nodepool_condition{type="Ready",status="True"} == 0`
for longer than the refresh interval.

## Health endpoints

`/healthz` (liveness) and `/readyz` (readiness) on the probe port (default
8081). Both only report that the process is serving; whether a particular
SCNodePool is healthy is in its status.

## Permissions

The manager's ClusterRole (generated from the kubebuilder markers in
`internal/controller/scnodepool_controller.go` into `config/rbac/role.yaml`):

| Resources | Verbs | Why |
|---|---|---|
| `karpenter.sparecores.com` `scnodepools` | get, list, watch, update, patch | Reconcile, finalizer |
| `scnodepools/status`, `scnodepools/finalizers` | get, update, patch; update | Status, the Orphan finalizer |
| `karpenter.sh` `nodepools` | get, list, watch, create, patch, update | The generated NodePools (never deleted explicitly: garbage collection does it) |
| `karpenter.sh` `nodeclaims` | get, list, watch | Region detection, drift-loop detection |
| core `nodes` | get, list, watch | Region detection (last resort) |
| core `events`, `events.k8s.io` `events` | create, patch (core also list, watch) | Events; watching Karpenter's `NoCompatibleInstanceTypes` Events |
| the NodeClass kinds of the supported providers | get, list, watch | Region detection and the NodeClass watch |
| `coordination.k8s.io` `leases` (and `configmaps`), namespaced | get, list, watch, create, update, patch, delete | Leader election |
| `tokenreviews`, `subjectaccessreviews` | create | Authentication and authorisation of the metrics endpoint |
