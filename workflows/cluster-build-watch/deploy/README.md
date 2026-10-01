# cluster-build-watch — Kubernetes deploy (KCL)

Same pattern as [backstage-template-execution/deploy](../../backstage-template-execution/deploy/README.md),
composed from the shared [`deploy-base`](../../../deploy-base/README.md).

## What it renders

1. `Namespace/cluster-build-watch`
2. `Secret/redis-password`: Dapr statestore auth
3. `Secret/notify-webhooks`: keys `teams` and `webhook`. Empty means the sink is off
4. `Component/statestore`: Redis state store
5. `ServiceAccount/cluster-build-watch`
6. `ClusterRole` + `ClusterRoleBinding` `cluster-build-watch-read`: `get` on
   Argo CD Applications, Flux Kustomizations and ClusterStacks
7. `Role` + `RoleBinding` `cluster-build-watch-status`: `get/create/patch` on
   ConfigMaps, in this namespace only
8. `Deployment/cluster-build-watch`

## Render & apply

```bash
kcl run main.k -D imageTag=<release-tag> > manifests.yaml
kubectl apply -f manifests.yaml
```

Fill in `redis-password`, and the webhook URLs if you want them, before
applying. In flux, replace both Secrets with SOPS-encrypted ones.

## Watching another XR kind

Add a rule to `_readRules` in `main.k`, then point `target.apiVersion` /
`target.resource` at it. The XR only needs a `status.stage` string and the
usual Crossplane `Ready` condition.
