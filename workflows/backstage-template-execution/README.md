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

cd workflows/backstage-template-execution   # from repo root

dapr run \
  --app-id backstage-template-execution \
  --app-protocol grpc \
  --dapr-grpc-port 50011 \
  --dapr-http-port 3500 \
  -- go run .
```

Wait for the log line `worker ready — use run.sh to start a workflow`.

#### Verify the Backstage token before starting Dapr

`BACKSTAGE_AUTH_TOKEN` is a **Bearer token validated by Backstage**, sent in
the `Authorization` header against `/api/scaffolder/v2/...`. It is **not** a
`DAPR_API_TOKEN` (which would protect the Dapr sidecar's own API and isn't
used by this worker). The token must come from Backstage — typically a static
token from `backend.auth.externalAccess[].options.token` in `app-config.yaml`,
or a JWT issued by Backstage's auth backend.

Smoke-test the token against the catalog before spinning Dapr up — saves a
round of debugging if the wrong token type was supplied:

```bash
# $BACKSTAGE_URL is what the worker itself uses — run this from inside the
# cluster (kubectl run --rm ... curlimages/curl) if you want the answer that
# matters. A name that resolves from your laptop may not resolve from the pod.
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $BACKSTAGE_AUTH_TOKEN" \
  -k "$BACKSTAGE_URL/api/catalog/entities/by-name/template/default/ansible-provisioning"
```

- `200` → token valid, template registered, you're good
- `401` / `403` → wrong token (not a Backstage token, expired, or lacks catalog/scaffolder permission)
- `404` → token valid but template not registered under `template:default/ansible-provisioning` at that Backstage URL
- `000` → the name does not resolve or the host is unreachable **from where you ran it**. Seen on a LabDA cluster against a LabUL endpoint: the run dies with a DNS timeout deep inside `CallScaffolder` and nothing on the cluster looks unhealthy

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

`run.sh` posts `input.json` to the sidecar's workflow API and then polls the
instance until it reaches a terminal state.

```bash
cd workflows/backstage-template-execution   # from repo root

# Must match --dapr-http-port used in shell 1
export DAPR_HTTP_PORT=3510

./run.sh                          # uses input.json (create VM)
./run.sh input-delete.json        # delete VM via delete-terraform-vm template
./run.sh input-ansible-kind.json  # dryRun ansible-provisioning (kind-cluster)
./run.sh status run-<ts>          # re-check a previous instance
```

Edit `input.json` / `input-delete.json` to change the target template,
`values`, and the `watch.branch` that the workflow tails on GitHub Actions.

Note: neither `GITHUB_TOKEN` nor `BACKSTAGE_AUTH_TOKEN` are needed in shell 2.
Both are read by the worker process in shell 1 — `BACKSTAGE_AUTH_TOKEN` is
used as the fallback when the input JSON's `authToken` field is empty (see
`main.go:210`), and `GITHUB_TOKEN` is read by the `FetchGitHubRun` /
`MergePullRequest` activities at run time.

### dryRun semantics

When the input has `"dryRun": true`, the worker:

1. Fetches the template entity from `/api/catalog/entities/by-name/...`
2. POSTs to `/api/scaffolder/v2/dry-run` with empty `directoryContents`
3. Returns immediately with `TaskID: "dry-run"` — **no polling, no GitHub
   Actions watch, no PR merge**

So a dryRun only needs `BACKSTAGE_AUTH_TOKEN`; `GITHUB_TOKEN` is irrelevant.
And because `directoryContents` is empty, server-side `fetch:template` steps
that reference `./content` will error — that's expected. The dryRun validates
auth, template registration, and parameter shape, not a full render.

**Successful dryRun output:**

```
Started: {"instanceID":"run-..."}
Watching status (ctrl+c to stop)...
  HH:MM:SS  RUNNING  dry-run complete: task dry-run
  HH:MM:SS  COMPLETED  dry-run complete: task dry-run

Status      : COMPLETED
Output:
{
  "taskId": "dry-run",
  "finalStatus": "completed",
  "dryRun": true
}
```

**Common dryRun failures:**

| Symptom | Likely cause |
|---|---|
| `HTTP 401` from Backstage | `BACKSTAGE_AUTH_TOKEN` wrong/expired or lacks scaffolder permission |
| `HTTP 404 fetch template` | Template not registered at that URL or wrong namespace in `templateRef` |
| `fetch:template` error inside dry-run | Expected — worker sends empty `directoryContents`. Auth + params validated up to that point |
| `HTTP 500 ENOENT: ... lstat '/tmp/dry-run-content-.../content'` | Same root cause as the row above, surfaced earlier (during the `fetch:template ./content` step). **Templates with a `fetch:template ./content` step cannot be fully dry-run** by this worker — Backstage tries to `lstat` the unpacked content dir, but `directoryContents` was empty so the dir doesn't exist. The dryRun confirmed auth + template registration; promote to a real run to actually exercise the template. |

> **Practical takeaway:** for any template that uses `fetch:template`
> (most do — including `ansible-provisioning`), dryRun via this worker only
> proves "Backstage accepts my token and finds the template". To validate
> parameter rendering, you have to do a real run via `/v2/tasks`. Backstage
> handles content fetching itself there — the worker doesn't need to bundle
> anything.

### Gotcha: parameter defaults aren't applied on API calls

Backstage's scaffolder only populates a template's schema `default:` values
when a user submits the template via the **UI form**. When you call
`/api/scaffolder/v2/tasks` directly (which this worker does), any parameter
you omit arrives at the template as **`undefined`**, even if its schema
declares a default. Nunjucks expressions like
`{% if "foo" in collections %}` then throw:

```
Error: Cannot use "in" operator to search for "foo" in unexpected types.
```

**Always pass every parameter you reference in template content explicitly
in your `values:` block, even if it just mirrors the schema default.** Easy
to crib defaults straight out of the template's `template.yaml`. Surfaces in
the task event log under the failing step.

### Inspecting a failed task

`run.sh` only surfaces the worker's own error string, which can be terse
(e.g. `task <id> → failed (step: )`). The full event stream — including
each step's stdout and the rendering engine's stack trace — lives in
Backstage. Two ways to get it:

- **UI:** `<backstageURL>/create/tasks/<taskId>`
- **API** (handy for grepping):

```bash
curl -sS -k \
  -H "Authorization: Bearer $BACKSTAGE_AUTH_TOKEN" \
  "$BACKSTAGE_URL/api/scaffolder/v2/tasks/<taskId>/events" \
  | jq -r '.[] | "[\(.type)] step=\(.body.stepId // "-") \(.body.message // "")"'
```

### Promoting a dryRun to a real run

1. Edit the input file: `"dryRun": false`
2. If you want auto-merge of the resulting PR: `"watch": { ..., "merge": { "enabled": true, "method": "squash" } }`
3. Confirm `watch.workflowFile` matches the GitHub Actions workflow that fires on the PR branch (for `ansible-provisioning-*` branches in `stuttgart-things/stuttgart-things`, that's `pr-ansible-provisioning.yaml`)
4. Re-run `./run.sh <input-file>`

### Token hygiene

`BACKSTAGE_AUTH_TOKEN` and `GITHUB_TOKEN` end up in the shell history of the
worker shell. To keep them out:

```bash
export HISTCONTROL=ignorespace
 export BACKSTAGE_AUTH_TOKEN='...'   # leading space → not recorded
 export GITHUB_TOKEN='...'
```

Or source them from a file outside the repo (e.g. `source ~/.envrc.backstage`).
Rotate both tokens immediately if they're ever pasted into a chat, logs, or
a PR description.

## Required environment variables

| Var | Where | Purpose |
|---|---|---|
| `BACKSTAGE_URL` | worker shell | Backstage base URL. Fallback when the workflow input omits `backstageURL` — see [Lab-agnostic inputs](#lab-agnostic-inputs) |
| `BACKSTAGE_AUTH_TOKEN` | worker shell | Bearer token for Backstage scaffolder API |
| `GITHUB_TOKEN` | worker shell | Used by `FetchGitHubRun` and `MergePullRequest` activities |
| `BACKSTAGE_INSECURE_TLS` | worker shell (optional) | `true` to skip TLS verify |
| `DAPR_HTTP_PORT` | trigger shell | Must match `--dapr-http-port` from shell 1 (default `3500`) |

## Lab-agnostic inputs

The Backstage endpoint is **per-lab and cluster-side**, exactly like the token
beside it. The worker reads `BACKSTAGE_URL` from its own environment whenever
the workflow input omits `backstageURL`, so the same input file runs on any
cluster. In Kubernetes it comes from `deploy/main.k` (`-D backstageURL=...`),
which flux sets per cluster.

Why it works this way: it used to be required in every input, and every input
file in this repo named the LabUL endpoint. On a LabDA cluster that name does
not resolve, and the run dies with

```
scaffolder call failed: dial tcp: lookup backstage.platform.sthings-vsphere.labul.sva.de
  on 10.43.0.10:53: i/o timeout
```

— inside a workflow run, where nothing on the cluster reports a problem. Same
shape as the CA bundle that was seeded from a shared Vault path.

Pass `backstageURL` in the input only to override a single run.

### What is still lab-specific — and has to be

Only the plumbing is lab-agnostic. Values inside `values` name real
infrastructure and belong to the caller:

| Example | Lab / cloud | Note |
|---|---|---|
| `input-vsphere-labda.json` | **LabDA / vSphere** | Use this while LabUL is down. Values taken from a real deployed VM (`stuttgart-things/terraform/vsphere/labda/cicd-machinery-test5`) |
| `input.json` | **LabUL / Proxmox** | `pve_api_url` points at `ul-pve01`. There is no LabDA Proxmox host in this fleet — only `ul-pve*` exists — so this one cannot be flipped |
| `input-ansible-kind.json` | anywhere the targets exist | `ansible-provisioning` writes config files only |

The template itself enforces the split: `LabDA` offers vSphere only, `LabUL`
offers Proxmox only (`template.yaml`, `dependencies.lab.oneOf`). So the lab
choice picks the cloud, and the watch branch follows it —
`vsphere-vm-<name>-labda` vs `proxmox-vm-<name>-labul`.

### Validating an input without building a VM

`create-terraform-vm` has a `fetch:template ./content` step, so a dryRun cannot
render it (see [dryRun semantics](#dryrun-semantics)) — it confirms auth and
template registration and nothing about your values. Validate the values
against the template's own schema instead:

```python
import yaml, json, jsonschema
tpl = yaml.safe_load(open("backstage/templates/create-terraform-vm/template.yaml"))
values = json.load(open("input-vsphere-labda.json"))["values"]
for g in tpl["spec"]["parameters"]:
    s = {k: v for k, v in g.items() if k in ("required", "properties", "dependencies")}
    if s:
        jsonschema.validate(values, {**s, "type": "object"})
```

This catches the failure mode these files actually have: a value that was
valid when written and has since dropped out of an enum. Both inputs here
were checked this way — `input.json` was **invalid** until this was run on it
(`s3_endpoint` still named the pre-rename `artifacts.demo-infra…` host,
`s3_bucket` was `state`, and the `sthings_collections` release had moved on).

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
