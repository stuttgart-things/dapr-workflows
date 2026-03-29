# Dapr CLI Setup

## Prerequisites

- Docker (for local development containers)
- Go 1.24+

## Install Dapr CLI

```bash
# Linux (amd64)
wget -q https://raw.githubusercontent.com/dapr/cli/master/install/install.sh -O - | /bin/bash

# macOS (Homebrew)
brew install dapr/tap/dapr-cli
```

## Initialize Dapr Locally

```bash
dapr init
```

This starts the following Docker containers:

| Container | Purpose |
|-----------|---------|
| `dapr_redis` | State store + pub/sub for workflows |
| `dapr_zipkin` | Distributed tracing |
| `dapr_placement` | Actor placement service |
| `dapr_scheduler` | Scheduled job execution |

## Verify Installation

```bash
dapr --version
# CLI version: 1.17.0
# Runtime version: 1.17.3

docker ps --format "table {{.Names}}\t{{.Status}}"
# dapr_redis       Up ...
# dapr_zipkin      Up ...
# dapr_placement   Up ...
# dapr_scheduler   Up ...
```

## Default Components (Local)

After `dapr init`, default component definitions are created at `~/.dapr/components/`:

| File | Type | Backend |
|------|------|---------|
| `statestore.yaml` | state.redis | localhost:6379 |
| `pubsub.yaml` | pubsub.redis | localhost:6379 |

These are automatically loaded when running apps with `dapr run`.

## Running a Dapr App Locally

```bash
dapr run --app-id myapp --app-port 8080 -- go run main.go
```

## Useful Commands

```bash
dapr list                    # List running Dapr apps
dapr dashboard               # Open Dapr dashboard (localhost:8080)
dapr stop --app-id myapp     # Stop a running app
dapr uninstall               # Remove Dapr from local environment
```

## Next Steps

Once Dapr is running locally, proceed to scaffold the golden-image-build workflow.
