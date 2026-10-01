"""
cluster-build-watch — Dapr workflow worker.

Composed from the shared deploy_base module, in the same three concerns as
backstage-template-execution, plus the one this worker adds:

  1. Core deployment       — the worker pod (worker.buildDaprWorker)
  2. Env / secrets wiring  — REDIS_PASSWORD, TEAMS_WEBHOOK_URL,
                             STATUS_WEBHOOK_URL (Secrets + secretKeyRef)
  3. RBAC                  — READ the objects it watches cluster-wide,
                             WRITE only ConfigMaps in its own namespace
  4. TLS trust + machinery — the cluster's trust-manager bundle as
                             SSL_CERT_FILE, an optional machinery token
"""

import k8s.api.core.v1 as corev1
import k8s.api.rbac.v1 as rbacv1
import deploy_base.worker as w
import deploy_base.statestore as s
import deploy_base.secrets as sec
import deploy_base.ca as ca
import manifests

# ═══════════════════════════════════════════════════════════════════════
# Identity + image
# ═══════════════════════════════════════════════════════════════════════
_name = "cluster-build-watch"
_namespace = "cluster-build-watch"

# Injectable so the release pipeline renders this base for the exact commit it
# just built: kcl run main.k -D imageTag=<short-sha>
_imageRepo: str = option("imageRepo") or "ghcr.io/stuttgart-things/dapr-cluster-build-watch"
# Placeholder until the first release: there is no image under this name yet.
# The release job always passes imageTag, so a published base never carries it.
_imageTag: str = option("imageTag") or "unreleased"
_image = "${_imageRepo}:${_imageTag}"

# ═══════════════════════════════════════════════════════════════════════
# Backends
# ═══════════════════════════════════════════════════════════════════════
# The Dapr state store. Differs per cluster (cicd-machinery-test5:
# redis-stack.dapr-redis:6379), so it is an option, not a constant.
_redisHost: str = option("redisHost") or "redis-stack.homerun2-flux.svc.cluster.local:6379"

# The kro trigger Job starts workflows by POSTing to <app-id>-dapr:3500 from
# its OWN pod. Dapr's default binds the sidecar HTTP API to loopback only, so
# without this every start fails with "connection refused" (first e2e run on
# cicd-machinery-test5, dapr-workflows#49).
_sidecarListenAddresses: str = option("sidecarListenAddresses") or "0.0.0.0"

# ═══════════════════════════════════════════════════════════════════════
# What the worker may read
# ═══════════════════════════════════════════════════════════════════════
# One rule per watched kind, `get` only: the worker addresses every object by
# name, so it needs neither list nor watch. Add an XR group here before
# pointing a ClusterBuildWatch at it -- a missing grant is a 403, which the
# watch reports as "observe failed" until its timeout, never as "not found".
#
# Only source kube uses these grants. A watch with source machinery on both
# sides reads nothing from this API server, and a worker that only ever runs
# such watches could drop the ClusterRole; the status Role below stays either
# way. Kept by default so source kube works out of the box.
_readRules = [
    {apiGroups = ["argoproj.io"], resources = ["applications"], verbs = ["get"]}
    {apiGroups = ["kustomize.toolkit.fluxcd.io"], resources = ["kustomizations"], verbs = ["get"]}
    {apiGroups = ["config.stuttgart-things.com"], resources = ["clusterstacks"], verbs = ["get"]}
]

# ═══════════════════════════════════════════════════════════════════════
# 1. Namespace
# ═══════════════════════════════════════════════════════════════════════
namespace = corev1.Namespace {
    metadata = {
        name = _namespace
        labels = {
            "toolkit.fluxcd.io/tenant" = "sthings-team"
        }
    }
}

# ═══════════════════════════════════════════════════════════════════════
# 2. Secrets + env wiring
# ═══════════════════════════════════════════════════════════════════════
# Placeholders so `kcl run main.k` is directly applyable for local dev. In flux
# they are deleted with kustomize $patch: delete and replaced by SOPS-encrypted
# Secrets, as for backstage-template-execution.
#
# Both webhook URLs are OPTIONAL: an empty value switches that sink off, and
# the status ConfigMap is written regardless. The URLs are credentials (Teams
# signs them in the query string), hence Secrets rather than plain env.
redisSecret = sec.buildOpaqueSecret("redis-password", _namespace, {
    password = "REPLACE_WITH_REDIS_PASSWORD"  # pragma: allowlist secret
})
notifySecret = sec.buildOpaqueSecret("notify-webhooks", _namespace, {
    teams = ""
    webhook = ""
})

_secretEnv = [
    sec.secretKeyRefEnv("TEAMS_WEBHOOK_URL", "notify-webhooks", "teams")
    sec.secretKeyRefEnv("STATUS_WEBHOOK_URL", "notify-webhooks", "webhook")
]

# ═══════════════════════════════════════════════════════════════════════
# 3. Dapr state store (Redis)
# ═══════════════════════════════════════════════════════════════════════
stateStore = s.buildRedisStateStore(s.RedisStateStoreSpec {
    namespace = _namespace
    redisHost = _redisHost
    passwordSecret = "redis-password"  # pragma: allowlist secret
})

# ═══════════════════════════════════════════════════════════════════════
# 4. TLS trust (source machinery, and every HTTPS sink)
# ═══════════════════════════════════════════════════════════════════════
# A machinery endpoint behind the lab gateway presents a certificate of the
# lab CA, which the image's system pool does not know. trust-manager
# distributes a bundle (system CAs + lab CAs) as a ConfigMap into the
# namespace; mounting it and pointing SSL_CERT_FILE at it makes Go trust it.
#
# SSL_CERT_FILE REPLACES the system pool for every TLS client in the worker,
# Teams and the CloudEvent webhook included. Fine with a trust-manager bundle,
# which carries the system CAs; do not point it at a bundle with lab CAs only.
#
# On by default. The ConfigMap is NOT rendered here (trust-manager owns it);
# without it in the namespace the pod stays in ContainerCreating, which is
# louder than a TLS error per poll. -D trustBundle=false switches it off,
# -D trustBundleConfigMap=<name> / trustBundleKey=<key> point elsewhere.
_trustBundle: bool = option("trustBundle", type="bool", default=True)
_trustCfg = ca.CABundleConfig {
    enable = _trustBundle
    source = "configmap"
    namespace = _namespace
    name = option("trustBundleConfigMap") or "cluster-trust-bundle"
    fileName = option("trustBundleKey") or "trust-bundle.pem"
    mountPath = "/etc/ssl/cluster-trust-bundle"
}
_trustVolumes = [ca.caVolume(_trustCfg)] if _trustCfg.enable else []
_trustVolumeMounts = [ca.caVolumeMount(_trustCfg)] if _trustCfg.enable else []
_trustEnv = [ca.caEnv(_trustCfg)] if _trustCfg.enable else []

# Optional bearer token for a machinery server with auth enabled. Off by
# default (machinery runs without auth today). -D machineryTokenSecret=<name>
# mounts key `token` of that Secret -- provided by you, e.g. SOPS, never
# rendered here -- and the worker reads it per call from
# MACHINERY_AUTH_TOKEN_FILE. A file rather than secretKeyRef env, because env
# is frozen at pod start and a mounted Secret is not: a rotated token takes
# effect without a restart.
_machineryTokenSecret: str = option("machineryTokenSecret") or ""
_machineryTokenDir = "/var/run/secrets/machinery"
_tokenVolumes = [{
    name = "machinery-token"
    secret = {secretName = _machineryTokenSecret, items = [{key = "token", path = "token"}]}
}] if _machineryTokenSecret else []
_tokenVolumeMounts = [{
    name = "machinery-token"
    mountPath = _machineryTokenDir
    readOnly = True
}] if _machineryTokenSecret else []
_tokenEnv = [{
    name = "MACHINERY_AUTH_TOKEN_FILE"
    value = "${_machineryTokenDir}/token"
}] if _machineryTokenSecret else []

# ═══════════════════════════════════════════════════════════════════════
# 5. RBAC
# ═══════════════════════════════════════════════════════════════════════
serviceAccount = corev1.ServiceAccount {
    metadata = {name = _name, namespace = _namespace}
}

clusterRole = rbacv1.ClusterRole {
    metadata.name = "${_name}-read"
    rules = _readRules
}

clusterRoleBinding = rbacv1.ClusterRoleBinding {
    metadata.name = "${_name}-read"
    roleRef = {apiGroup = "rbac.authorization.k8s.io", kind = "ClusterRole", name = "${_name}-read"}
    subjects = [{kind = "ServiceAccount", name = _name, namespace = _namespace}]
}

# Server-side apply needs create (first write) and patch (every later one).
# Scoped to this namespace: the status ConfigMaps live next to the worker,
# not next to the CR, so the worker never needs write access elsewhere.
statusRole = rbacv1.Role {
    metadata = {name = "${_name}-status", namespace = _namespace}
    rules = [{apiGroups = [""], resources = ["configmaps"], verbs = ["get", "create", "patch"]}]
}

statusRoleBinding = rbacv1.RoleBinding {
    metadata = {name = "${_name}-status", namespace = _namespace}
    roleRef = {apiGroup = "rbac.authorization.k8s.io", kind = "Role", name = "${_name}-status"}
    subjects = [{kind = "ServiceAccount", name = _name, namespace = _namespace}]
}

# ═══════════════════════════════════════════════════════════════════════
# 6. Worker Deployment
# ═══════════════════════════════════════════════════════════════════════
deployment = w.buildDaprWorker(w.WorkerSpec {
    name = _name
    namespace = _namespace
    appID = _name
    image = _image
    serviceAccountName = _name
    sidecarListenAddresses = _sidecarListenAddresses
    env = _secretEnv + _trustEnv + _tokenEnv
    volumes = _trustVolumes + _tokenVolumes
    volumeMounts = _trustVolumeMounts + _tokenVolumeMounts
})

# ═══════════════════════════════════════════════════════════════════════
# Render
# ═══════════════════════════════════════════════════════════════════════
manifests.yaml_stream([
    namespace
    redisSecret
    notifySecret
    stateStore
    serviceAccount
    clusterRole
    clusterRoleBinding
    statusRole
    statusRoleBinding
    deployment
])
