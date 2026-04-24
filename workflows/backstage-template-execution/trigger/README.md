# backstage-template-execution/trigger

A [kro](https://kro.run) `ResourceGraphDefinition` that bundles the
`ConfigMap` (workflow input) and the `Job` (curl → dapr sidecar) used
to trigger a `BackstageTemplateWorkflow` run. One ~15-line
`BackstageTemplateRun` CR replaces ~60 lines of raw CM+Job boilerplate.

## Prerequisites

1. `kro` installed in the cluster (e.g. via `flux/infra/kro`).
2. The `dapr-backstage-template-execution` worker Deployment running
   in the target namespace — the RGD talks to its daprd sidecar
   service (`backstage-template-execution-dapr.<ns>.svc.cluster.local:3500`).

## Install the RGD

```bash
kubectl apply -f rgd.yaml
kubectl get crd backstagetemplateruns.kro.run
```

Once the CRD exists you can `kubectl explain backstagetemplaterun.spec`
to see the generated schema.

## Trigger a run

Create a namespaced `BackstageTemplateRun` instance (see
[`examples/create-terraform-vm.yaml`](./examples/create-terraform-vm.yaml)):

```yaml
apiVersion: kro.run/v1alpha1
kind: BackstageTemplateRun
metadata:
  name: create-vm-demo
  namespace: backstage-workflows
spec:
  backstageURL: https://backstage.platform.sthings-vsphere.labul.sva.de
  templateRef: template:default/create-terraform-vm
  dryRun: true
  values:
    lab: LabUL
    cloud: proxmox
    vm_name: dapr-monday-1
    # ... any template-specific values
  watch:
    owner: stuttgart-things
    repo: stuttgart-things
    workflowFile: pr-vm-deploy.yaml
    branch: proxmox-vm-dapr-monday-1-labul
    timeoutMin: 30
    merge:
      enabled: true
      method: squash
```

```bash
kubectl apply -f examples/create-terraform-vm.yaml
```

kro reconciles it into:

- `ConfigMap/create-vm-demo-input` — holds the JSON-serialized input
- `Job/create-vm-demo` — curls the dapr sidecar and POSTs
  `/v1.0-beta1/workflows/dapr/BackstageTemplateWorkflow/start`

The Job has `ttlSecondsAfterFinished: 300` so it self-cleans; deleting
the `BackstageTemplateRun` CR tears both resources down immediately.

## Watching status

The generated CR exposes Job status in its own `.status` block:

```bash
kubectl -n backstage-workflows get backstagetemplaterun
kubectl -n backstage-workflows logs job/create-vm-demo
```

For the actual workflow progress (beyond "did the trigger POST
succeed?") tail the worker pod logs:

```bash
kubectl -n backstage-workflows logs -l app=backstage-template-execution \
  -c workflow -f
```

Or poll the sidecar directly for a specific instance id:

```bash
kubectl -n backstage-workflows exec deploy/backstage-template-execution \
  -c daprd -- \
  wget -qO- "http://localhost:3500/v1.0-beta1/workflows/dapr/<instanceId>"
```

## Schema reference

| Field | Type | Default | Notes |
|---|---|---|---|
| `workflowName` | string | `BackstageTemplateWorkflow` | Dapr workflow name registered by the worker |
| `sidecarService` | string | `backstage-template-execution-dapr.backstage-workflows.svc.cluster.local` | In-cluster DNS of the daprd sidecar service |
| `sidecarPort` | integer | `3500` | Dapr HTTP port |
| `triggerImage` | string | `curlimages/curl:8.11.1` | Job image |
| `backstageURL` | string | *required* | Backstage base URL |
| `templateRef` | string | *required* | e.g. `template:default/create-terraform-vm` |
| `dryRun` | boolean | `true` | Worker stops before any mutating step |
| `values` | object | *required* | Free-form map — shape depends on the template |
| `watch` | object | *required* | GitHub Actions watch config |

`values` and `watch` are untyped (`object`) by design — the RGD is
template-agnostic. Field validation happens inside the worker, not at
admission time.
