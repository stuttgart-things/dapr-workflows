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
    passwordSecret = "redis-password"  # pragma: allowlist secret
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

## Local path dep and the dagger render

`workflows/*/deploy/kcl.mod` depends on this module by relative path
(`{ path = "../../../deploy-base" }`), which points outside the workflow's own
`deploy/` directory. Rendering with `--source ./deploy` fails in the dagger
container because the parent folders are not mounted.

The fix is `--subpath`: mount the repo root and let `kcl` cd into the
sub-package, so the relative dep resolves.

```bash
dagger call -m github.com/stuttgart-things/dagger/kcl@v0.129.4 \
  render-kustomize-base \
  --source . \
  --subpath workflows/backstage-template-execution/deploy \
  export --path=/tmp/kustomize-base
```

That is what the release job in `.github/workflows/build-scan-changed.yaml`
does. Publishing `deploy_base` as its own OCI KCL package is therefore not
required — the path dep stays, and the module has no release cycle of its own
to keep in sync.

Local `kcl run main.k` inside a workflow's `deploy/` folder is unaffected and
still works.
