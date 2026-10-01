# cluster-build-watch/trigger

A [kro](https://kro.run) `ResourceGraphDefinition` that starts one
`ClusterBuildWatchWorkflow` run per `ClusterBuildWatch` CR. It has the same
shape and the same start-once guarantees as the
[backstage-template-execution trigger](../../backstage-template-execution/trigger/README.md):
a `ConfigMap` with the input and a `Job` that POSTs it to the worker's dapr
sidecar.

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

Only `target.namespace` and `target.name` are required. Everything else is
described in the [workflow README](../README.md#start-a-watch). Unlike a
`BackstageTemplateRun`, a `ClusterBuildWatch` only reads, so applying the
example is harmless.

kro reconciles the CR into:

- `ConfigMap/<name>-input`: the JSON input
- `Job/<name>`: starts instance `<namespace>_<name>`, once

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

To watch the same build again, delete the CR, purge the instance, and
re-apply:

```bash
curl -X POST "http://<sidecar>:3500/v1.0-beta1/workflows/dapr/<namespace>_<name>/purge"
```

## What the CR does not show

The CR's `status` holds only the trigger Job's result. The build's own progress
is in the status ConfigMap, in Teams and on the webhook. Projecting it into the
CR is listed under "Not done yet" in the [workflow README](../README.md).
