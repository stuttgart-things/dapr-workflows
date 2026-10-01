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

Built-in stage timeouts (minutes, override per stage with
`target.stageTimeoutMin`): `gitops-sync` 15, `xr-pending` 10, `xr-created`
10, `vm` 30, `baseos` 30, `distribution` 30, `kubeconfig` 10, `access` 10,
`platform` 45, anything else `target.defaultStageTimeoutMin` (30). The whole
watch fails after `timeoutMin` (240).

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
| `pollSeconds` | 30 | |
| `timeoutMin` | 240 | |

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
./run.sh status <id>
```

## Deploy

[`deploy/main.k`](deploy/main.k) renders the worker with its own ServiceAccount:

- **ClusterRole `cluster-build-watch-read`**: `get` on Argo CD Applications,
  Flux Kustomizations and `clusterstacks.config.stuttgart-things.com`. Add a
  rule there before watching another XR kind. A missing grant is a 403, which
  the watch reports as `observe failed` in its custom status, and never as
  "not found".
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

- **Not run on a live cluster.** The unit tests cover the state machine, the
  parsers, the sinks and the RGD's CEL payload (both branches, evaluated with
  cel-go). Not covered: a real Argo CD Application, a real `ClusterStack`, a
  real Teams webhook, kro itself.
- **Chaining from `backstage-template-execution`.** After its merge step, that
  workflow knows the merge SHA. It could start this one, with
  `gitops.revision` filled in, instead of a human writing the CR. Cross-app
  start works with `WithDetachedWorkflowAppID`.
- **Status on the CR.** The kro CR shows only the trigger Job. Projecting the
  status ConfigMap into `ClusterBuildWatch.status` (kro `externalRef`) would make
  `kubectl get clusterbuildwatch` show the stage.
- **Checks on the built cluster** (nodes Ready, Argo CD apps on the target
  healthy) as checkpoints after `ready`.
