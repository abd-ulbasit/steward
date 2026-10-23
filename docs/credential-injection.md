# Credential Injection

How an application container reaches the database, cache, and queue that the
operator provisioned for it.

## Problem

Without injection, a developer has to hand-write a `secretKeyRef` entry for every
credential field — host, port, user, password, database name — for every
component. That is five to twenty lines of boilerplate per Application, it
duplicates naming conventions the operator already owns, and getting one key name
wrong produces a crash-looping pod with no useful error. It also defeats the
premise of the abstraction: the developer would have to know how the operator
names its Secrets.

## Approach

The controller injects well-known environment variables into workload containers,
derived from which infrastructure is declared in the Application spec. Values are
always `secretKeyRef` references to the provider-created Secrets, never literals
— the plaintext never enters the Deployment manifest, so `kubectl get deployment
-o yaml` does not leak credentials.

The alternative considered was a projected volume of credential files. Env vars
won because every language's standard database driver reads `DATABASE_URL` /
`REDIS_URL` / `AMQP_URL` with no application changes, which is the whole point.

## CRD Changes

Two new fields on `WorkloadSpec`:

- `injectCredentials` (`*bool`, default `true`) — opt-out toggle for auto-injection
- `envFrom` (`[]corev1.EnvFromSource`) — bulk-mount external Secrets/ConfigMaps

## Injected Env Vars

### Database (postgres)
`DATABASE_URL`, `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE`

### Database (mysql)
`DATABASE_URL`, `MYSQL_HOST`, `MYSQL_PORT`, `MYSQL_USER`, `MYSQL_PASSWORD`, `MYSQL_DATABASE`

### Cache (redis)
`REDIS_URL`, `REDIS_HOST`, `REDIS_PORT`, `REDIS_PASSWORD`

### Queue (rabbitmq)
`AMQP_URL`, `RABBITMQ_HOST`, `RABBITMQ_PORT`, `RABBITMQ_USER`, `RABBITMQ_PASSWORD`

All values come from `secretKeyRef` pointing to the provider-created credential Secrets.

## Precedence

User-defined env vars in `spec.workload.env` always win. The controller skips injecting any var the user already defined.

## Secret Naming

Unchanged from existing KubernetesProvider conventions:
- Database: `{app.Name}-db-credentials`
- Cache: `{app.Name}-cache-credentials`
- Queue: `{app.Name}-queue-credentials`

## Where the plaintext lives

Only in those three Secrets. The operators read the same Secrets instead of a
copy of the password:

| Component | How the operator gets the password |
|---|---|
| CNPG `Cluster` | `spec.bootstrap.initdb.secret.name` |
| `RedisFailover` | `spec.auth.secretPath` |
| `RabbitmqCluster` | `spec.secretBackend.externalSecret.name`; the operator mounts the `default_user.conf` key (RabbitMQ Cluster Operator v2.2.0 or later) |

The password is not written into the Deployment, an operator CR, a ConfigMap
rendered from a CR, or `Application.status`. `status.infrastructure.queue.url`
is `amqp://<host>:<port>` without credentials.

Releases before this change put the RabbitMQ password into
`RabbitmqCluster.spec.rabbitmq.additionalConfig` and copied the full AMQP URL,
password included, into `Application.status`. The next reconcile removes both.
The RabbitMQ operator then re-renders its `<app>-queue-server-conf` ConfigMap
without the password and rolls the broker pods once. The broker keeps its
existing users because they are stored on its persistent volume, and the
credentials Secret still holds the same password. The operator stops managing
the `<app>-queue-default-user` Secret it generated earlier; it still holds a
copy of the password and can be deleted.
