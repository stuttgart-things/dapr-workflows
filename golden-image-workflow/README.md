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
| `examples/inputs/ubuntu24-labul.json` | Ubuntu 24 on labul |
| `examples/inputs/ubuntu24-labda.json` | Ubuntu 24 on labda |
| `examples/inputs/rocky9-labul.json` | Rocky 9 on labul |

The GitHub token is read from the `GITHUB_TOKEN` environment variable (not stored in JSON).

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
