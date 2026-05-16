"""
Dapr workflow worker Deployment.

A "worker" here is a pod that registers workflows/activities with its
Dapr sidecar and waits for requests from the sidecar's HTTP API. It has
no Service, no container port, no probes — just a Deployment with the
dapr.io annotations and whatever env/volumes the caller injects.
"""

import k8s.api.apps.v1 as appsv1

schema WorkerSpec:
    name: str
    namespace: str
    appID: str
    image: str
    replicas?: int = 1
    containerName?: str = "workflow"
    imagePullPolicy?: str = "IfNotPresent"
    env?: [any] = []
    volumes?: [any] = []
    volumeMounts?: [any] = []
    resources?: any = {
        requests = {cpu = "50m", memory = "64Mi"}
        limits = {cpu = "500m", memory = "256Mi"}
    }
    enableAPILogging?: bool = True

buildDaprWorker = lambda spec: WorkerSpec -> appsv1.Deployment {
    appsv1.Deployment {
        metadata = {
            name = spec.name
            namespace = spec.namespace
            labels = {app = spec.name}
        }
        spec = {
            replicas = spec.replicas
            selector.matchLabels = {app = spec.name}
            template = {
                metadata = {
                    labels = {app = spec.name}
                    annotations = {
                        "dapr.io/enabled" = "true"
                        "dapr.io/app-id" = spec.appID
                        "dapr.io/enable-api-logging" = "true" if spec.enableAPILogging else "false"
                    }
                }
                spec = {
                    volumes = spec.volumes
                    containers = [{
                        name = spec.containerName
                        image = spec.image
                        imagePullPolicy = spec.imagePullPolicy
                        env = spec.env
                        volumeMounts = spec.volumeMounts
                        resources = spec.resources
                    }]
                }
            }
        }
    }
}
