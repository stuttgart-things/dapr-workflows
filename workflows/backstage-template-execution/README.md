# backstage-template-execution

Dapr workflow that triggers a Backstage scaffolder template, polls the task,
watches the resulting GitHub Actions run on the opened PR, and optionally
merges the PR.

## Run locally

You need **two shells**: one for the Dapr sidecar + worker, one to trigger runs.

### Prerequisites

- `dapr` CLI initialized (`dapr init`)
- Go toolchain
- A Backstage instance reachable from your machine
- GitHub PAT with `repo` scope (for the `watch` + merge step)
- Backstage service token (if your Backstage requires auth)

### Shell 1 — sidecar + worker

Export the required secrets, then start the worker under a Dapr sidecar.

> **Important:** these env vars must be exported in the *same shell* that runs
> `dapr run -- go run .`. The worker process reads `GITHUB_TOKEN` and
> `BACKSTAGE_AUTH_TOKEN` at activity-execution time via `os.Getenv`, so if they
> are only set in the trigger shell the workflow will fail with
> `GITHUB_TOKEN env var not set in worker process`.

```bash
export BACKSTAGE_AUTH_TOKEN='<backstage-service-token>'
export GITHUB_TOKEN='<gh-pat-with-repo-scope>'

# Optional: skip TLS verification against self-signed Backstage
export BACKSTAGE_INSECURE_TLS=true

cd backstage-template-execution

dapr run \
  --app-id backstage-template-execution \
  --app-protocol grpc \
  --dapr-grpc-port 50011 \
  --dapr-http-port 3500 \
  -- go run .
```

Wait for the log line `worker ready — use run.sh to start a workflow`.

Notes:

- Do **not** pass `--resources-path ./deploy` — that directory contains the
  KCL manifests used to render the k8s deployment, not Dapr component YAMLs.
  Without the flag, Dapr loads the default components from `~/.dapr/components/`
  (`statestore.yaml`, `pubsub.yaml`) which is what the workflow needs.
- If you see `Port 3500 is not available` / `Port 50001 is not available`,
  another Dapr app is already running. Either stop it (`dapr stop --app-id ...`)
  or pick different ports as shown above (`3510` / `50011`). Remember to export
  the same HTTP port for shell 2: `export DAPR_HTTP_PORT=3510`.

### Shell 2 — trigger a run

`run.sh` posts `input-vm-only.json` (default) to the sidecar's workflow API and then polls the
instance until it reaches a terminal state.

```bash
cd backstage-template-execution

# Must match --dapr-http-port used in shell 1
export DAPR_HTTP_PORT=3510

./run.sh                          # uses input-vm-only.json (create VM)
./run.sh input-vm-ansible.json    # VM + Ansible chain
./run.sh input-ansible-only.json  # Ansible against an existing target
./run.sh input-vm-delete.json     # delete VM via delete-terraform-vm template
./run.sh status run-<ts>          # re-check a previous instance
```

Edit any of the `input-*.json` files to change the target template,
`values`, and the `watch.branch` that the workflow tails on GitHub Actions.

Note: neither `GITHUB_TOKEN` nor `BACKSTAGE_AUTH_TOKEN` are needed in shell 2.
Both are read by the worker process in shell 1 — `BACKSTAGE_AUTH_TOKEN` is
used as the fallback when the input JSON's `authToken` field is empty (see
`main.go:210`), and `GITHUB_TOKEN` is read by the `FetchGitHubRun` /
`MergePullRequest` activities at run time.

## Required environment variables

| Var | Where | Purpose |
|---|---|---|
| `BACKSTAGE_AUTH_TOKEN` | worker shell | Bearer token for Backstage scaffolder API |
| `GITHUB_TOKEN` | worker shell | Used by `FetchGitHubRun` and `MergePullRequest` activities |
| `BACKSTAGE_INSECURE_TLS` | worker shell (optional) | `true` to skip TLS verify |
| `DAPR_HTTP_PORT` | trigger shell | Must match `--dapr-http-port` from shell 1 (default `3500`) |

## Troubleshooting

**`GITHUB_TOKEN env var not set in worker process`**
You exported the token in the trigger shell but not in the shell running
`dapr run -- go run .`. Stop the worker, `export GITHUB_TOKEN=...` in shell 1,
and restart it. Verify with `echo $GITHUB_TOKEN` in that same shell before
starting Dapr.

If you're *sure* you exported it in shell 1 and still see this error, you
probably have **stale worker processes** from previous `dapr run` invocations.
`kill`ing `daprd` does not kill its `go run` child — the old worker keeps
running, stays subscribed to the actor/workflow backend (Redis), and steals
activities from your new worker. Since the old worker was started before you
fixed the env, its `os.Getenv("GITHUB_TOKEN")` returns empty and the activity
fails.

Find and kill all leftover workers:

```bash
pgrep -af 'go-build.*backstage-template-execution'
# verify which ones lack the token:
for pid in $(pgrep -f 'go-build.*backstage-template-execution'); do
  echo -n "$pid: "; tr '\0' '\n' < /proc/$pid/environ | grep -c '^GITHUB_TOKEN='
done
# kill the ones that aren't your current worker:
kill -9 <stale-pids>
```

Only the worker you just started (the one whose env contains `GITHUB_TOKEN`)
should remain.

**`invalid configuration for HTTPPort. Port <N> is not available`**
Something is already bound to that port — usually a previous `dapr run` that
didn't shut down cleanly, or another app using the default `3500` / `50001`.
Find and stop the holder, then retry:

```bash
dapr list                                       # find the app holding the port
dapr stop --app-id backstage-template-execution
# or, if dapr doesn't know about it:
lsof -iTCP:3510 -sTCP:LISTEN                    # find the PID
kill <pid>
```

Or just pick different free ports and update shell 2's `DAPR_HTTP_PORT` to
match:

```bash
dapr run \
  --app-id backstage-template-execution \
  --app-protocol grpc \
  --dapr-grpc-port 50021 \
  --dapr-http-port 3520 \
  -- go run .
```

**`A non-YAML Component file kcl.mod was detected`**
You passed `--resources-path ./deploy`. Drop the flag — `deploy/` holds KCL
manifests, not Dapr components. The defaults in `~/.dapr/components/` are what
the workflow needs.
