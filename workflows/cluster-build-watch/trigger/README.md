# cluster-build-watch/trigger

A [kro](https://kro.run) `ResourceGraphDefinition` that starts one
`ClusterBuildWatchWorkflow` run per `ClusterBuildWatch` CR. It has the same
shape and the same start-once guarantees as the
[backstage-template-execution trigger](../../backstage-template-execution/trigger/README.md):
a `ConfigMap` with the input and the start script, and a `Job` that runs the
script, which POSTs the input to the worker's dapr sidecar.

## Install

```bash
kubectl apply -f rgd.yaml
kubectl get crd clusterbuildwatches.kro.run
```

The release publishes it like the backstage RGD:
`oci://ghcr.io/stuttgart-things/dapr-cluster-build-watch-kustomize:<release-tag>-trigger`.

## Start a watch

```bash
kubectl apply -f examples/watch-clusterstack.yaml
```

To read the build through a machinery gRPC endpoint instead of the worker's
own API server (e.g. a ClusterStack on the machinery cluster), set
`source: machinery` and `machinery.server` inside `target` and/or `gitops`:

```bash
kubectl apply -f examples/watch-machinery.yaml
```

The two keys sit inside the `target` and `gitops` objects, which kro passes
through to the worker as they are. So they need no `default=""` and no
`has()` in the RGD, and a CR without them renders the same input as before.
Argo CD (`gitops.kind: argocd`) cannot be read through machinery; the worker
rejects that combination at start.

Only `target.namespace` and `target.name` are required. Everything else is
described in the [workflow README](../README.md#start-a-watch). Unlike a
`BackstageTemplateRun`, a `ClusterBuildWatch` only reads, so applying the
example is harmless.

kro reconciles the CR into:

- `ConfigMap/<name>-input`: the JSON input, the deadline and `start.sh`
- `Job/<name>-<hash>` (`.status.jobName`): runs `start.sh`, which starts
  instance `<namespace>_<name>`, once. The CR name may be at most 54
  characters.

The worker then writes `ConfigMap/cluster-build-watch.<namespace>.<name>` in
its own namespace and keeps it current.

## One CR, one run

These rules are inherited unchanged:

- The instance ID is `<namespace>_<name>`. The Job starts it only on a clean
  `ERR_INSTANCE_ID_NOT_FOUND`, so a recreated Job does not start a second
  watch, and nobody gets every notification twice.
- There is no `ttlSecondsAfterFinished`, because kro would recreate the Job.
- `notAfter` stops a CR kept in git from starting again on a rebuilt cluster
  with an empty state store.
- An RGD update leaves existing CRs healthy and starts nothing: the script
  lives in the ConfigMap, and what can still change in the Job (the CR's
  `triggerImage`, the marker `trigger-v1`) is hashed into its name. Details and
  the upgrade from the old layout:
  [RGD updates](../../backstage-template-execution/trigger/README.md#rgd-updates).

To watch the same build again, delete the CR, purge the instance, and
re-apply:

```bash
curl -X POST "http://<sidecar>:3500/v1.0-beta1/workflows/dapr/<namespace>_<name>/purge"
```

## What the CR does not show

The CR's `status` holds only the trigger Job's result. The build's own progress
is in the status ConfigMap, in Teams and on the webhook. Projecting it into the
CR is listed under "Not done yet" in the [workflow README](../README.md).
