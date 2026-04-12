# dapr-workflows

Dapr workflow orchestration for infrastructure automation — golden image builds, provisioning, and more.

## Overview

This repository contains Dapr durable workflows that orchestrate infrastructure automation tasks. Workflows run as Go services on Kubernetes with Dapr sidecar injection, calling [Dagger modules](https://github.com/stuttgart-things/blueprints) as building blocks.

## Workflows

| Workflow | Description | Status |
|----------|-------------|--------|
| golden-image-build | Render → packer build → test VM → promote golden image | Planned |
| backstage-template-execution | Trigger Backstage scaffolder templates, poll the task to completion, optionally watch a follow-up GitHub Actions run, and auto-merge the resulting PR | Working |

### backstage-template-execution

End-to-end orchestration around a Backstage scaffolder template:

1. POST to `/api/scaffolder/v2/tasks` (or `/dry-run`) to start the template
2. Poll the scaffolder task until it reaches `completed` / `failed` / `cancelled`
3. Optionally poll a GitHub Actions workflow run on a deterministic branch (the activity skips runs that completed with conclusion `skipped`, e.g. PRs that fired before labels were applied)
4. Optionally squash/merge/rebase-merge the PR once the GH run reaches `success`

Inputs are passed as JSON; the worker reads `GITHUB_TOKEN` from its environment for the GH-watch + merge activities. See [`backstage-template-execution/input.json`](backstage-template-execution/input.json) (create flow) and [`backstage-template-execution/input-delete.json`](backstage-template-execution/input-delete.json) (delete flow) for examples wired against the `create-terraform-vm` and `delete-terraform-vm` Backstage templates.

```bash
cd backstage-template-execution
export DAPR_SERVICE_TOKEN=...   # Backstage scaffolder token
export GITHUB_TOKEN=...         # PAT with repo scope (actions:read + pull_requests:write)
dapr run --app-id backstage-tpl --dapr-http-port 3500 -- go run .

# in another terminal
./run.sh                  # uses input.json
./run.sh input-delete.json
./run.sh status <id>
```

## Getting Started

### Install Dapr CLI

```bash
wget -q https://raw.githubusercontent.com/dapr/cli/master/install/install.sh -O - | /bin/bash
dapr init
```

See [docs/setup/dapr-cli.md](docs/setup/dapr-cli.md) for details.

## Author

Patrick Hermann, stuttgart-things (2025-2026)

## License

Licensed under the Apache License 2.0.
