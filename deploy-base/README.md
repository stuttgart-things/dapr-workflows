# deploy-base

Shared KCL module for Dapr workflow deployments in this repo.

Provides reusable building blocks so each workflow's `deploy/main.k` only
has to declare **identity + knobs**, not boilerplate Deployment / Dapr
Component / CA wiring.

## What's here

| File            | Exposes                                              | Purpose |
|-----------------|------------------------------------------------------|---------|
| `worker.k`      | `WorkerSpec`, `buildDaprWorker`                      | Dapr-sidecar'd worker `Deployment` (no Service/probes/ports) |
| `statestore.k`  | `RedisStateStoreSpec`, `buildRedisStateStore`        | Dapr `state.redis` Component with `secretKeyRef` password |
| `secrets.k`     | `buildOpaqueSecret`, `secretKeyRefEnv`               | Opaque Secret + `env[].valueFrom.secretKeyRef` helper |
| `ca.k`          | `CABundleConfig`, `buildCAConfigMap`, `caVolume`, `caVolumeMount`, `caEnv` | Optional custom CA bundle (mounted from ConfigMap or Secret) with matching volume/mount/env entries |

## Design — separation of concerns

Each workflow's `main.k` composes these primitives in three distinct
sections:

1. **Core deployment** — `buildDaprWorker(...)` with identity + image
2. **Env / secrets wiring** — `buildOpaqueSecret(...)` + `secretKeyRefEnv(...)`
3. **Optional extras** — CA bundle, extra volumes, etc. — each toggleable
   via a top-level feature flag

The goal is that a flux consumer (or a cluster admin) can drop any single
piece by patching out its manifest without knowing anything about the
others, and adding a new workflow doesn't mean copy-pasting hundreds of
lines of Deployment YAML.

## Using it from a workflow

In the workflow's `deploy/kcl.mod`:

```toml
[package]
name = "my-workflow-deploy"
edition = "v0.11.0"
version = "0.1.0"

[dependencies]
k8s = "1.31.2"
deploy_base = { path = "../../../deploy-base" }
```

In `deploy/main.k`:

```kcl
import deploy_base.worker as w
import deploy_base.statestore as s
import deploy_base.secrets as sec
import deploy_base.ca as ca
import k8s.api.core.v1 as corev1
import manifests

_name = "my-workflow"
_namespace = "my-workflow"
_image = "ghcr.io/acme/my-workflow:latest"

namespace = corev1.Namespace { metadata.name = _namespace }

tokenSecret = sec.buildOpaqueSecret("api-token", _namespace, {token = "REPLACE_ME"})

stateStore = s.buildRedisStateStore(s.RedisStateStoreSpec {
    namespace = _namespace
    redisHost = "redis.default.svc.cluster.local:6379"
    passwordSecret = "redis-password"
})

deployment = w.buildDaprWorker(w.WorkerSpec {
    name = _name
    namespace = _namespace
    appID = _name
    image = _image
    env = [sec.secretKeyRefEnv("API_TOKEN", "api-token", "token")]
})

manifests.yaml_stream([namespace, tokenSecret, stateStore, deployment])
```

## Local path dep vs OCI push

The current Taskfile `build-scan-image-ko` uses Dagger's
`push-kustomize-base` against `$DEPLOY_DIR`, which mounts only the workflow's
`deploy/` directory. A `{ path = "../../deploy-base" }` dep resolves fine
for local `kcl run`, but breaks inside the dagger container because the
parent folder isn't mounted.

When you're ready to publish kustomize bases via that task, either:
- publish `deploy_base` as an OCI KCL package (`kcl mod push oci://ghcr.io/stuttgart-things/deploy_base`) and switch the dep to `{ oci = "...", tag = "..." }`, or
- update the dagger call to mount the repo root and pass a sub-path to `kcl run`.

Until then, local `kcl run main.k` in a workflow's `deploy/` folder works
and is enough for applying manifests directly to a dev cluster.
