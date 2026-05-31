# ObserveX on ArgoCD

Phase C-3a ships an example Argo CD `AppProject` + `Application` pair so
that an operator can roll the existing Helm chart out via GitOps in two
commands:

```bash
kubectl apply -f deploy/argocd/appproject.yaml
kubectl apply -f deploy/argocd/application.yaml
```

The chart is rendered straight from the in-repo Helm chart at
`deploy/helm/observex/`. To pin to a release commit, set
`spec.source.targetRevision` to a tag or commit SHA instead of `HEAD`.

## Topology

```
deploy/argocd/
├── appproject.yaml      # AppProject "observex" — RBAC + repo scope
└── application.yaml     # Application "observex" — chart pointer + sync policy
```

Both manifests assume:

- Argo CD is installed in the `argocd` namespace.
- The cluster the workloads should land on is the same cluster Argo CD
  runs in (`destination.server: https://kubernetes.default.svc`). For
  multi-cluster setups, change `destination` to the target cluster.
- ObserveX workloads run in namespace `observex` (auto-created).

## Secrets

The Helm chart expects a secret named `observex-secrets` with these
keys:

| key             | required           | used by                |
|-----------------|--------------------|------------------------|
| `admin-token`   | yes                | tenant-api             |
| `postgres-url`  | yes                | tenant-api, alert-manager |
| `slack-webhook` | optional           | alert-manager          |
| `pagerduty-key` | optional           | alert-manager          |

Manage that secret with whatever you already trust — Sealed Secrets,
SOPS-encrypted GitOps repo, External Secrets Operator pulling from
AWS Secrets Manager or Vault, etc. The Application here does NOT ship
the secret; bringing your own is required.

## Drift policy

`syncPolicy.automated.prune: true` deletes resources that disappear
from the chart. `selfHeal: true` reconciles manual cluster edits back
to the chart. Disable both during incident response by patching the
Application: `kubectl -n argocd patch app observex --type merge -p
'{"spec":{"syncPolicy":null}}'`.
