# Reference

Technical descriptions of how specific parts of sc-karpenter-nodepool work.

- [Selectors](selectors.md) — every way an `SCNodePool` can select instance
  types from Spare Cores data: fields, units, data sources, coverage per
  vendor, ranking and data-quality rules, benchmark catalog
- [API reference](api.md) — every field of the `SCNodePool` custom resource
  (generated from the CRD)
- [Controller reference](controller.md) — manager flags and environment
  variables, Helm chart values, what the controller creates, status conditions
  and reasons, Events, metrics, permissions
