# Design Decisions

How the `Application` control plane is built, and why each choice was made over
the alternatives. Written for someone who has to extend, operate, or review this
operator — it covers the parts that are not obvious from reading the code
top-to-bottom.

Companion documents: [drift-detection.md](drift-detection.md) (mechanics of
self-healing), [observability.md](observability.md) (metrics and generated
monitoring resources), [credential-injection.md](credential-injection.md)
(injected environment variables).

---

## 1. CRD schema

### One `Application` kind, not four

The obvious alternative is a kind per resource — `Database`, `Cache`, `Queue` —
composed by reference, which is what Crossplane does with Managed Resources.
That is the more flexible model and the wrong one here.

The premise of this operator is that a developer describes *their service*, not
its parts. A single kind means:

- One `kubectl get application` shows whether the whole stack is healthy. With
  four kinds, "is my service provisioned?" is a join the human has to do.
- Cross-component rules are expressible at admission time. `tier: critical`
  requires HA on both database *and* cache
  ([application_webhook.go](../internal/webhook/v1alpha1/application_webhook.go)).
  Across separate kinds that rule has nowhere to live — a `Database` CR does not
  know the tier of the service that will use it.
- Deletion is one object with one finalizer, not a dependency graph.

The cost is real and accepted: the CRD is large, and a database cannot be shared
between two Applications. Sharing would need a `databaseRef`-style escape hatch,
which is deliberately not in v1alpha1.

### Optional blocks are pointers, scalar toggles are `*bool`

`Database`, `Cache`, `Queue`, `Storage`, `Scaling`, `Observability` are all
pointer fields on `ApplicationSpec`. The distinction that matters is **absent**
vs **present-and-zero**:

```go
Database *DatabaseSpec  // nil = "I don't want a database"
                        // &DatabaseSpec{} = "I want one, use defaults"
```

A value type cannot express that. The provider relies on it directly: a `nil`
block means "clean up whatever exists for that component"
([kubernetes_provider.go](../internal/provider/kubernetes_provider.go), the
`else { _ = p.cleanupDatabase(...) }` branches in `Provision`).

**This makes removing a block from the spec a destructive edit.** Deleting
`database:` from a live Application destroys the CNPG cluster. That is
consistent with declarative semantics, and it is the sharpest edge in the API.
The webhook blocks *changing* `database.type` for the same reason it does not
block *removing* the block: changing the type is never intentional, removing it
plausibly is.

The same reasoning drives `*bool` for `enabled` and `injectCredentials`: a plain
`bool` defaults to `false`, so "field omitted" and "explicitly disabled" would be
indistinguishable, and there would be no way to default a feature to *on*.

### `size` is a t-shirt, not a `ResourceRequirements`

`small` / `medium` / `large` / `xlarge` map to concrete CPU, memory, and storage
in the provider. Exposing raw quantities would be more expressive and would
defeat the point: the platform team owns what "medium" costs, and can change the
mapping in one place without touching a single Application. It also makes the
CRD portable across providers — an AWS provider maps `medium` to an instance
class, the Kubernetes provider maps it to requests/limits.

`workload.resources` remains a raw `corev1.ResourceRequirements` because the
application container is the developer's own code, not managed infrastructure.

### `tier` is policy, not documentation

`tier` (`critical` / `standard` / `development`) is the single input that drives
cross-cutting policy:

| Consumer | Effect of `tier` |
|---|---|
| Validating webhook | `critical` rejects non-HA database and cache |
| ServiceMonitor | scrape interval (15s / 30s / 60s) |
| PrometheusRule | alert thresholds and `for` durations |
| Metrics | `applications_total` is labelled by tier |

Encoding SLA intent once, then deriving everything from it, is what keeps the
15-line YAML honest. The alternative — letting developers set scrape intervals
and alert thresholds — reproduces the snowflake problem the operator exists to
remove.

### Status carries both a phase and conditions

`status.phase` is a single enum (`Pending`, `Provisioning`, `Ready`, `Failed`,
`Deleting`) surfaced as a printer column. `status.conditions` is the
`metav1.Condition` list.

Kubernetes API conventions prefer conditions and discourage phase. Both exist
here because they answer different questions: `kubectl get applications` needs
one column a human can scan, and `kubectl describe` needs to say *which* of six
components is not ready. Phase is derived from the conditions, never set
independently — there is no state machine to get out of sync.

Every condition carries `observedGeneration`, and `status.observedGeneration` is
written at the end of a successful pass. Without it a user cannot distinguish
"Ready, and that reflects my latest edit" from "Ready, from before my edit".
Drift detection also depends on it (§3).

---

## 2. The reconcile loop

### Level-triggered, no event interpretation

`Reconcile` never asks "what changed". It reads the Application, computes the
full desired state, and applies all of it. Every child goes through
`controllerutil.CreateOrUpdate` with a mutate function that overwrites the fields
we manage and leaves everything else alone. Running the loop N times has the same
effect as running it once.

The consequence people miss: this is also the entire self-healing mechanism. A
hand-edited Deployment is not "detected and repaired" by special-case code — it
is simply overwritten by the next ordinary pass.

### Desired state is built pure, then applied

Every child has a `buildX(app) *X` function with no client access, and a
`reconcileX(ctx, app, dt)` that calls `CreateOrUpdate` around it. Splitting them
means the interesting logic — what a `critical`-tier PDB looks like, which env
vars a postgres+redis app gets — is testable as a pure function with no envtest,
no API server, and no `Eventually`. The envtest suite then only has to cover the
apply semantics.

### The finalizer is added and provisioning continues in the same pass

The finalizer must be added before any external resource is created, otherwise a
delete arriving in the gap orphans infrastructure. The usual scaffold returns
immediately after adding it and relies on a requeue.

That does not work here, and the reason is worth stating because it is the kind
of thing that produces a silent 5-minute stall:

- Adding a finalizer writes `metadata`, so `metadata.generation` does **not**
  change.
- The primary watch is filtered by `predicate.GenerationChangedPredicate{}`.
- Therefore the update event from the finalizer write is **dropped by the
  predicate** — no reconcile is triggered by it.

Returning early would have depended entirely on `Result.Requeue`, which
controller-runtime deprecated in v0.20. Instead the loop falls through: the
client decodes the API server response back into the in-memory object, so it
already has the finalizer and a fresh `resourceVersion`, and provisioning
proceeds. One fewer round-trip, and no reliance on a deprecated field.

### Status is written only when it changed

The status update runs inside `retry.RetryOnConflict`, re-fetches the object,
recomputes conditions, and compares with `reflect.DeepEqual` against a
`DeepCopy` of the status taken before the recompute. If nothing changed, no write
happens.

This matters more than it looks. The loop re-runs every 5 minutes forever. An
unconditional `Status().Update()` would be a write per Application per 5 minutes,
each one a watch event delivered to every client watching Applications. The
guard depends on condition helpers being stable — `SetCondition` must not report
a change when only `LastTransitionTime` moved — which is why
[conditions_test.go](../api/v1alpha1/conditions_test.go) pins that property
directly rather than through the controller.

`RetryOnConflict` is needed because concurrent reconciles of the same object can
race on the status subresource; retrying with a re-fetch is cheaper and less
surprising than failing the whole pass.

### Requeue policy

| Situation | Return | Why |
|---|---|---|
| Success | `RequeueAfter: 5m` | Backstop resync for changes no watch reported |
| Infrastructure not ready | `RequeueAfter: 10s` | Provisioning in progress, poll |
| Child resource error | `RequeueAfter: 10s`, `err == nil` | Status already records the failure; backoff adds nothing |
| Invalid config / no provider | `Result{}`, `err == nil` | Terminal until the user edits the spec — retrying is noise |
| Cleanup failure | `Result{}`, `err != nil` | Let the workqueue rate limiter apply exponential backoff |

The last row is a rule, not a preference: **controller-runtime ignores `Result`
whenever the returned error is non-nil.** Returning `{RequeueAfter: 10s}, err`
reads like "retry in 10 seconds" and does something else entirely. Every path
here returns either a `Result` or an error, never both.

---

## 3. Drift detection

Detail in [drift-detection.md](drift-detection.md); this is the design rationale.

### The discriminator is `generation` vs `observedGeneration`

`CreateOrUpdate` returns `Created` or `Updated` in two completely different
situations: first-time provisioning (expected) and correcting somebody's manual
edit (drift). The operation result alone cannot tell them apart. The
discriminator is:

```
drift == (a child was Created/Updated) AND (spec.generation == status.observedGeneration)
```

`observedGeneration` is only written at the end of a *successful* pass, so
reading it at the top of `Reconcile` gives the last generation fully reconciled.
If it equals the current generation, the desired state is one already applied —
so anything that still needed correcting was changed from outside.

### `Updated` alone is a false positive

For ConfigMap, Secret, HPA, and PDB, `CreateOrUpdate` reports `Updated` on
harmless server-side defaulting, not just on real edits. Flagging that as drift
would fire a `DriftCorrected` event every few minutes on an untouched cluster,
and an alert that cries wolf gets muted. So those kinds only count `Created` —
which means "it was deleted and we recreated it" — as drift. Deployment and
Service compare the specific fields they manage (replicas, image, port set)
before recording anything.

Under-reporting was chosen over over-reporting deliberately: a false negative
still gets corrected on the next pass, it just is not announced.

### The tracker is request-scoped

`driftTracker` is allocated per `Reconcile` call, not stored on the reconciler.
controller-runtime runs reconciles for different objects concurrently against one
reconciler instance, so accumulating drift on `r` would be a data race that
appears only under load. A fresh tracker per call is race-free by construction.

### Infrastructure drift is detect-only

Kubernetes children (Deployment, Service, …) are owned by this controller and
auto-corrected. Operator CRDs (CNPG `Cluster`, `RedisFailover`,
`RabbitmqCluster`) are owned by *their* operators, and `DetectDrift` is a
read-only comparison — it reports, it does not overwrite.

The reason is blast radius. `Provision` already re-applies the fields it manages
on its own cadence; an aggressive corrector layered on top could fight an
operator mid-failover, or revert an operator-initiated change (a CNPG failover
promoting a replica) that we do not understand. Reporting closes the visibility
gap between the external edit and the next `Provision` pass without taking that
risk. Storage-size drift is reported as `critical` rather than corrected for the
same reason — shrinking a live database volume is not a change an operator should
make on its own.

`DriftDetector` is an optional capability interface, discovered with a type
assertion, so a provider that cannot compare state simply does not implement it.

---

## 4. Ownership, finalizers, deletion

### Owner references do most of the cleanup

Every child — including the CNPG `Cluster`, `RedisFailover`, `RabbitmqCluster`,
and PVCs — gets `controllerutil.SetControllerReference`. That buys two things:
Kubernetes garbage-collects children when the Application is deleted, and
`.Owns()` maps a child event back to its owner so edits and deletions trigger a
reconcile without polling.

### So why keep a finalizer?

Because owner-reference GC is asynchronous, unordered, and namespace-local. It
cannot express "tear the queue down before the database", it does not exist for
resources outside the cluster, and it gives no place to report that cleanup
failed.

`Destroy` therefore deletes explicitly, in reverse dependency order
(queue → cache → database → storage), and only for components actually present in
the spec. That last guard is not cosmetic: attempting to delete a
`RabbitmqCluster` when the RabbitMQ operator was never installed fails, the
finalizer is never removed, and the Application is stuck in `Deleting` forever.

The finalizer is also what makes this operator portable to a provider whose
resources are *not* Kubernetes objects. Owner references would do nothing for an
RDS instance; the finalizer path is unchanged.

### Deletion timeout is observed, not enforced

`handleDeletion` stamps `platform.steward.sh/deletion-started` on the first
pass and emits a `DeletionStuck` warning event once cleanup exceeds 30 minutes.
It does **not** force-remove the finalizer.

Force-removal would clear the stuck object and silently orphan whatever it was
protecting — exactly the outcome the finalizer exists to prevent. The object
stays stuck, loudly, and a human decides. Everything an operator needs to make
that decision is in the events and conditions.

Like the finalizer add, the annotation write is metadata-only, so it does not
requeue — cleanup runs in the same pass.

---

## 5. Partial provisioning failure

An Application can ask for a database, a cache, and a queue. They fail
independently. Three decisions define what happens:

**`Provision` returns partial state alongside the error.** Its signature is
`(*ResourceState, error)`, and the state is populated as far as it got. The
controller writes per-component status from that state even on the error path, so
`DatabaseReady=True, CacheReady=False` is a state the user can actually see —
rather than the whole object reporting a single opaque failure.

**"Not ready" is soft, everything else is hard.** A `NotReadyError` from one
component is recorded and provisioning continues to the next, because "CNPG is
still electing a primary" should not stop Redis from being created. Any other
error returns immediately with the state accumulated so far. The consequence is
explicit: if the database fails hard, the cache and queue are not attempted on
that pass. They are attempted on the next one — the loop is level-triggered, so
progress is made incrementally rather than transactionally.

**Error type decides requeue behaviour.** The typed error taxonomy in
[errors.go](../internal/provider/errors.go) exists so the controller can react
correctly instead of retrying everything forever:

| Error | Meaning | Controller response |
|---|---|---|
| `NotReadyError` | Still provisioning | Requeue in 10s, phase `Provisioning` |
| `RetryableError` | Transient (API throttle, conflict) | Requeue in 10s |
| `InvalidConfigError` | User's spec is wrong | `Failed` condition, **no requeue** |
| `ProviderNotConfiguredError` | Operator CRD missing | `Failed` condition, **no requeue** |
| `QuotaExceededError` | Needs human action | Warning event, no automatic retry |
| anything else | Unknown | Return the error, workqueue backs off |

Requeueing an `InvalidConfigError` forever would burn API calls and fill the log
with a failure only a spec edit can fix — and a spec edit bumps `generation`,
which triggers a reconcile on its own.

### There is no rollback

A partially provisioned Application is not torn down. Convergence is forward-only:
the next pass retries what failed and leaves what succeeded. Rolling back would
mean destroying a healthy, possibly data-bearing database because an unrelated
queue failed to come up.

### Monitoring failures never block provisioning

`reconcileMonitoring` errors produce a `MonitoringFailed` warning event and the
reconcile continues. A missing or broken Prometheus Operator must not stop a
developer's database from being created. For the same reason, `.Owns()` for
`ServiceMonitor` and `PrometheusRule` is registered only after a discovery check
— calling `.Owns()` for a type whose CRD is absent panics the manager at startup.

---

## 6. Known limits

Stated plainly rather than left for a reviewer to find:

- **v1alpha1 only.** No conversion webhook, so there is no migration path yet for
  a breaking schema change.
- **Infrastructure drift is reported, never corrected.** §3 explains why; it is
  still a gap if you expected enforcement.
- **`mysql`, `memcached`, `sqs`, and `kafka` are schema-level only.** The
  Kubernetes provider implements postgres (CNPG), redis (Spotahome), and rabbitmq
  (RabbitMQ Cluster Operator). The CRD accepts the others and the provider
  rejects them with a typed `InvalidConfigError` naming the offending field —
  clear, but it is admission-time validation that should arguably live in the
  webhook instead.
- **`storage` provisions a PVC whatever `type` says.** The spec models object
  storage (`s3` / `gcs`); the Kubernetes provider creates a
  PersistentVolumeClaim. The spec shape is ahead of the implementation and a
  cloud provider is what would close the gap.
- **Credentials are plain Kubernetes Secrets.** No External Secrets Operator, no
  CSI driver, no rotation.
- **One database per Application, not shareable.** A consequence of §1.
- **`status.estimatedMonthlyCost` is defined but not populated** by the
  Kubernetes provider — the `CostEstimator` capability exists for a cloud
  provider that has real prices.
