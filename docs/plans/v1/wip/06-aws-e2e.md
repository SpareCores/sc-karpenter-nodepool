# 06 — Real AWS e2e workflow (L5) and sweeper

Depends on: 05. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §8.5.

## Goal

A manually triggered workflow that proves the controller end to end on EKS
Auto Mode and on EKS with self-managed Karpenter, plus a scheduled sweeper.
This plan writes and statically validates them; it does **not** run them
(that costs money and is triggered by the coordinator).

## Prerequisites (already provisioned, do not change)

GitHub environment `aws-e2e` (deployments from `main` only) and repository
variables `AWS_ROLE_ARN`, `AWS_REGION`, `AWS_ACCOUNT_ID`, `SCKN_VPC_ID`,
`SCKN_SUBNET_IDS`, `SCKN_CLUSTER_ROLE_ARN`, `SCKN_AUTO_NODE_ROLE_NAME`,
`SCKN_KARPENTER_NODE_ROLE_NAME`, `SCKN_KARPENTER_NODE_ROLE_ARN`,
`SCKN_KARPENTER_CONTROLLER_ROLE_ARN`. The role can only manage `sckn-ci-*`
clusters, pass the listed roles, and launch small instance types. Never put
real values of these into the repository; reference the variables.

## Scope

- `.github/workflows/e2e-aws.yml` (`workflow_dispatch`, inputs `targets:
  both|auto|karpenter`, `capacityType: on-demand|spot` default on-demand),
  structured as in overview §8.5 (image build → matrix jobs with
  `environment: aws-e2e`, `concurrency`, `timeout-minutes: 120`,
  `if: always()` teardown, diagnostics dump on failure).
- `test/e2e-aws/`: eksctl configs (`eksctl-auto.yaml`,
  `eksctl-karpenter.yaml`, rendered with `envsubst` from the variables),
  `setup-auto.sh` (custom NodeClass `sckn-ci`, infra NodePool for our
  controller), `setup-karpenter.sh` (Karpenter v1.14.1 via Helm, Pod Identity,
  EC2NodeClass), `deploy-controller.sh` (our Helm chart if plan 07 is done,
  else the kustomize manifests), `test-select.sh`, `test-drift.sh`,
  `dump-diagnostics.sh`, `teardown.sh`, `preflight-cleanup.sh`, and an
  `SCNodePool` that yields a small known Graviton/t4g set valid on Auto Mode.
- `.github/workflows/e2e-aws-sweeper.yml`: every 3 hours + manual; deletes
  `sckn-ci-*` clusters older than 3 hours and orphans (instances tagged
  `eks:eks-cluster-name=sckn-ci-*`, Auto Mode instances via
  `--include-managed-resources`, launch templates, `sckn-ci-*` instance
  profiles, `eksctl-sckn-ci-*` stacks in `DELETE_FAILED`).
- Pin eksctl (download + checksum) and every action version.

## Gates

`actionlint` on both workflows, `shellcheck test/e2e-aws/*.sh`,
`eksctl create cluster --dry-run -f <rendered config>` for both configs with
placeholder values (inside the dev container, no AWS credentials), CONVENTIONS
gates 1–3 unchanged.
