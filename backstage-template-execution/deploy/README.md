# backstage-template-execution — Kubernetes deploy (KCL)

KCL manifests for deploying the `backstage-template-execution` Dapr workflow worker.

The worker is headless (no HTTP server) — it registers workflows/activities with
the Dapr sidecar and waits for `workflows/dapr/BackstageTemplateWorkflow/start`
requests against the sidecar's HTTP API. So there is **no Service, no container
port, and no liveness/readiness probe** in this deployment.

## Layout

| File       | Purpose                                       |
|------------|-----------------------------------------------|
| `kcl.mod`  | KCL package with `k8s` schema dependency      |
| `main.k`   | All manifests as a single YAML stream         |

## What it renders

1. `Namespace/backstage-workflows`
2. `Secret/github-token` — `GITHUB_TOKEN` for watch + PR merge activities
3. `Secret/backstage-auth-token` — Backstage service-to-service static token (env fallback when workflow input omits `authToken`)
4. `Secret/redis-password` — Dapr statestore auth
5. `Component/statestore` (`dapr.io/v1alpha1`) — Redis state store for workflow/actor state
6. `ConfigMap/backstage-ca` — custom CA bundle (optional, see below)
7. `Deployment/backstage-template-execution` — single replica, Dapr sidecar enabled via annotations

## Render & apply

```bash
kcl run main.k > manifests.yaml
kubectl apply -f manifests.yaml
```

Before applying, replace the three `REPLACE_WITH_*` placeholders in the
generated Secrets (or patch them at apply time with
`kubectl create secret generic --dry-run=client -o yaml | kubectl apply -f -`).

## Configuration knobs (top of `main.k`)

| Variable              | Default                                                 | Notes |
|-----------------------|---------------------------------------------------------|-------|
| `_name` / `_appID`    | `backstage-template-execution`                          | Deployment name and Dapr `app-id` |
| `_namespace`          | `backstage-workflows`                                   | |
| `_image`              | `ttl.sh/stuttgart-things/backstage-template-execution:1h` | Placeholder — replace with the image built by `Taskfile.yaml` |
| `_replicas`           | `1`                                                     | Keep at 1 unless you've validated multi-worker semantics |
| `_redisHost`          | `redis-stack.homerun2-flux.svc.cluster.local:6379`      | Dapr statestore backend |
| `_enableCA`           | `True`                                                  | Mount a trusted CA bundle (see below) |
| `_caSource`           | `"configmap"`                                           | `"configmap"` or `"secret"` |
| `_caConfigMapName`    | `backstage-ca`                                          | |
| `_caSecretName`       | `backstage-ca`                                          | Used when `_caSource = "secret"` |
| `_caFileName`         | `ca.pem`                                                | Key inside the ConfigMap/Secret |
| `_caMountPath`        | `/etc/ssl/backstage`                                    | `SSL_CERT_FILE` points to `${_caMountPath}/${_caFileName}` |
| `_caConfigMapData`    | placeholder PEM                                         | Used when `_caSource = "configmap"` |

## Trusting a self-signed Backstage cert

The worker talks to Backstage over HTTPS. If Backstage is fronted by a cert
issued by an internal CA, the worker needs to trust it. Two options:

**Option A — ConfigMap with inline PEM (default):**
Paste the CA PEM into `_caConfigMapData` in `main.k`, re-render, apply.

**Option B — pre-existing Secret:**
Set `_caSource = "secret"` in `main.k`, then create the Secret yourself:

```bash
kubectl -n backstage-workflows create secret generic backstage-ca \
  --from-file=ca.pem=/path/to/ca.pem
```

Either way the file is mounted at `/etc/ssl/backstage/ca.pem` and
`SSL_CERT_FILE` is set so Go's TLS stack picks it up — no code changes, and
**no need** for `BACKSTAGE_INSECURE_TLS=true` in-cluster.

Set `_enableCA = False` to skip the CA mount entirely (e.g. for a
publicly-trusted Backstage cert).

## Environment variables injected into the container

| Env                    | Source                                     |
|------------------------|--------------------------------------------|
| `GITHUB_TOKEN`         | `Secret/github-token` → key `token`        |
| `BACKSTAGE_AUTH_TOKEN` | `Secret/backstage-auth-token` → key `token` (used when workflow input `authToken` is empty) |
| `SSL_CERT_FILE`        | Set to `${_caMountPath}/${_caFileName}` when `_enableCA` is true |

## Triggering a workflow

With the sidecar running alongside the pod, port-forward the Dapr HTTP port
and reuse the repo's `run.sh`:

```bash
kubectl -n backstage-workflows port-forward deploy/backstage-template-execution 3500:3500
cd ..    # back to backstage-template-execution/
./run.sh input.json
./run.sh status <instanceId>
```

`input.json` can omit `authToken` — the worker falls back to
`BACKSTAGE_AUTH_TOKEN` from the mounted Secret.

## Prerequisites

- Dapr control plane installed in the target cluster (`dapr-system` namespace)
- Redis reachable at `_redisHost` (adjust if you're not on the homerun2 cluster)
- Image built and pushed by `Taskfile.yaml`; update `_image` to match the
  actual tag before applying
