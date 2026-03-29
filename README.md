# dapr-workflows

Dapr workflow orchestration for infrastructure automation — golden image builds, provisioning, and more.

## Overview

This repository contains Dapr durable workflows that orchestrate infrastructure automation tasks. Workflows run as Go services on Kubernetes with Dapr sidecar injection, calling [Dagger modules](https://github.com/stuttgart-things/blueprints) as building blocks.

## Workflows

| Workflow | Description | Status |
|----------|-------------|--------|
| golden-image-build | Render → packer build → test VM → promote golden image | Planned |

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
