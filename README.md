# sc-karpenter-nodepool

A Kubernetes controller that turns a declarative `SCNodePool` into a regular
[Karpenter](https://karpenter.sh) `NodePool` whose instance-type list is
selected with [Spare Cores](https://sparecores.com) data, and keeps it current.

Karpenter chooses instance types from specifications (vCPU, memory, family
labels) and price. Spare Cores measures every instance type of eight clouds with
the same benchmarks and knows hardware facts Karpenter does not expose: CPU flags,
cache sizes, hyper-threading, measured memory, GPU details. An `SCNodePool` says
what you need in those terms; the controller writes the NodePool.

```yaml
apiVersion: karpenter.sparecores.com/v1alpha1
kind: SCNodePool
metadata:
  name: web-fast
spec:
  nodePoolTemplate:
    spec:
      template:
        spec:
          nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
      disruption: {consolidationPolicy: WhenEmptyOrUnderutilized, consolidateAfter: 1m}
  selector:
    vcpus: {min: 2, max: 16}
    price: {maxHourly: "0.50"}
  ranking:
    score: {benchmark: {id: geekbench:score, config: {cores: single}}}
    band: {withinPercent: 12}
    families: {min: 3, max: 6}
```

This yields a NodePool restricted to the fastest single-core families up to
$0.50 an hour, with every size of each, refreshed daily with churn control so
running nodes are not replaced needlessly.

## Demo

[![GILty on a hand-picked r5a.large and on the r6a.large sc-karpenter-nodepool picked, side by side](docs/demo/demo.svg)](https://sparecores.github.io/sc-karpenter-nodepool/demo/)

[GILty](https://github.com/SpareCores/GILty), a Python app that can use only
one core, runs on an `r5a.large` picked by hand. An `SCNodePool` asks for at
least the same vCPUs and memory for at most 10% more money, ranked by measured
single-core performance, and gets an `r6a.large`: same size, same price, and
the same program runs about 2.4× faster there. Recorded live on EKS Auto Mode;
[watch it with pause, seek and
notes](https://sparecores.github.io/sc-karpenter-nodepool/demo/).

## Features

- Selectors on CPU (flags, caches, manufacturer, SMT, clock), memory,
  accelerators, network, local storage, price, benchmarks and composite
  workload profiles; family-level ranking with a score band
- Providers: AWS, EKS Auto Mode, Azure and AKS NAP, GCP, Alibaba Cloud, Hetzner,
  UpCloud, OVH, Vultr; the cloud region is detected automatically
- Safe by construction: validated and dry-run before it applies, never an empty
  list, hysteresis on removals, status conditions, Events and Prometheus metrics
- Works with managed Karpenter: it creates its own NodePools and patches nothing

## Documentation

- Learn: [kind quickstart](docs/tutorials/kind-quickstart.md) (no cloud account)
- Do: [install](docs/how-to-guides/install.md),
  [write an SCNodePool](docs/how-to-guides/write-an-scnodepool.md),
  [test and develop](docs/how-to-guides/testing.md)
- Look up: [API](docs/reference/api.md), [selectors](docs/reference/selectors.md),
  [controller](docs/reference/controller.md)
- Understand: [decisions](docs/explanation/DECISIONS.md), the
  [v1 overview](docs/plans/v1/wip/00-overview.md)

## Install

```bash
helm install sc-karpenter-nodepool \
  oci://ghcr.io/sparecores/charts/sc-karpenter-nodepool \
  --version <version> --namespace sc-karpenter-nodepool --create-namespace
```

See the [install guide](docs/how-to-guides/install.md) for provider notes.

## Development

Everything runs in a container, so only Docker is needed:
`hack/dev.sh make test`. See [test and develop](docs/how-to-guides/testing.md).

## License

[MPL-2.0](LICENSE)
