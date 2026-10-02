# cluster-build-watch

Dapr workflow that **watches** a cluster build end to end and reports every
checkpoint it crosses: GitOps sync → the Crossplane XR's stages → Ready.

It does not drive anything. The build runs as it does today (Backstage → PR →
merge → Argo CD / Flux → Crossplane); this workflow follows it, notices when a
stage hangs, and tells people and systems about it.

```
 gitops-sync ──► xr-pending ──► vm ──► baseos ──► distribution ──► kubeconfig ──► access ──► platform ──► [argo-sync] ──► Ready
 (Argo CD/Flux    (XR not yet     └──────────── status.stage of the ClusterStack XR ────────────┘   (the cluster's
  applied the      created)                                                                           Argo CD Apps
  merge commit)                                                                                       Synced+Healthy)
```

`argo-sync` is optional and only runs for a stack that registers the cluster
with Argo CD, see [Argo checkpoint](#argo-checkpoint).

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
| `ready` | success | the XR is done (and, with `argo`, the cluster's Applications are Synced + Healthy), with every stage's duration |
| `failed` | error | the overall timeout passed, the XR was deleted mid-build, or `argo` found no Application after its grace period |

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
| `argo-sync` | 30 | | | |

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
independent: if Teams or homerun2 is down, the ConfigMap and the webhook still
get the checkpoint. A failed report never stops the watch.

| Sink | Configured by | Content |
|---|---|---|
| Status ConfigMap | always (worker namespace, or `STATUS_NAMESPACE`) | `cluster-build-watch.<namespace>.<name>` with `phase`, `stage`, `message`, `lastEvent`, `stages` (JSON with durations). Label `cluster-build-watch.sthings.io/phase` |
| Microsoft Teams | `TEAMS_WEBHOOK_URL` | Adaptive Card, colour by severity, stage durations on the final card |
| Any HTTP endpoint | `STATUS_WEBHOOK_URL` | a CloudEvent (`io.sthings.clusterbuild.<event>`). The `id` is stable per checkpoint, so a receiver can drop the duplicate a retry sends |
| homerun2 | `HOMERUN_PITCH_URL` (+ token) | a `homerun.Message` POSTed to omni-pitcher `/pitch`, see [homerun2](#homerun2) |
| Dapr | always | the workflow's custom status, e.g. `[Watching] baseos: stage baseos` |

```bash
kubectl -n cluster-build-watch get cm -l app.kubernetes.io/managed-by=cluster-build-watch \
  -L cluster-build-watch.sthings.io/phase
kubectl -n cluster-build-watch get cm cluster-build-watch.cluster-build-watch.u26-kind1 \
  -o jsonpath='{.data.stage}{"  "}{.data.message}{"\n"}'
```

Webhook URLs are credentials (Teams signs them in the query string). They come
from a Secret, and a transport error never echoes them (`redactURL`).

### homerun2

With `HOMERUN_PITCH_URL` set, every checkpoint is pitched to homerun2's
omni-pitcher (`POST /pitch`, `Authorization: Bearer <token>`), decided in
dapr-workflows#49:

| homerun.Message | from the checkpoint |
|---|---|
| `title` | `<name>: <event>`, e.g. `u26-kind1: ready` (target name when `name` is empty) |
| `message` | the event message; on `ready`/`failed` plus `Stages: vm 19m0s, baseos 12m3s, …` |
| `severity` | unchanged (`info`/`success`/`warning`/`error` are all homerun2 severities) |
| `author`, `system` | `cluster-build-watch` |
| `tags` | `cluster-build,<event>,<stage>,<target namespace>/<name>` |
| `timestamp` | the event time, RFC 3339 |
| `url` | not set: the watch input carries none |

| Env | |
|---|---|
| `HOMERUN_PITCH_URL` | e.g. `https://omni.platform.sthings-vsphere.labul.sva.de/pitch`. Empty = sink off |
| `HOMERUN_AUTH_TOKEN_FILE` | file with omni-pitcher's `AUTH_TOKEN`; read per report, wins over |
| `HOMERUN_AUTH_TOKEN` | the token itself |

- **Stream `messages`.** No routing rule in omni-pitcher, so the pitch lands
  on homerun2's default stream. The LED and light catchers read it too: the
  office lights react to build checkpoints.
- **Teams goes through homerun2.** notification-catcher on platform-sthings
  posts the Adaptive Card (`match: {system: cluster-build-watch}`). Leave
  `TEAMS_WEBHOOK_URL` empty while the homerun sink is on, or every checkpoint
  reaches Teams twice. The direct Teams sink stays for setups without homerun2.
- **TLS.** The token is only ever sent over https; a plain-http URL with a
  token is refused with an error, and the token is scrubbed from every error.
  The pitcher's certificate is verified against `SSL_CERT_FILE`, the
  trust-manager bundle, which therefore has to carry the pitcher's CA. On
  cicd-machinery-test5 the LabUL CA (`infra.sthings-vsphere.labul.sva.de`) is a
  source of `cluster-trust-bundle` for exactly this.
- **No dedupe.** omni-pitcher does not deduplicate. When any sink fails, the
  Report activity is retried as a whole (3 attempts), so a checkpoint can
  reach homerun2, and thus Teams, more than once. The CloudEvent sink has a
  stable `id` for that; homerun.Message has no such field.
- **No homerun-library import.** The body is a local struct with
  homerun.Message's JSON names. The library pulls the Redis clients into a
  worker that never talks to Redis, and its HTTP client sends `X-Auth-Token`
  where omni-pitcher expects a bearer token.

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

**Not supported:** `gitops.kind: argocd` with `source: machinery`; it is
rejected when the watch starts. (machinery now serves `Application` for the
[Argo checkpoint](#argo-checkpoint), but the gitops Argo path is not wired to
it.)

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

## Argo checkpoint

After the XR is ready, a stack that registers its cluster with Argo CD
(`spec.rancher.argocd.register: true`) is not done yet: Argo CD still has to
install the platform profiles on it. With an `argo` block the watch enters
`argo-sync` and waits until every Application generated for the cluster is
`Synced` **and** `Healthy`.

```json
"argo": {
  "source": "machinery",
  "machinery": {"server": "machinery-grpc.<argo cd cluster domain>:443"}
}
```

See [`input-argo.json`](input-argo.json) and
[`trigger/examples/watch-argo.yaml`](trigger/examples/watch-argo.yaml).

| Field | Default | |
|---|---|---|
| `argo.project` | `target.name` | the AppProject = the cluster name |
| `argo.namespace` | `argocd` | namespace of the Applications |
| `argo.includeDefaultProject` | `true` | also count `default`-project Applications named `*-<project>` |
| `argo.always` | `false` | run even when the stack's register flag is unknown |
| `argo.graceMin` | `15` | how long zero matching Applications counts as "not generated yet" |
| `argo.settlePolls` | `2` | consecutive all-green polls, with an unchanged count, before done |
| `argo.source` | — | must be `machinery` |
| `argo.machinery.server` / `.kind` / `.plaintext` | — / `Application` / `false` | a machinery **on the Argo CD cluster** |

**Read through machinery only, never the kube API.** The worker runs on a
different cluster than Argo CD and has no credentials there, by design.
`argo.source: kube` is rejected at start.

**Which Applications.** Generated Applications carry no cluster label. The
handle is the AppProject: the `cluster-projects` ApplicationSet creates one
per registered cluster, named after it, and the generated Applications sit in
it (`spec.project`; inner names are partly `<component>-<sha1(server)[:8]>`, so
the name is no handle). A few outer ones sit in `default` instead:
`cert-manager-install-<cluster>`, `trust-manager-install-<cluster>` and
`proj-<cluster>`; `includeDefaultProject` picks those up by suffix. When
another project's name is the longer suffix match (`proj-app-dev` vs project
`dev`), the Application belongs to that one. machinery cannot filter by field,
so the worker lists every Application (`GetResources`, kind `Application`) and
filters on the `Project` info field. Live on platform-sthings (2026-10-02):
`app-dev` 84 Applications (81 in the project, 3 in `default`),
`homerun2-dev2` 50.

**When it runs.** Only when `argo` is set **and** the XR's `ArgoRegister`
(`spec.rancher.argocd.register`) is `true`. `false` finishes at XR ready as
before, with "not registered with Argo CD, argo checkpoint skipped" in the
ready message. An unknown flag (machinery does not map it, or the XRD has
none) is skipped the same way and says so; `argo.always` overrides.

**Done** means every matching Application `Synced` + `Healthy`, on
`settlePolls` consecutive observations with the same count. One green poll is
not enough: generation is staged (an app-of-apps creates its children after it
has synced), so the first all-green poll can come before the rest exists.

**Zero Applications.** Waited for up to `graceMin`; after that the watch
**fails**. A registered cluster with nothing in its project means the wrong
Argo CD, the wrong project, or a machinery that does not serve `Application`
(machinery answers an empty list for a configured kind whose CRD the cluster
does not serve, not an error). Waiting longer fixes none of these.

**Degraded / stuck.** `Degraded` health, a sync operation `Failed`/`Error`,
or an `*Error` condition (`ComparisonError`, `SyncError`, …) on any matching
Application is reported as `degraded` (and `recovered`) on the transition.
Warnings such as `OrphanedResourceWarning` are not errors. Past the
`argo-sync` limit (30 min) a `stuck` event names the Applications still
waiting, with their sync/health state.

**What machinery has to provide** (stuttgart-things/flux
`cicd/machinery/watch-config.yaml`): kind `Application` with info fields
`Project` (`spec.project`), `Sync` (`status.sync.status`), `Health`
(`status.health.status`), `Operation` (`status.operationState.phase`), and on
the machinery cluster's `ClusterStack` the info field `ArgoRegister`. Missing
`Sync`/`Health` read as not ready: such a server waits and goes stuck rather
than passing.

**Reachability.** The machinery for the checkpoint runs on the Argo CD
cluster (flux component `machinery-argocd`, plus `machinery-grpcroute`). Its
gateway certificate must verify against the worker's trust bundle and its
Gateway must negotiate ALPN `h2` (see [Source machinery](#source-machinery)).
On test5 the bundle carries the LabDA root only: LabDA `sthings-platform`
verifies, LabUL `platform-sthings` (where `app-dev` and `homerun2-dev2`
register today) does not.

## Run locally

The worker reads the API through `KUBE_API_SERVER` when it is set, so
`kubectl proxy` against any cluster is enough:

```bash
kubectl proxy --port 8001 &

export KUBE_API_SERVER=http://127.0.0.1:8001
export STATUS_NAMESPACE=default          # where the status ConfigMap goes
export TEAMS_WEBHOOK_URL='https://...'   # optional
export STATUS_WEBHOOK_URL='https://...'  # optional
export HOMERUN_PITCH_URL='https://.../pitch' HOMERUN_AUTH_TOKEN_FILE=...  # optional

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
  [`watch.go`](watch.go) (and `AdvanceArgo` for the `argo-sync` stage):
  observation in, events out, no Dapr, no HTTP. That is
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
- **Checks on the built cluster** (nodes Ready) as a checkpoint after
  `ready`. The Argo CD half is the [Argo checkpoint](#argo-checkpoint); not
  yet run as a workflow end to end (the client and filter were run read-only
  against a local machinery on platform-sthings, `go test -tags live -run
  TestLiveArgo`).
