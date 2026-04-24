# Golden Image Workflow

Dapr workflow that orchestrates golden image builds by dispatching GitHub Actions workflows and polling for completion. The Dapr workflow acts as a pure orchestrator — all heavy lifting (Dagger, Packer, Ansible) runs on GitHub Actions runners.

## Architecture

```
POST /start (JSON) ──► Dapr Workflow ──► GH Actions: Render + Commit + PR
                                     ──► GH Actions: Packer Build
                                     ──► GH Actions: Test VM (optional)
                                     ──► GH Actions: Promote (optional)
                                     ──► GH Actions: Notify (optional)
```

Each step dispatches a `workflow_dispatch` event via the GitHub API, finds the triggered run, and polls until completion. Step results (run URLs) are captured in the workflow output for observability.

## Prerequisites

- [Dapr CLI](https://docs.dapr.io/getting-started/install-dapr-cli/) (for local testing)
- [Go 1.26+](https://go.dev/dl/)
- A GitHub token with `repo` + `actions:write` scopes
- Target repo with dispatch workflow files (see [examples/](examples/))

## Local Development

### Build

```bash
cd golden-image-workflow
go build -o golden-image-workflow .
```

### Initialize Dapr (first time only)

```bash
dapr init
```

### Run with JSON input

```bash
export GITHUB_TOKEN=ghp_your_token
dapr run --app-id golden-image -- ./golden-image-workflow --run --input examples/inputs/ubuntu24-labul.json
```

### Example input files

| File | Description |
|------|-------------|
| `examples/inputs/ubuntu24-labul.json` | Ubuntu 24 on labul (full pipeline) |
| `examples/inputs/ubuntu24-labda.json` | Ubuntu 24 on labda (full pipeline) |
| `examples/inputs/rocky9-labul.json` | Rocky 9 on labul (full pipeline) |
| `examples/inputs/vsphere-ubuntu26-labda.json` | Ubuntu 26 on labda (build + promote only) |
| `examples/inputs/vsphere-rocky9-labul.json` | Rocky 9 on labul (build + promote only) |
| `examples/inputs/vsphere-ubuntu26-labda-resume.json` | Resume mode: skip build, promote an existing template |

The GitHub token is read from the `GITHUB_TOKEN` environment variable (not stored in JSON).

### Workflows

Two workflows are registered:

| Name | Steps | Use when |
|------|-------|----------|
| `GoldenImageBuildWorkflow` (default) | render → build → (test) → (promote) → (notify) | You want the full pipeline from source rendering through notification |
| `VsphereTemplateWorkflow` | build → promote | Packer config is already committed; you just want to build and promote to golden |

Select the lean workflow with `--workflow`:

```bash
dapr run --app-id golden-image -- ./golden-image-workflow \
  --run --workflow VsphereTemplateWorkflow \
  --input examples/inputs/vsphere-ubuntu26-labda.json
```

`VsphereTemplateWorkflow` exposes the HTTP endpoint `/start-vsphere-template`:

```bash
curl -X POST http://localhost:8080/start-vsphere-template \
  -H "Content-Type: application/json" \
  -d @examples/inputs/vsphere-ubuntu26-labda.json
```

#### Template name handoff

The input JSON only carries the **target** golden image name (`promotion.targetName`, e.g. `sthings-u26`). The **source** template name — Packer's timestamped build artifact like `ubuntu26-base-20260423-1124` — doesn't exist yet at dispatch time. It flows between jobs at runtime:

1. The Packer build GH Actions workflow constructs the name (`<os>-<provisioning>-<YYYYMMDD-hhmm>`) and echoes `Template name: <value>` to the run log.
2. `PackerBuildActivity` fetches the run log after completion and extracts the value via `ExtractFromLog` (last match wins, to skip the shell-source line).
3. Dapr persists it as workflow state — if the worker crashes between build and promote, the resumed worker still has the name.
4. `PromoteActivity` passes it as the `template-name` input to `dispatch-packer-movetemplate.yaml`, which uses it as the rename source.

The workflow fails fast with `FailedStep: "PackerBuild"` if extraction returns empty, so a malformed log can't silently cascade into a broken rename.

#### Resilience: retries, adoption, and manual resume

Three safety nets prevent lost work when something goes wrong between "dispatch the build" and "promote the result":

1. **Dispatch retry.** `DispatchWorkflow` retries transient network errors and HTTP 5xx responses up to 3 times with exponential backoff (5s → 10s → 20s). 4xx responses (bad ref, missing inputs) are returned immediately — they won't resolve on retry.

2. **Dispatch adoption.** GitHub's dispatch endpoint is not strictly transactional — a 5xx response can still mean the run was queued. `DispatchAndFindRun` always polls for a recently-created run after dispatching, even when dispatch errored. If GH queued the run despite the error, the activity adopts it instead of giving up. This eliminates a whole class of orphan runs that previously accumulated in GH while Dapr had forgotten about them.

3. **Activity retry policy.** Both the Packer build and the Promote activities are wrapped in durable retry policies:
   - Packer build: 2 attempts, 30s initial backoff, 5m total timeout. Conservative to avoid kicking off a second long-running build.
   - Promote: 3 attempts, 10s initial backoff, 10m total timeout. Safe to retry — the promote workflow's `continue-on-error` on the delete step handles pre-existing templates.

4. **Manual resume mode.** For the case where a previous Dapr run terminated after GH had already started the Packer build (so a template exists in vCenter but Dapr has no memory of it), set `existingTemplateName` in the input JSON:

   ```json
   "existingTemplateName": "ubuntu26-base-os-20260423-1428"
   ```

   When set, the workflow skips the Packer build step entirely and jumps straight to promote with the given template name. See [examples/inputs/vsphere-ubuntu26-labda-resume.json](examples/inputs/vsphere-ubuntu26-labda-resume.json).

   Use case: Dapr terminated with `dispatch returned 500` but the GH build is still running or finished. Wait for the GH run to finish, note the template name from its summary, then re-run Dapr with that name set — you get the promote step without paying for a second Packer build.

### HTTP API (server mode)

```bash
dapr run --app-id golden-image --app-port 8080 -- ./golden-image-workflow
```

Then trigger via:

```bash
curl -X POST http://localhost:8080/start \
  -H "Content-Type: application/json" \
  -d @examples/inputs/ubuntu24-labul.json
```

## Kubernetes Deployment

### Prerequisites

- Kubernetes cluster with Dapr installed
- Redis instance for workflow state

### Install Dapr on the cluster

```bash
helm repo add dapr https://dapr.github.io/helm-charts
helm repo update
helm install dapr dapr/dapr --namespace dapr-system --create-namespace --wait
```

### Build and push container image

```bash
docker build -t ghcr.io/stuttgart-things/golden-image-workflow:latest .
docker push ghcr.io/stuttgart-things/golden-image-workflow:latest
```

### Deploy

1. Create the namespace:

```bash
kubectl apply -f deploy/namespace.yaml
```

2. Create secrets (do NOT commit real values — the files in `deploy/secret.yaml` are templates):

```bash
# GitHub token
kubectl create secret generic github-token \
  --from-literal=token=ghp_your_token \
  -n golden-image

# Redis password
printf 'your-redis-password' | kubectl create secret generic redis-password \
  --from-file=password=/dev/stdin \
  -n golden-image
```

3. Apply the state store component and deployment:

```bash
kubectl apply -f deploy/statestore.yaml
kubectl apply -f deploy/deployment.yaml
```

4. Verify:

```bash
kubectl get pods -n golden-image
# Should show 2/2 Running (app + Dapr sidecar)

kubectl logs -n golden-image -l app=golden-image-workflow -c workflow
# Should show "workflow worker started" and "listening on :8080"
```

### Trigger a workflow on K8s

```bash
# Port forward
kubectl port-forward -n golden-image svc/golden-image-workflow 8080:8080 &

# Trigger
curl -X POST http://localhost:8080/start \
  -H "Content-Type: application/json" \
  -d @examples/inputs/ubuntu24-labul.json
```

### Manifests

| File | Description |
|------|-------------|
| `deploy/namespace.yaml` | `golden-image` namespace |
| `deploy/secret.yaml` | Secret templates (replace values before applying) |
| `deploy/statestore.yaml` | Dapr Redis state store component |
| `deploy/deployment.yaml` | Deployment + Service with Dapr sidecar annotations |

## GitHub Actions Workflows

The Dapr workflow dispatches these GH Actions workflows in the target repo. Example workflow files are in [examples/](examples/):

| Workflow | Purpose | Example |
|----------|---------|---------|
| `dispatch-render-packer-config.yaml` | Render packer config, commit to branch, create PR | Already exists in stuttgart-things |
| `dispatch-packer-build-dagger.yaml` | Run Packer build via Dagger | [examples/dispatch-packer-build-dagger.yaml](examples/dispatch-packer-build-dagger.yaml) |
| `dispatch-packer-testvm-dagger.yaml` | Create test VM and validate | Already exists in stuttgart-things |
| `dispatch-packer-movetemplate.yaml` | Rename built template → delete any pre-existing golden → move into the golden folder | Already exists in stuttgart-things |

`PromoteActivity` dispatches `dispatch-packer-movetemplate.yaml` with `template-name`, `target-name`, `lab` (required — selects the vSphere build/golden folders), and optionally `build-folder`/`golden-folder`/`runner`. The `lab` input is threaded through from `input.Environment` in both `VsphereTemplateWorkflow` and `GoldenImageBuildWorkflow`; the GH workflow's `Resolve vSphere folders` step exits 1 if it's missing.

## Project Structure

```
golden-image-workflow/
├── main.go                 # Entrypoint: workflow worker + HTTP API + --run mode
├── workflow.go             # GoldenImageBuildWorkflow orchestration logic
├── Dockerfile              # Multi-stage build (distroless)
├── activities/
│   ├── render.go           # Dispatch render + commit GH Action
│   ├── packer_build.go     # Dispatch packer build GH Action
│   ├── test_vm.go          # Dispatch test VM GH Action
│   ├── promote.go          # Dispatch promote GH Action
│   ├── notify.go           # Dispatch notify GH Action
│   └── commit_pr.go        # Dispatch commit/PR GH Action (unused, merged into render)
├── github/
│   └── client.go           # GH REST API client (dispatch, find run, poll, wait)
├── types/
│   ├── input.go            # Workflow input schema
│   ├── output.go           # Workflow output schema
│   └── errors.go           # Transient/Permanent error types
├── deploy/
│   ├── namespace.yaml      # K8s namespace
│   ├── secret.yaml         # Secret templates
│   ├── statestore.yaml     # Dapr Redis state store
│   └── deployment.yaml     # K8s Deployment + Service
└── examples/
    ├── render-config.yaml                  # Example GH Actions render workflow
    ├── dispatch-packer-build-dagger.yaml   # Example GH Actions packer build workflow
    └── inputs/
        ├── ubuntu24-labul.json             # Example input: Ubuntu 24 / labul
        ├── ubuntu24-labda.json             # Example input: Ubuntu 24 / labda
        └── rocky9-labul.json               # Example input: Rocky 9 / labul
```
