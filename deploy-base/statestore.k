"""
Dapr state store Component backed by Redis.

Used by workflow workers to persist workflow/actor state. The Redis
password is read from a Kubernetes Secret via `secretKeyRef` — the
caller is expected to create that Secret separately (see secrets.k).
"""

schema RedisStateStoreSpec:
    name?: str = "statestore"
    namespace: str
    redisHost: str
    passwordSecret: str
    passwordKey?: str = "password"
    actorStateStore?: bool = True

buildRedisStateStore = lambda spec: RedisStateStoreSpec -> {str:any} {
    {
        apiVersion = "dapr.io/v1alpha1"
        kind = "Component"
        metadata = {
            name = spec.name
            namespace = spec.namespace
        }
        spec = {
            type = "state.redis"
            version = "v1"
            metadata = [
                {name = "redisHost", value = spec.redisHost}
                {name = "redisPassword", secretKeyRef = {name = spec.passwordSecret, key = spec.passwordKey}}
                {name = "actorStateStore", value = "true" if spec.actorStateStore else "false"}
            ]
        }
    }
}
