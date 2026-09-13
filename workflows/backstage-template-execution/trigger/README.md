# backstage-template-execution/trigger

A [kro](https://kro.run) `ResourceGraphDefinition` that bundles the
`ConfigMap` (workflow input) and the `Job` (curl → dapr sidecar) used
to trigger a `BackstageTemplateWorkflow` run. One ~15-line
`BackstageTemplateRun` CR replaces ~60 lines of raw CM+Job boilerplate.

## Prerequisites

1. `kro` installed in the cluster (e.g. via `flux/infra/kro`).
2. The `backstage-template-execution` worker Deployment running with its
   daprd sidecar. The trigger Job reaches it through `sidecarService`, whose
   default is the fully qualified
   `backstage-template-execution-dapr.backstage-workflows.svc.cluster.local`,
   so the CR itself can live in any namespace that is allowed to reach it.
   Override `sidecarService` only if the worker runs somewhere else.

The CR carries no secrets and no lab-specific plumbing. `BACKSTAGE_AUTH_TOKEN`,
`GITHUB_TOKEN` and `BACKSTAGE_URL` all come from the worker's environment.

## Install the RGD

The RGD is applied by hand, not by Flux. Re-apply it after changing `rgd.yaml`:

```bash
kubectl apply -f rgd.yaml
kubectl get crd backstagetemplateruns.kro.run
```

Once the CRD exists you can `kubectl explain backstagetemplaterun.spec`
to see the generated schema.

## Trigger a run

Only `templateRef` and `values` are required:

```yaml
apiVersion: kro.run/v1alpha1
kind: BackstageTemplateRun
metadata:
  name: create-vm-demo-1        # also the Job name — unique per run
  namespace: backstage-workflows
spec:
  templateRef: template:default/create-terraform-vm
  dryRun: true                  # the default; set false for a real run
  values:
    lab: LabDA
    cloud: vsphere
    vm_name: demo-1
    # ... plus whatever the template requires that has no schema default
```

`values` only needs what differs from the template's own schema defaults. The
worker fills in every `default:` the CR leaves out, following the `oneOf` branch
the chosen values select, and never overwrites a value the CR sets — see
[Schema defaults are filled in by the worker](../README.md#schema-defaults-are-filled-in-by-the-worker).

`backstageURL` is left out on purpose: the worker uses its own `BACKSTAGE_URL`,
which the cluster sets per lab. Set it in the CR only to override a single run.

For a real run set `dryRun: false`, and usually add a `watch` block so the
workflow follows the GitHub Actions run on the PR and, optionally, merges it:

```yaml
  watch:
    owner: stuttgart-things
    repo: stuttgart-things
    workflowFile: pr-vm-deploy.yaml
    branch: vsphere-vm-demo-1-labda
    timeoutMin: 30
    merge:
      enabled: true
      method: squash
```

Without `watch` the workflow stops once the scaffolder task completes. Complete
examples: [`examples/create-vsphere-vm-labda.yaml`](./examples/create-vsphere-vm-labda.yaml)
(LabDA / vSphere) and [`examples/create-terraform-vm.yaml`](./examples/create-terraform-vm.yaml)
(LabUL / Proxmox).

```bash
kubectl apply -f examples/create-vsphere-vm-labda.yaml
```

kro reconciles it into:

- `ConfigMap/<name>-input` — holds the JSON-serialized input
- `Job/<name>` — curls the dapr sidecar and POSTs
  `/v1.0-beta1/workflows/dapr/BackstageTemplateWorkflow/start`

## One CR, one run

The workflow instance ID is the CR's `<namespace>_<name>`, e.g.
`backstage-workflows_create-vm-demo-1`. The trigger Job starts it only when the
sidecar reports that no such instance exists, so a name runs once, whatever
happens to the CR or the Job afterwards:

- A Job that kro recreates — deleted by hand, or its CR deleted and re-applied
  by Flux — finds the instance and exits without starting anything. Its log
  says `already exists, not starting it again`.
- Changing the CR's `spec` does not start a new run either: the Job has already
  run, and a Job's pod template is immutable.
- The Job has **no** `ttlSecondsAfterFinished` and stays Complete until the CR
  is deleted. A TTL made kro recreate the Job every five minutes, and before
  the existence check every recreation started a new run — 237 scaffolder tasks
  in 20 hours on labda-dev-a.
- If the sidecar cannot be asked, the Job fails instead of starting blind.
- Deleting the CR removes the ConfigMap and the Job — nothing else. A workflow
  instance that is already running keeps going, and whatever it created (PR,
  VM) stays. To stop an instance, terminate it through the sidecar (below).

To run the same thing again, create a CR under a new name. To reuse a name,
purge its instance first (below).

The memory behind this is the Dapr state store. If the instance has been
purged — or the cluster was rebuilt with an empty Redis — a CR that is still in
git starts a new run when Flux applies it again.

That is what `notAfter` is for. It is the part of the memory that travels with
the CR: past that UTC deadline the Job logs `notAfter … has passed` and starts
nothing, and a value it cannot parse fails the Job. Whatever renders CRs into
git should stamp one — the `request-vm` Backstage template in
stuttgart-things sets seven days after the request.

## Watching status

The CR's `.status` mirrors the trigger Job, so it only says whether the POST to
the sidecar succeeded:

```bash
kubectl -n backstage-workflows get backstagetemplaterun
kubectl -n backstage-workflows logs job/create-vm-demo-1   # started, or already exists
```

For the actual workflow progress tail the worker logs:

```bash
kubectl -n backstage-workflows logs -l app=backstage-template-execution \
  -c workflow -f
```

Or ask the sidecar about a specific instance. Both containers in the worker pod
are distroless — there is no shell, `wget` or `curl` to `exec` into — so go
through a port-forward:

```bash
kubectl -n backstage-workflows port-forward deploy/backstage-template-execution 3500:3500 &

ID=backstage-workflows_create-vm-demo-1                    # <namespace>_<name>
curl -s "http://localhost:3500/v1.0-beta1/workflows/dapr/$ID"
curl -s -X POST "http://localhost:3500/v1.0-beta1/workflows/dapr/$ID/terminate"
curl -s -X POST "http://localhost:3500/v1.0-beta1/workflows/dapr/$ID/purge"   # forget it, so the name can run again
```

## Schema reference

| Field | Type | Default | Notes |
|---|---|---|---|
| `templateRef` | string | *required* | e.g. `template:default/create-terraform-vm` |
| `values` | object | *required* | Free-form map — shape depends on the template. Schema defaults are filled in by the worker |
| `dryRun` | boolean | `true` | Worker stops before any mutating step |
| `watch` | object | *optional* | GitHub Actions watch config, see below. Omit to stop after the scaffolder task |
| `notAfter` | string | `""` | UTC deadline, `YYYY-MM-DDTHH:MM:SSZ`. Past it the Job starts nothing. Empty means no deadline |
| `backstageURL` | string | `""` | Empty means the worker's `BACKSTAGE_URL` |
| `workflowName` | string | `BackstageTemplateWorkflow` | Dapr workflow name registered by the worker. The payload is fixed to this workflow's input shape, so `GateAndMergeWorkflow` is **not** reachable through this CR |
| `sidecarService` | string | `backstage-template-execution-dapr.backstage-workflows.svc.cluster.local` | In-cluster DNS of the daprd sidecar service |
| `sidecarPort` | integer | `3500` | Dapr HTTP port |
| `triggerImage` | string | `curlimages/curl:8.11.1` | Job image |

`watch` fields:

| Field | Notes |
|---|---|
| `owner`, `repo` | Repository the template opens its PR against |
| `workflowFile` | GitHub Actions workflow that runs on the PR branch, e.g. `pr-vm-deploy.yaml` |
| `branch` | PR branch the template creates |
| `timeoutMin` | How long to wait for that run before the instance fails |
| `merge.enabled`, `merge.method` | Merge the PR once its checks are green; `squash` \| `merge` \| `rebase` |

Do not set `watch.notBefore` — the workflow stamps it itself, so that a run left
over from an earlier attempt on the same branch is not taken for this one.

`values` and `watch` are untyped (`object`) by design — the RGD is
template-agnostic. Nothing is validated at admission time; a wrong value
surfaces in the worker or in Backstage.
