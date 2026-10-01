# cluster-build-watch

Dapr workflow that **watches** a cluster build end to end and reports every
checkpoint it crosses: GitOps sync → the Crossplane XR's stages → Ready.

It does not drive anything. The build runs as it does today (Backstage → PR →
merge → Argo CD / Flux → Crossplane); this workflow follows it, notices when a
stage hangs, and tells people and systems about it.

```
 gitops-sync ──► xr-pending ──► vm ──► baseos ──► distribution ──► kubeconfig ──► access ──► platform ──► Ready
 (Argo CD/Flux    (XR not yet     └──────────── status.stage of the ClusterStack XR ────────────┘
  applied the      created)
  merge commit)
```

That is the standard path; the rancher and machine-pool paths are under
[Stages](#stages). Both sides can be read from the worker's own API server or
from a [machinery](#source-machinery) gRPC endpoint in front of another
cluster.

Why a workflow and not just events: a failure produces an event, a **hang does
not**. Only something that waits can say "this build has been in `baseos` for
40 minutes". Each stage has its own timeout for exactly that.

## Checkpoints

| Event | Severity | When |
|---|---|---|
| `started` | info | the watch begins |
| `synced` | info | the Argo CD Application / Flux Kustomization applied the wanted revision |
| `stage` | info | the XR's `status.stage` changed, with the duration of the previous stage |
| `stuck` | warning | a stage ran past its timeout. Reported **once** per stage; the watch goes on |
| `degraded` / `recovered` | warning / info | `Synced=False` on the XR, a failed sync, Degraded health. Reported on the transition, not on every poll |
| `ready` | success | the XR is done, with every stage's duration |
| `failed` | error | the overall timeout passed, or the XR was deleted mid-build |

What counts as done is deliberately strict:

- **GitOps:** `Synced` **at the wanted revision**. Without `gitops.revision`,
  `Synced` may describe the tree *before* the merge, since Argo has not refreshed
  yet. Health is reported but not required: an Application carrying the XR can
  stay `Progressing` for as long as the cluster builds.
- **XR:** Crossplane's `Ready` condition **and** `status.ready` (when the XRD
  has it). One alone is not enough: a `ClusterStack` can show `status.ready:
  true` while `Ready` stays False ("a stack can look finished and not be", see
  crossplane-configurations `bootstrap/cluster`).

### Stages

The XR stages are the `status.stage` values a `ClusterStack` writes (kcl
`xplane-cluster` 0.25.1). There are three paths, depending on how the cluster
is built:

| Path | Stages |
|---|---|
| standard | `vm` → `baseos` → `distribution` → `kubeconfig` → `access` → `platform` → [`management-plane`] → `ready` |
| rancher custom node | `rancher` → `vm` → `baseos` → `join` → `access` → `platform` → [`management-plane`] → `ready` |
| machine pool | `node-ip` → `rancher` → `kubeconfig` → `access` → `platform` → [`management-plane`] → `ready` |

`management-plane` is optional. `status.ready` ignores it, the Ready
condition does not, which is one reason the watch requires both. **There is
no failure value**: a step that fails leaves the stage where it is. That is
exactly what the per-stage timeouts are for, and why `stuck` exists.

Built-in stage timeouts (minutes, override per stage with
`target.stageTimeoutMin`):

| Stage | Min | | Stage | Min |
|---|---|---|---|---|
| `gitops-sync` | 15 | | `kubeconfig` | 10 |
| `xr-pending` | 10 | | `access` | 10 |
| `xr-created` | 10 | | `platform` | 45 |
| `vm` | 30 | | `management-plane` | 45 |
| `baseos` | 30 | | `ready` | 10 |
| `distribution` | 30 | | `rancher` | 30 |
| `join` | 30 | | `node-ip` | 15 |

Anything else gets `target.defaultStageTimeoutMin` (30). The whole watch fails
after `timeoutMin` (240).

`gitops-sync` stays at 15 minutes on purpose: on machinery the GitRepository
polls every minute and a merge reaches `machinery-xrs` in 30 to 60 seconds, so
15 minutes is already generous. A `gitops-sync` that does hang is usually a
Kustomization whose `dependsOn` (on machinery: `machinery-fleet-state`) is not
ready; the stuck notification carries the Ready condition's reason and
message (`False, DependencyNotReady: dependency '...' is not ready`), so it
says which.

## Where the status goes

Each checkpoint goes to every sink that is configured. The sinks are
independent: if Teams is down, the ConfigMap and the webhook still get the
checkpoint. A failed report never stops the watch.

| Sink | Configured by | Content |
|---|---|---|
| Status ConfigMap | always (worker namespace, or `STATUS_NAMESPACE`) | `cluster-build-watch.<namespace>.<name>` with `phase`, `stage`, `message`, `lastEvent`, `stages` (JSON with durations). Label `cluster-build-watch.sthings.io/phase` |
| Microsoft Teams | `TEAMS_WEBHOOK_URL` | Adaptive Card, colour by severity, stage durations on the final card |
| Any HTTP endpoint | `STATUS_WEBHOOK_URL` | a CloudEvent (`io.sthings.clusterbuild.<event>`). The `id` is stable per checkpoint, so a receiver can drop the duplicate a retry sends |
| Dapr | always | the workflow's custom status, e.g. `[Watching] baseos: stage baseos` |

```bash
kubectl -n cluster-build-watch get cm -l app.kubernetes.io/managed-by=cluster-build-watch \
  -L cluster-build-watch.sthings.io/phase
kubectl -n cluster-build-watch get cm cluster-build-watch.cluster-build-watch.u26-kind1 \
  -o jsonpath='{.data.stage}{"  "}{.data.message}{"\n"}'
```

Webhook URLs are credentials (Teams signs them in the query string). They come
from a Secret, and a transport error never echoes them (`redactURL`).

## Start a watch

On a cluster, use the kro trigger. One `ClusterBuildWatch` CR starts one run, see
[`trigger/`](trigger/README.md):

```yaml
apiVersion: kro.run/v1alpha1
kind: ClusterBuildWatch
metadata:
  name: u26-kind1
  namespace: cluster-build-watch
spec:
  gitops:
    kind: argocd
    name: clusterstack-u26-kind1
    revision: <merge commit>
  target:
    namespace: crossplane-system
    name: u26-kind1
```

Input fields (see `Input` in [`watch.go`](watch.go)):

| Field | Default | |
|---|---|---|
| `name` | `target.name` | Display name in notifications |
| `gitops.kind` | `argocd` | `argocd` (Application) or `flux` (Kustomization) |
| `gitops.namespace` | `argocd` / `flux-system` | |
| `gitops.name` | — | required when `gitops` is set |
| `gitops.revision` | — | merge commit, at least 7 characters of the SHA |
| `target.apiVersion` | `config.stuttgart-things.com/v1alpha1` | any XR that writes `status.stage` |
| `target.resource` | `clusterstacks` | plural |
| `target.namespace`, `target.name` | — | required |
| `target.stageTimeoutMin` | built-ins above | `{stage: minutes}` |
| `target.source`, `gitops.source` | `kube` | `kube` or `machinery`, per side, see [below](#source-machinery) |
| `target.machinery`, `gitops.machinery` | — | `{server, kind, plaintext}`, required with `source: machinery` |
| `pollSeconds` | 30 | |
| `timeoutMin` | 240 | |

## Source machinery

By default (`source: kube`) the worker GETs the objects from the API server it
runs on, which needs the ClusterRole below and the watched objects on that
cluster. With `source: machinery` a side is read from a
[machinery](https://github.com/stuttgart-things/machinery) ResourceService
instead: a read-only gRPC service (`GetResourceDetail`, release v1.14.0) in
front of another cluster's informer cache. The worker then needs no RBAC on
the watched cluster at all.

```json
"gitops": {
  "kind": "flux", "namespace": "flux-system", "name": "machinery-xrs",
  "revision": "<merge commit>",
  "source": "machinery",
  "machinery": {"server": "machinery-grpc.machinery.4sthings.tiab.ssc.sva.de:443"}
},
"target": {
  "namespace": "default", "name": "<clusterstack>",
  "source": "machinery",
  "machinery": {"server": "machinery-grpc.machinery.4sthings.tiab.ssc.sva.de:443"}
}
```

See [`input-machinery.json`](input-machinery.json) and
[`trigger/examples/watch-machinery.yaml`](trigger/examples/watch-machinery.yaml).

| Field | Default | |
|---|---|---|
| `machinery.server` | — | `host:port` |
| `machinery.kind` | `ClusterStack` (target), `Kustomization` (gitops) | the kind name as configured on that machinery server |
| `machinery.plaintext` | `false` | no TLS. For an in-cluster Service only |

**What machinery has to provide.** The answer is mapped back onto the object
the kube parsers read, so both sources are judged by the same code
(`machineryObject` in [`machinery.go`](machinery.go)):

| Parser reads | From machinery |
|---|---|
| `status.conditions` | `conditions` |
| `status.stage` | info field `Stage` |
| `status.ready` | info field `StatusReady` (`"true"` / `"false"`) |
| `status.lastAppliedRevision` | info field `Revision` |

machinery omits an info field when it is empty on the object, so a missing
label cannot be told from a missing mapping. Where that matters it is an
error, not a guess: a `ClusterStack` with `Ready=True` but no `StatusReady`,
and a Kustomization that is Ready without a `Revision` while `gitops.revision`
is set. Both show up as `observe failed` in the custom status. The machinery
cluster's server maps all four today.

**Errors.** Only gRPC `NotFound` means "not found". `InvalidArgument` (kind not
configured on that server), `Unavailable` (CRD not served, informer not
ready, or the connection failed) and everything else are errors, like a 403 on
the kube path: the watch keeps its clocks running and says why in the custom
status.

**Not supported:** Argo CD. No machinery serves `Application` yet, and the
Argo parser needs sync, health and operation state; `gitops.kind: argocd` with
`source: machinery` is rejected when the watch starts.

**TLS.** The machinery gateway presents a certificate from the lab CA. Go
honours `SSL_CERT_FILE`, and the deploy mounts trust-manager's
`cluster-trust-bundle` (system CAs plus lab CAs) and points it there, see
[`deploy/`](deploy/README.md). grpc-go 1.67 and later require ALPN `h2` from
the server; the machinery Gateway negotiates it (it did not at first, which
showed up as a handshake error, not as a gRPC status).

**Auth.** machinery runs without auth today. When it gets bearer auth, give
the worker a token through `MACHINERY_AUTH_TOKEN_FILE` (a mounted Secret, see
the deploy option `machineryTokenSecret`; read per call, so rotation needs no
restart) or `MACHINERY_AUTH_TOKEN`. It is sent as `authorization: Bearer ...`,
over TLS only, never with `plaintext: true`. The token is never part of the
input, since the input lands in the workflow history and the status
ConfigMap, and errors are scrubbed of it.

One connection per observation: an activity makes one RPC every
`pollSeconds`, so a pooled connection would save a TLS handshake per half
minute at the price of lifecycle code for CA and token rotation.

## Run locally

The worker reads the API through `KUBE_API_SERVER` when it is set, so
`kubectl proxy` against any cluster is enough:

```bash
kubectl proxy --port 8001 &

export KUBE_API_SERVER=http://127.0.0.1:8001
export STATUS_NAMESPACE=default          # where the status ConfigMap goes
export TEAMS_WEBHOOK_URL='https://...'   # optional
export STATUS_WEBHOOK_URL='https://...'  # optional

cd workflows/cluster-build-watch
dapr run --app-id cluster-build-watch --app-protocol grpc \
  --dapr-grpc-port 50012 --dapr-http-port 3500 -- go run .

# another shell
./run.sh                 # uses input.json
./run.sh input-machinery.json   # source machinery: no kubectl proxy needed
                                # for the watch, only for the status ConfigMap
./run.sh status <id>
```

## Deploy

[`deploy/main.k`](deploy/main.k) renders the worker with its own ServiceAccount:

- **ClusterRole `cluster-build-watch-read`**: `get` on Argo CD Applications,
  Flux Kustomizations and `clusterstacks.config.stuttgart-things.com`. Add a
  rule there before watching another XR kind. A missing grant is a 403, which
  the watch reports as `observe failed` in its custom status, and never as
  "not found". Only `source: kube` uses it; a worker that only runs
  `source: machinery` watches could do without.
- **Role `cluster-build-watch-status`**: `get/create/patch` on ConfigMaps in
  the worker's own namespace only. The status ConfigMaps live next to the
  worker, not next to the CR.

The release pipeline publishes the image, the kustomize base and the trigger
RGD like it does for `backstage-template-execution` (see the
[repo README](../../README.md#release)). **The first release creates new ghcr
packages, and those start private**: set `dapr-cluster-build-watch` and
`dapr-cluster-build-watch-kustomize` to public once, or flux's anonymous pull
fails with a 401.

## Design notes

- **The checkpoint logic is a pure function**, `Advance` in
  [`watch.go`](watch.go): observation in, events out, no Dapr, no HTTP. That is
  what the tests exercise, and it keeps the workflow function replay-safe,
  because time only enters through the orchestration clock.
- **ContinueAsNew every 60 rounds.** A watch polls for hours, and Dapr replays
  the whole history on every step. The state travels in `Input.State`, under the
  same instance ID.
- **Observation errors are not fatal.** An API blip or missing RBAC shows up in
  the custom status. The clocks still run, so a watch that can never observe
  anything still ends at its timeout.
- **No client-go.** Three GETs and one server-side apply do not justify the
  dependency tree. [`kube.go`](kube.go) is about 100 lines over `net/http`.

## Not done yet

- **Not run on a live cluster as a workflow.** The unit tests cover the state
  machine, the parsers, the sinks, the machinery client (in-process gRPC
  server over bufconn) and the RGD's CEL payload (both branches, evaluated with
  cel-go). The machinery client and mapping were run read-only against the
  real machinery endpoint (`go test -tags live -run TestLiveMachinery`, see
  [`machinery_live_test.go`](machinery_live_test.go)). Not covered: a full
  workflow run under Dapr, a real Argo CD Application, a real Teams webhook,
  kro itself.
- **Chaining from `backstage-template-execution`.** After its merge step, that
  workflow knows the merge SHA. It could start this one, with
  `gitops.revision` filled in, instead of a human writing the CR. Cross-app
  start works with `WithDetachedWorkflowAppID`.
- **Status on the CR.** The kro CR shows only the trigger Job. Projecting the
  status ConfigMap into `ClusterBuildWatch.status` (kro `externalRef`) would make
  `kubectl get clusterbuildwatch` show the stage.
- **Checks on the built cluster** (nodes Ready, Argo CD apps on the target
  healthy) as checkpoints after `ready`.
