# Dapr Workflows

Dapr workflow orchestration for infrastructure automation — golden image builds, provisioning, and more.

## Overview

This repository contains Dapr durable workflows that orchestrate infrastructure automation tasks. Each workflow is a standalone Go service deployed on Kubernetes with Dapr sidecar injection.

Workflows call existing [Dagger modules](https://github.com/stuttgart-things/blueprints) as building blocks via `dagger call` subprocess invocations, combining them into reliable, retryable pipelines with compensation logic.

## Workflows

| Workflow | Description | Status |
|----------|-------------|--------|
| golden-image-build | Render config, packer build, test VM, promote golden image | Planned |

## Architecture

```
Cron (Dapr binding) or API trigger
    ↓
Dapr Workflow Engine (state persisted in Redis)
    ↓
Activities (each wraps a `dagger call` subprocess)
    ↓
Dagger Modules (blueprints: vmtemplate, vm, packer)
```

## Getting Started

1. [Install the Dapr CLI](setup/dapr-cli.md)
2. Scaffold and run your first workflow (coming soon)

## Author

Patrick Hermann, stuttgart-things (2025-2026)

## License

Licensed under the Apache License 2.0.
