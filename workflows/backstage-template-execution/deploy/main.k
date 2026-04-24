import k8s.api.core.v1 as corev1
import k8s.api.apps.v1 as appsv1
import manifests

_name = "backstage-template-execution"
_namespace = "backstage-workflows"
_appID = "backstage-template-execution"
_image = "ttl.sh/stuttgart-things/backstage-template-execution:1h"
_replicas = 1

_labels = {
    app = _name
}

_redisHost = "redis-stack.homerun2-flux.svc.cluster.local:6379"

# ── CA bundle mount ──────────────────────────────────────────────────────────
# Set _enableCA = True to trust a custom/self-signed CA (e.g. the Backstage
# ingress cert). Source is either "configmap" (default, PEM stored in
# _caConfigMapData) or "secret" (reference an existing Secret by name).
# SSL_CERT_FILE is set so Go's TLS stack picks it up.
_enableCA: bool = True # pragma: allowlist secret
_caSource: str = "configmap"  # "configmap" | "secret" # pragma: allowlist secret
_caConfigMapName = "backstage-ca"
_caSecretName = "backstage-ca" # pragma: allowlist secret
_caFileName = "ca.pem"
_caMountPath = "/etc/ssl/backstage"
_caConfigMapData = """-----BEGIN CERTIFICATE-----
REPLACE_WITH_PEM_ENCODED_CA_CERTIFICATE
-----END CERTIFICATE-----
"""

namespace = corev1.Namespace {
    metadata.name = _namespace
}

githubSecret = corev1.Secret {
    metadata = {
        name = "github-token"
        namespace = _namespace
    }
    type = "Opaque"
    stringData = {
        token = "REPLACE_WITH_YOUR_GITHUB_TOKEN"
    }
}

backstageSecret = corev1.Secret {
    metadata = {
        name = "backstage-auth-token"
        namespace = _namespace
    }
    type = "Opaque"
    stringData = {
        token = "REPLACE_WITH_BACKSTAGE_SERVICE_TOKEN"
    }
}

redisSecret = corev1.Secret {
    metadata = {
        name = "redis-password"
        namespace = _namespace
    }
    type = "Opaque"
    stringData = {
        password = "REPLACE_WITH_REDIS_PASSWORD" # pragma: allowlist secret
    }
}

caConfigMap = corev1.ConfigMap {
    metadata = {
        name = _caConfigMapName
        namespace = _namespace
    }
    data = {
        "${_caFileName}" = _caConfigMapData
    }
} if _enableCA and _caSource == "configmap" else Undefined

statestore = {
    apiVersion = "dapr.io/v1alpha1"
    kind = "Component"
    metadata = {
        name = "statestore"
        namespace = _namespace
    }
    spec = {
        type = "state.redis"
        version = "v1"
        metadata = [
            {name = "redisHost", value = _redisHost}
            {name = "redisPassword", secretKeyRef = {name = "redis-password", key = "password"}}
            {name = "actorStateStore", value = "true"}
        ]
    }
}

_caVolume = {
    name = "backstage-ca"
    configMap.name = _caConfigMapName
} if _caSource == "configmap" else {
    name = "backstage-ca"
    secret.secretName = _caSecretName
}

_baseEnv = [
    {
        name = "GITHUB_TOKEN"
        valueFrom.secretKeyRef = {
            name = "github-token"
            key = "token"
        }
    }
    {
        name = "BACKSTAGE_AUTH_TOKEN"
        valueFrom.secretKeyRef = {
            name = "backstage-auth-token"
            key = "token"
        }
    }
]

_caEnv = [
    {
        name = "SSL_CERT_FILE"
        value = "${_caMountPath}/${_caFileName}"
    }
] if _enableCA else []

_caVolumeMounts = [
    {
        name = "backstage-ca"
        mountPath = _caMountPath
        readOnly = True
    }
] if _enableCA else []

deployment = appsv1.Deployment {
    metadata = {
        name = _name
        namespace = _namespace
        labels = _labels
    }
    spec = {
        replicas = _replicas
        selector.matchLabels = _labels
        template = {
            metadata = {
                labels = _labels
                annotations = {
                    "dapr.io/enabled" = "true"
                    "dapr.io/app-id" = _appID
                    "dapr.io/enable-api-logging" = "true"
                }
            }
            spec = {
                volumes = [_caVolume] if _enableCA else []
                containers = [
                    {
                        name = "workflow"
                        image = _image
                        imagePullPolicy = "IfNotPresent"
                        env = _baseEnv + _caEnv
                        volumeMounts = _caVolumeMounts
                        resources = {
                            requests = {cpu = "50m", memory = "64Mi"}
                            limits = {cpu = "500m", memory = "256Mi"}
                        }
                    }
                ]
            }
        }
    }
}

_docs = [namespace, githubSecret, backstageSecret, redisSecret, statestore, deployment]
_docsWithCA = _docs + [caConfigMap] if _enableCA and _caSource == "configmap" else _docs

manifests.yaml_stream(_docsWithCA)
