# Steward

A Kubernetes operator that turns a 20-line `Application` manifest into a running
service with a Postgres cluster, injected credentials, autoscaling, a pod
disruption budget, and tier-appropriate Prometheus alerts — then keeps all of it
matching the manifest. Redis and RabbitMQ are two more blocks of the same shape.

```yaml
apiVersion: platform.steward.sh/v1alpha1
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
drift == (spec.generation == status.observedGeneration)
         AND ( a child was Created
               OR a field the operator manages actually changed )
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

Test coverage — every package `make test` reports, unabridged (Ginkgo + envtest
running a real kube-apiserver and etcd, not a fake client):

```
internal/webhook/v1alpha1   98.5%
internal/controller         75.2%
internal/provider           62.6%
api/v1alpha1                 3.5%
cmd                          0.0%
test/utils                   0.0%
```

The low ones need explaining rather than hiding. `api/v1alpha1` is dominated by
650 lines of generated `zz_generated.deepcopy.go`; the hand-written part is
`conditions.go`, which is at 100% (`conditions_test.go`) and is the part that
matters — the reconciler skips its status write when those helpers report no
change, so a bug there turns the 5-minute resync into a write per Application per
pass. `cmd` is the manager entry point and `test/utils` is e2e helpers; both run
under `test/e2e`, neither has unit tests. `internal/provider` is the lowest
number that reflects real risk: its error and capability paths run through
`MockProvider` rather than against a live CNPG install.

128 Ginkgo specs — most against envtest — plus 43 Go test functions, and a
Kind-based e2e suite under `test/e2e`.

---

## Renamed from `goplatform` — breaking API group change

This project was called `goplatform` until 2026-07-27. The repository is now
`github.com/abd-ulbasit/steward` (GitHub redirects the old URL), the Go module
is `github.com/abd-ulbasit/steward`, and the CRD moved with it:

| | Before | After |
|---|---|---|
| API group | `platform.platform.goplatform.io` | `platform.steward.sh` |
| `apiVersion` | `platform.platform.goplatform.io/v1alpha1` | `platform.steward.sh/v1alpha1` |
| Finalizer | `platform.goplatform.io/finalizer` | `platform.steward.sh/finalizer` |
| Generated labels | `platform.goplatform.io/{team,owner,tier}` | `platform.steward.sh/{team,owner,tier}` |
| Metric prefix | `goplatform_` | `steward_` |
| Namespace / name prefix | `goplatform-system`, `goplatform-` | `steward-system`, `steward-` |
| Provider env var | `GOPLATFORM_PROVIDER` | `STEWARD_PROVIDER` |

There is no conversion webhook and no migration path: the old and new groups are
unrelated CRDs as far as the API server is concerned. Existing `Application`
resources are **not** carried over. This is a personal project with no external
users, so the break was taken rather than papered over. To move an existing
cluster, delete the old CRD and its CRs, install the new CRD, and re-apply the
manifests with the new `apiVersion`.

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
`./hack/validate-e2e.sh` scripts that whole lifecycle (create → CNPG Cluster
appears → phase `Ready` → delete → everything cleaned up) and reports pass/fail
per step.

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

## How this was built

About half the commits here carry a `Co-authored-by: Claude` trailer — run
`git log --grep='^Co-authored-by: Claude' -i --oneline | wc -l` against
`git log --oneline | wc -l` for the current ratio. I build with coding agents and
review, run, and integrate what comes back; the CRD schema, the provider
abstraction, and the generated-resource plumbing are largely that. The parts that
decided the shape of this operator were not generated: the drift discriminator at
the top of this README came from watching `CreateOrUpdate` report `Updated` for
two situations that mean opposite things and working out that `observedGeneration`
is the only thing that separates them, and the coverage table above still prints
the 3.5% and the 0.0% with an explanation rather than a chart that flatters them.
To judge the engineering rather than the tooling, read that section and
[docs/DESIGN-DECISIONS.md](docs/DESIGN-DECISIONS.md).

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
