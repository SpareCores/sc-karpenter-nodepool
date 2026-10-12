# Kind quickstart: a benchmark-driven NodePool on your laptop

In this tutorial you run the controller in a local kind cluster, together with
real Karpenter (its KWOK test provider, which launches fake nodes) and a replay
of Spare Cores data, so it needs no cloud account and no network access to Spare
Cores. You will create an `SCNodePool`, read how the controller selected its
instance types, schedule pods onto the NodePool it generated, and tighten the
selection to watch it change the NodePool.

The cluster behaves like an AWS `us-east-1` cluster with the Spare Cores data of
the repository's committed snapshot (2026-10-12), so the numbers below are what
you will see too, apart from generated names.

You need: Docker, `git`, about 10 minutes, and internet access for the first
run (it downloads Karpenter's source and the KWOK manifests and builds the
images). Everything else runs in the project's dev container, so nothing is
installed on your machine.

## 1. Start the cluster

```bash
git clone https://github.com/SpareCores/sc-karpenter-nodepool.git
cd sc-karpenter-nodepool
hack/dev.sh make kwok-up
```

`hack/dev.sh` runs a command in the dev container (built on first use). `make
kwok-up` then:

- builds the controller from your checkout, the KWOK build of Karpenter core
  v1.14 with an AWS instance-type catalog, and a small server that replays the
  Spare Cores snapshot as if it were the Keeper API;
- creates a kind cluster named `sckn-kwok` and installs KWOK, Karpenter, the
  SCNodePool CRD and the controller.

When it ends, it prints the path of the kubeconfig
(`test/kwok/.cache/sckn-kwok.kubeconfig`). Define a shortcut that runs `kubectl`
in the dev container against that cluster (with your own `kubectl`, point
`KUBECONFIG` at the file instead):

```bash
k() { hack/dev.sh kubectl --kubeconfig test/kwok/.cache/sckn-kwok.kubeconfig "$@"; }
k get pods -A
```

You will see Karpenter (`kube-system/karpenter`), the KWOK controller, and the
`sc-karpenter-nodepool-system` namespace with the controller and the Keeper
replay server.

## 2. Tell the cluster where it is

The controller needs the cloud region to look up prices. It reads it from the
NodeClass of your NodePool, or, when that offers none, from the
`topology.kubernetes.io/region` label of the nodes. A kind node has no such
label, and the cluster has no real EC2NodeClass, so give the node one:

```bash
k label node sckn-kwok-control-plane topology.kubernetes.io/region=us-east-1
```

On a real cluster the cloud provider sets this label, and you do nothing.

## 3. Create an SCNodePool

Save this as `fast.yaml`:

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: fast
spec:
  nodePoolTemplate:
    spec:
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
          requirements:
            - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
      disruption:
        consolidationPolicy: WhenEmptyOrUnderutilized
        consolidateAfter: 1m
      limits: {cpu: "64"}
  selector:
    vcpus: {min: 2, max: 8}
    price: {maxHourly: "0.50"}
  ranking:
    score: {benchmark: {id: geekbench:score, config: {cores: single}}}
    objective: Score
    band: {withinPercent: 12}
    families: {min: 3, max: 6}
```

It says: a pool of 2 to 8 vCPU machines up to $0.50 an hour, among them the
families whose single-core Geekbench score is within 12% of the best family
(at least three families, at most six). `nodeClassRef` names an AWS EC2NodeClass;
it only selects the AWS provider rules here, because the test cluster's
controller is configured to point every generated NodePool at KWOK's own
NodeClass.

```bash
k apply -f fast.yaml
k get scnodepools
```

```
NAME   NODEPOOL   PROVIDER   TYPES   READY   LAST REFRESH   AGE
fast   fast       aws        10      True    8s             36s
```

> If `READY` is `False` with the reason `RegionPending`, the node had no region
> label when the controller first looked. It retries on its own after half a
> minute; to do it now, run
> `k annotate scnodepool fast karpenter.sparecores.com/refresh-requested=1 --overwrite`.

## 4. Read what it decided

```bash
k get scnodepool fast -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

```
Ready=True Ready: NodePool fast has 10 instance types
RegionConflict=False SourcesAgree: regions [us-east-1] from AllNodes
DataAvailable=True KeeperReachable: Spare Cores data and benchmark configs are available
SelectionNonEmpty=True Selected: 10 instance types match the selection
NodePoolSynced=True Applied: NodePool fast has 10 instance types
NodePoolManaged=True StatusReported: a Karpenter controller reports status for the NodePool
NodePoolValid=True ValidationSucceeded:
NodePoolSchedulable=True NoSchedulingFailures: Karpenter reports no NoCompatibleInstanceTypes events for the pool
```

The last three conditions come from Karpenter itself: it has picked the NodePool
up and found it valid. Now the selection:

```bash
k get scnodepool fast -o json | hack/dev.sh jq '.status | {regions, selected: {count: .selected.count,
  band: .selected.band, families: [.selected.families[] | [.name, .rank, .objective, .sizes]]}, excluded}'
```

You get the detected regions, the band that was applied (reference family
`m8azn` at a score of 3122, lowest kept `c8a` 11.6% below it), the four kept
families with their median score and number of sizes, and `excluded`: how many
candidates each step dropped (932 by `vcpus`, 89 by the price cap, 280 by the
ranking, and so on). That is the first place to look when a selection is not
what you expect. The same data lists every selected type with its score and price:

```bash
k get scnodepool fast -o json | hack/dev.sh jq -r '.status.selected.instanceTypes[] | [.name, .score, .price] | @tsv'
```

## 5. Look at the NodePool

```bash
k get nodepool fast -o yaml
```

It is an ordinary Karpenter NodePool, owned by the SCNodePool (an
`ownerReference`), with your template and one more requirement of each kind:

```yaml
spec:
  template:
    spec:
      nodeClassRef: {group: karpenter.kwok.sh, kind: KWOKNodeClass, name: default}   # the test override
      requirements:
        - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
        - {key: kubernetes.io/arch, operator: In, values: [amd64]}
        - key: node.kubernetes.io/instance-type
          operator: In
          values: [c8a.2xlarge, c8a.large, c8a.xlarge, m8a.2xlarge, m8a.large, m8a.xlarge,
                   m8azn.large, m8azn.xlarge, r8a.large, r8a.xlarge]
```

`kubernetes.io/arch` was added from the selected types, and the instance-type
list is the selection, sorted. Karpenter does everything else.

## 6. Put pods on it

Save as `inflate.yaml`: three pods that each ask for 1.5 vCPU and may only run
on nodes of this NodePool (the real node of the kind cluster is tainted, so they
could not go there anyway):

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: inflate
spec:
  replicas: 3
  selector:
    matchLabels: {app: inflate}
  template:
    metadata:
      labels: {app: inflate}
    spec:
      nodeSelector:
        karpenter.sh/nodepool: fast
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.10
          resources:
            requests: {cpu: "1500m", memory: 256Mi}
```

```bash
k apply -f inflate.yaml
k get nodeclaims
k get nodes -L node.kubernetes.io/instance-type,karpenter.sh/capacity-type,topology.kubernetes.io/zone
```

```
NAME         TYPE          CAPACITY    ZONE         NODE                                                READY   AGE
fast-969w4   c8a.2xlarge   on-demand   us-east-1d   kwok-fast-969w4-robinfate-1-ufccutrqgl-2252457648   True    26s
```

Within a few seconds Karpenter launched a (fake) node for the three pods. Its
type is one of the ten the controller listed: Karpenter picked one that fits
all three pods (4.5 vCPU) from the allowed list.

## 7. Change the selection

Say the price cap was too generous. Lower it:

```bash
k patch scnodepool fast --type merge -p '{"spec":{"selector":{"price":{"maxHourly":"0.20"}}}}'
k get scnodepool fast -o json | hack/dev.sh jq -c '.status | {count: .selected.count, pending: .pendingRemovals}'
k describe scnodepool fast | tail -12
```

The controller re-evaluates at once, because the spec changed. Several types no
longer qualify (`c8a.2xlarge`, `m8a.2xlarge`, `m8azn.xlarge`, ...). Removing a
type from a NodePool makes Karpenter replace every node of that type, so the
controller removes at most three per refresh (`churn.maxRemovalsPerRefresh`,
default 3) and keeps the others as `pendingRemovals`, with the number of
refreshes they have failed:

```
Normal  NodePoolUpdated  updated NodePool fast: removed 3 (c8a.2xlarge, m8a.2xlarge, m8azn.xlarge); 7 instance types
```

Your node of type `c8a.2xlarge` is now outside the NodePool's requirements, so
Karpenter marks it `Drifted` and replaces it with nodes of the remaining types
(`k get nodeclaims -w` shows it). The remaining pending types leave the NodePool
at the following refreshes. Types are never added with such a delay: a type
that qualifies is added immediately.

## 8. Try a dry run

`dryRun: true` computes and reports without touching any NodePool. Create a
second SCNodePool that selects GPU instances:

```bash
k apply -f - <<'EOF'
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: gpu-40g
spec:
  dryRun: true
  nodePoolTemplate:
    spec:
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
      disruption: {consolidationPolicy: WhenEmpty, consolidateAfter: 10m}
  selector:
    accelerator:
      types: [GPU]
      manufacturers: [NVIDIA]
      memoryPerDevice: {minGiB: 40}
    price: {maxHourly: "12"}
EOF
k get scnodepools
k get nodepools
```

`gpu-40g` reports `Ready` with the reason `DryRun` and a number of types, but
`k get nodepools` shows only `fast`: nothing was created. Remove `dryRun` to
create the pool.

## 9. Clean up

```bash
k delete scnodepool gpu-40g fast
k delete deployment inflate
k get nodepools,nodeclaims     # gone: the NodePool is owned by the SCNodePool
hack/dev.sh make kwok-down
```

Deleting an SCNodePool deletes its NodePool, and Karpenter drains the nodes
(set `deletionPolicy: Orphan` to keep the NodePool). `make kwok-down` deletes the
kind cluster and the images.

## What you learned

- an SCNodePool is a NodePool template plus a *selection* of instance types
  expressed in hardware, price and measured performance;
- the controller reports its reasoning in the status (conditions, `selected`,
  `excluded`) and in Events;
- the NodePool it generates is plain Karpenter: the controller only decides which
  types it lists, and removes types slowly because it replaces nodes;
- the test cluster is the same lane CI runs for every change
  ([testing](../how-to-guides/testing.md)).

## Next steps

- [Write an SCNodePool](../how-to-guides/write-an-scnodepool.md): recipes for
  common goals.
- [Install the controller](../how-to-guides/install.md) on a real cluster.
- [Selector reference](../reference/selectors.md): everything you can filter and
  rank on.
