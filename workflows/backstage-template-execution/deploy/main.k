"""
backstage-template-execution — Dapr workflow worker.

Composed from the shared deploy_base module. Three concerns are kept
explicitly separated so a downstream consumer (e.g. flux) can strip or
replace any one of them without touching the others:

  1. Core deployment       — the worker pod (worker.buildDaprWorker)
  2. Env / secrets wiring  — GITHUB_TOKEN, BACKSTAGE_AUTH_TOKEN,
                             REDIS_PASSWORD (via Secrets + secretKeyRef)
  3. Optional CA bundle    — custom CA for self-signed Backstage TLS
"""

import k8s.api.core.v1 as corev1
import deploy_base.worker as w
import deploy_base.statestore as s
import deploy_base.secrets as sec
import deploy_base.ca as ca
import manifests

# ═══════════════════════════════════════════════════════════════════════
# Identity + image
# ═══════════════════════════════════════════════════════════════════════
_name = "backstage-template-execution"
_namespace = "backstage-workflows"

# Repo and tag are injectable so the release pipeline can render this base
# for the exact commit it just built:
#   kcl run main.k -D imageTag=<short-sha>
# The defaults keep a plain `kcl run main.k` working for local dev.
_imageRepo: str = option("imageRepo") or "ghcr.io/stuttgart-things/dapr-backstage-template-execution"
_imageTag: str = option("imageTag") or "49b147e70413"
_image = "${_imageRepo}:${_imageTag}"
_replicas = 1

# ═══════════════════════════════════════════════════════════════════════
# Backends
# ═══════════════════════════════════════════════════════════════════════
_redisHost = "redis-stack.homerun2-flux.svc.cluster.local:6379"

# ═══════════════════════════════════════════════════════════════════════
# Feature flags
# ═══════════════════════════════════════════════════════════════════════
# _enableCA and _insecureTLS are mutually exclusive in practice —
# pick exactly one when talking to a self-signed Backstage.
_enableCA: bool = True
_insecureTLS: bool = False

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
# These placeholder Secrets are intentionally rendered so `kcl run main.k`
# produces a complete, directly-applyable manifest for local dev. In flux,
# they are deleted via kustomize $patch: delete and replaced with SOPS-
# encrypted secrets under flux/apps/dapr/components/template-execution/.
githubSecret = sec.buildOpaqueSecret("github-token", _namespace, {
    token = "REPLACE_WITH_GITHUB_TOKEN"
})
backstageSecret = sec.buildOpaqueSecret("backstage-auth-token", _namespace, {
    token = "REPLACE_WITH_BACKSTAGE_AUTH_TOKEN"
})
redisSecret = sec.buildOpaqueSecret("redis-password", _namespace, {
    password = "REPLACE_WITH_REDIS_PASSWORD"  # pragma: allowlist secret
})

_secretEnv = [
    sec.secretKeyRefEnv("GITHUB_TOKEN", "github-token", "token")
    sec.secretKeyRefEnv("BACKSTAGE_AUTH_TOKEN", "backstage-auth-token", "token")
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
# 4. Optional: custom CA bundle for Backstage TLS
# ═══════════════════════════════════════════════════════════════════════
_caCfg = ca.CABundleConfig {
    enable = _enableCA
    source = "configmap"
    namespace = _namespace
    name = "backstage-ca"
    fileName = "ca.pem"
    mountPath = "/etc/ssl/backstage"
    data = """-----BEGIN CERTIFICATE-----
REPLACE_WITH_PEM_ENCODED_CA_CERTIFICATE
-----END CERTIFICATE-----
"""
}

_caConfigMap = ca.buildCAConfigMap(_caCfg) if _caCfg.enable and _caCfg.source == "configmap" else Undefined
_caVolumes = [ca.caVolume(_caCfg)] if _caCfg.enable else []
_caVolumeMounts = [ca.caVolumeMount(_caCfg)] if _caCfg.enable else []
_caEnvVars = [ca.caEnv(_caCfg)] if _caCfg.enable else []

_insecureEnv = [{name = "BACKSTAGE_INSECURE_TLS", value = "true"}] if _insecureTLS else []

# ═══════════════════════════════════════════════════════════════════════
# 5. Worker Deployment
# ═══════════════════════════════════════════════════════════════════════
deployment = w.buildDaprWorker(w.WorkerSpec {
    name = _name
    namespace = _namespace
    appID = _name
    image = _image
    replicas = _replicas
    env = _secretEnv + _caEnvVars + _insecureEnv
    volumes = _caVolumes
    volumeMounts = _caVolumeMounts
})

# ═══════════════════════════════════════════════════════════════════════
# Render
# ═══════════════════════════════════════════════════════════════════════
_baseDocs = [
    namespace
    githubSecret
    backstageSecret
    redisSecret
    stateStore
    deployment
]
_docs = _baseDocs + [_caConfigMap] if _caCfg.enable and _caCfg.source == "configmap" else _baseDocs

manifests.yaml_stream(_docs)
