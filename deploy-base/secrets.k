"""
Helpers for Opaque Secrets and env-var wiring.

Workflow workers typically consume auth tokens via `GITHUB_TOKEN`-style
env vars sourced from Secrets. These helpers keep the caller's main.k
free of boilerplate.
"""

import k8s.api.core.v1 as corev1

buildOpaqueSecret = lambda name: str, namespace: str, stringData: {str:str} -> corev1.Secret {
    corev1.Secret {
        metadata = {
            name = name
            namespace = namespace
        }
        type = "Opaque"
        stringData = stringData
    }
}

secretKeyRefEnv = lambda envName: str, secretName: str, key: str -> any {
    {
        name = envName
        valueFrom = {
            secretKeyRef = {
                name = secretName
                key = key
            }
        }
    }
}
