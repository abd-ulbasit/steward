# GoPlatform

A Kubernetes operator that turns a 20-line `Application` manifest into a running
service with a Postgres cluster, injected credentials, autoscaling, a pod
disruption budget, and tier-appropriate Prometheus alerts — then keeps all of it
matching the manifest. Redis and RabbitMQ come from the same three lines each.

```yaml
apiVersion: platform.platform.goplatform.io/v1alpha1
kind: Application
metadata:
  name: payments-api
spec:
  team: payments
  owner: payments@example.com
  tier: critical
  workload:
    image: ghcr.io/org/payments-api:v1.0.0
    ports:
      - name: http
        containerPort: 8080
  scaling:
    minReplicas: 3
    maxReplicas: 12
  database:
    type: postgres
    highAvailability: true
```

`kubectl apply` on that produces:

| | Created |
|---|---|
| Workload | Deployment, Service, ConfigMap, Secret, HorizontalPodAutoscaler (3–12), PodDisruptionBudget |
| Database | CloudNativePG `Cluster` with 3 instances — `tier: critical` makes `highAvailability` mandatory and HA means 3 — plus a credential Secret |
| Wiring | `DATABASE_URL`, `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE` injected into the container as `secretKeyRef`s — no hand-written `secretKeyRef` blocks, no plaintext in the Deployment |
| Monitoring | `ServiceMonitor` scraping every 15s (60s if the tier were `development`) and a `PrometheusRule` with tier-scaled SLA and health alerts |

The developer never writes a `secretKeyRef`, never picks a scrape interval, and
never learns how CloudNativePG names its read-write Service.

---

## The part that was actually hard: telling drift from expected change

`controllerutil.CreateOrUpdate` returns `Created` or `Updated` in two situations
that mean opposite things — first-time provisioning, and overwriting somebody's
manual edit. The operation result alone cannot distinguish them, so a naive
implementation either announces "drift corrected" on every initial provision or
never announces it at all.

The discriminator used here is the spec generation:

```
drift == (a child was Created/Updated) AND (spec.generation == status.observedGeneration)
```

`observedGeneration` is written only at the end of a *successful* pass, so
reading it at the top of `Reconcile` yields the last generation fully reconciled.
If it still equals the current generation, the desired state is one already
applied — anything that needed correcting was changed from outside the operator.

The second half is avoiding false positives. `CreateOrUpdate` reports `Updated`
for harmless server-side defaulting on ConfigMaps, Secrets, HPAs, and PDBs. Left
alone, that fires a drift event every few minutes on an untouched cluster, and an
alert that cries wolf gets muted. So those kinds count only `Created` (meaning
"it was deleted and we put it back") as drift, while Deployment and Service
compare the specific fields the operator manages. Under-reporting was chosen
deliberately: a missed detection is still corrected on the next pass, it just is
not announced.

Infrastructure CRDs are handled differently again — CNPG `Cluster`,
`RedisFailover`, and `RabbitmqCluster` are owned by *their* operators, so drift
there is detected and reported but never force-corrected. An aggressive corrector
could fight a CNPG failover mid-promotion.

[docs/drift-detection.md](docs/drift-detection.md) has the mechanics;
[internal/controller/drift.go](internal/controller/drift.go) and
[internal/provider/kubernetes_drift.go](internal/provider/kubernetes_drift.go)
have the code.

---

## Architecture

```
kubectl apply
     │
     ▼
Kubernetes API server ── validating + defaulting webhooks ──► etcd
     │  watch (GenerationChangedPredicate)
     ▼
ApplicationReconciler
     ├── child resources ......... Deployment, Service, ConfigMap, Secret, HPA, PDB
     │                             (CreateOrUpdate, owner refs, drift-tracked)
     ├── InfrastructureProvider ... CNPG Cluster, RedisFailover, RabbitmqCluster, PVCs
     │                             (+ optional DriftDetector / CostEstimator / StateManager)
     └── monitoring .............. ServiceMonitor, PrometheusRule
                                   (skipped if Prometheus Operator CRDs are absent)
```

Infrastructure sits behind an `InfrastructureProvider` interface
(`Provision` / `GetStatus` / `Destroy` / `Healthy`) chosen by a factory at
startup. `KubernetesProvider` drives in-cluster operators; `MockProvider` backs
the controller tests. Optional capabilities — drift detection, cost estimation,
state management — are separate interfaces discovered by type assertion, so a
provider implements only what it can support.

Design rationale for all of the above, including the failure semantics:
**[docs/DESIGN-DECISIONS.md](docs/DESIGN-DECISIONS.md)**.

---

## Behaviour worth knowing before you rely on it

- **Partial failure does not roll back.** `Provision` returns partial state
  alongside its error, so status reports `DatabaseReady=True, CacheReady=False`
  rather than one opaque failure. The next pass retries what failed and leaves
  what succeeded. A healthy database is never destroyed because a queue failed.
- **Error type decides retry.** A `NotReadyError` requeues in 10s. An
  `InvalidConfigError` sets `Failed` and does **not** requeue — only a spec edit
  can fix it, and a spec edit triggers a reconcile on its own.
- **Removing a block is destructive.** Deleting `database:` from a live
  Application destroys the CNPG cluster. Optional blocks are pointers precisely
  so absent and present-and-empty are distinguishable, and absent means "clean
  up". The webhook blocks *changing* `database.type` (an unrecoverable engine
  swap) but permits removal, which can be intentional.
- **Deletion can get stuck, on purpose.** If cleanup keeps failing, the finalizer
  stays, a `DeletionStuck` warning is emitted after 30 minutes, and the object
  remains. Force-removing the finalizer would clear the object and orphan the
  infrastructure it was protecting.
- **`tier` is policy, not a label.** It drives HA enforcement at admission,
  scrape interval, and alert thresholds.

---

## Status

Implemented and tested against a real API server (envtest):

| Area | State |
|---|---|
| `Application` CRD (v1alpha1), OpenAPI validation, printer columns | done |
| Reconciler: 6 child resource kinds, conditions, phase, finalizer | done |
| Validating + defaulting webhooks (cross-field rules, immutability) | done |
| Provider abstraction + `KubernetesProvider` + `MockProvider` | done |
| Postgres (CloudNativePG), Redis (Spotahome), RabbitMQ (Cluster Operator) | done |
| Credential injection into workload containers | done |
| Controller metrics, `ServiceMonitor` + `PrometheusRule` generation | done |
| Drift detection + correction | done |
| `database: mysql`, `cache: memcached`, `queue: sqs`/`kafka` | schema accepts them; the Kubernetes provider rejects them with a typed `InvalidConfigError` naming the field |
| `storage` | provisions a PersistentVolumeClaim regardless of whether `type` is `s3` or `gcs` — there is no object-storage backend yet, and the spec shape is ahead of the implementation |
| Multi-version CRD + conversion webhook | not started |

Test coverage, from `make test` (Ginkgo + envtest running a real kube-apiserver
and etcd, not a fake client):

```
internal/webhook/v1alpha1   98.5%
internal/controller         75.2%
internal/provider           62.6%
```

128 Ginkgo specs — most against envtest — plus 45 table-driven Go tests, and a
Kind-based e2e suite under `test/e2e`. The provider number is the lowest because
its error and capability paths are exercised through `MockProvider` rather than
against a live CNPG install.

---

## Quick start

Requires Go, Docker, `kubectl`, and `kind`.

```bash
# Kind cluster + cert-manager + Prometheus Operator CRDs + CNPG + the Application CRD
make dev-setup

# Run the controller against the new cluster
make run
```

Against an existing Kubernetes 1.28+ cluster, `make install` alone installs the
CRD; the CNPG, Redis, and RabbitMQ operators must already be present for the
corresponding spec blocks to provision anything.

Then, in another shell:

```bash
kubectl apply -f config/samples/platform_v1alpha1_application.yaml
kubectl get applications -w
kubectl describe application application-sample
```

The sample runs a `psql` connectivity check against the database the operator
provisioned, using only the auto-injected environment variables — so a successful
pod log is end-to-end proof that provisioning and credential wiring both worked.

More manifests in [`examples/`](examples). Cluster setup detail in
[docs/dev-cluster-setup.md](docs/dev-cluster-setup.md).

---

## Development

```bash
make build          # build the manager binary
make test           # unit + envtest suites with coverage
make test-e2e       # e2e against an isolated Kind cluster
make lint           # golangci-lint v2
make manifests      # regenerate CRDs and RBAC from kubebuilder markers
make generate       # regenerate DeepCopy methods
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Documentation

- [docs/DESIGN-DECISIONS.md](docs/DESIGN-DECISIONS.md) — CRD schema rationale,
  idempotency strategy, drift model, ownership and finalizer semantics, partial
  failure behaviour, known limits
- [docs/drift-detection.md](docs/drift-detection.md) — watch propagation,
  correction vs detection, false-positive avoidance
- [docs/observability.md](docs/observability.md) — controller metrics vs
  generated application monitoring
- [docs/credential-injection.md](docs/credential-injection.md) — injected
  environment variables and precedence rules
- [docs/dev-cluster-setup.md](docs/dev-cluster-setup.md) — local Kind setup

## Built with

Go, kubebuilder v4, controller-runtime, CloudNativePG, Spotahome Redis Operator,
RabbitMQ Cluster Operator, Prometheus Operator CRDs, Ginkgo/Gomega + envtest,
Kind.

## License

MIT — see [LICENSE](LICENSE).
