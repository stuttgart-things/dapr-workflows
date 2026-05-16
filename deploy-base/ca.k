"""
Optional custom CA bundle for workflow workers.

Workers that call HTTPS endpoints signed by an internal CA need the
CA cert mounted + `SSL_CERT_FILE` pointed at it. This module renders
a ConfigMap (or refers to an existing Secret) and exposes the matching
volume / volumeMount / env entries so the worker Deployment can wire
them in without repeating the glue.
"""

import k8s.api.core.v1 as corev1

schema CABundleConfig:
    enable?: bool = False
    source?: str = "configmap"   # "configmap" | "secret"
    namespace: str
    name?: str = "ca-bundle"
    fileName?: str = "ca.pem"
    mountPath?: str = "/etc/ssl/ca"
    envName?: str = "SSL_CERT_FILE"
    data?: str = ""              # only used when source == "configmap"

buildCAConfigMap = lambda cfg: CABundleConfig -> corev1.ConfigMap {
    corev1.ConfigMap {
        metadata = {
            name = cfg.name
            namespace = cfg.namespace
        }
        data = {
            "${cfg.fileName}" = cfg.data
        }
    }
}

caVolume = lambda cfg: CABundleConfig -> any {
    {
        name = cfg.name
        configMap = {name = cfg.name}
    } if cfg.source == "configmap" else {
        name = cfg.name
        secret = {secretName = cfg.name}
    }
}

caVolumeMount = lambda cfg: CABundleConfig -> any {
    {
        name = cfg.name
        mountPath = cfg.mountPath
        readOnly = True
    }
}

caEnv = lambda cfg: CABundleConfig -> any {
    {
        name = cfg.envName
        value = "${cfg.mountPath}/${cfg.fileName}"
    }
}
