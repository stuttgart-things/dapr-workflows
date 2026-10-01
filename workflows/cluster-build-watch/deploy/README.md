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
8. `Deployment/cluster-build-watch`, with the trust-manager bundle mounted
   (see below)

Only `source: kube` needs the ClusterRole. A worker that only runs watches
with `source: machinery` on both sides reads nothing from its own API server
and could drop it; the status Role is needed either way.

## TLS trust and the machinery token

| Option (`-D`) | Default | |
|---|---|---|
| `trustBundle` | `true` | mount a CA bundle ConfigMap read-only and set `SSL_CERT_FILE` to it |
| `trustBundleConfigMap` | `cluster-trust-bundle` | the ConfigMap trust-manager distributes; not rendered here |
| `trustBundleKey` | `trust-bundle.pem` | key in that ConfigMap |
| `machineryTokenSecret` | empty (off) | Secret with key `token`, mounted as `MACHINERY_AUTH_TOKEN_FILE` |

The bundle is what lets the worker verify a machinery endpoint behind the lab
gateway (lab CA). `SSL_CERT_FILE` replaces Go's system pool for every TLS
client in the worker, Teams and the webhook included, so it must be a bundle
that carries the system CAs too, as trust-manager's does. Without the
ConfigMap in the namespace the pod does not start: make sure the
trust-manager `Bundle` targets the worker's namespace, or render with
`-D trustBundle=false`.

The token Secret is yours to provide (SOPS in flux). machinery runs without
auth today; the worker sends the token only over TLS.

## Render & apply

```bash
kcl run main.k -D imageTag=<release-tag> > manifests.yaml
# no trust-manager bundle in the namespace:
kcl run main.k -D imageTag=<release-tag> -D trustBundle=false > manifests.yaml
kubectl apply -f manifests.yaml
```

Fill in `redis-password`, and the webhook URLs if you want them, before
applying. In flux, replace both Secrets with SOPS-encrypted ones.

## Watching another XR kind

Add a rule to `_readRules` in `main.k`, then point `target.apiVersion` /
`target.resource` at it. The XR only needs a `status.stage` string and the
usual Crossplane `Ready` condition.
