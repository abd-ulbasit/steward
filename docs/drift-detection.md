# Drift Detection & Self-Healing (Milestone 9)

Drift is the gap between **desired state** (the `Application` spec) and **actual
state** (what's really in the cluster). Someone scales a Deployment by hand,
deletes a Service, or edits a CNPG Cluster. A real operator notices and heals.
This is the core value of the operator pattern — and the subtlety is in *not*
crying wolf.

## Two kinds of drift

| | What happened | How GoPlatform handles it |
|---|---|---|
| **Spec drift** | A managed field was changed externally (replicas 3→5, a port edited) | **Auto-corrected** — `CreateOrUpdate` overwrites it back to desired, and we flag `DriftDetected` |
| **State drift** | A resource was deleted or crashed | **Recovered** — the watch re-triggers reconcile, `CreateOrUpdate` recreates it |

## How a change reaches the reconciler: watch propagation

Every child resource is created with an **owner reference** to its Application,
and `SetupWithManager` registers `.Owns()` for each child type. That wiring is
what makes self-healing automatic:

```
  someone edits/deletes a child Deployment
            │
            ▼
  controller-runtime informer sees the change
            │  owner reference → Application
            ▼
  Application is enqueued for reconcile
            │
            ▼
  Reconcile() runs CreateOrUpdate on every child
            │
            ▼
  child restored to desired state
```

`.Owns(&appsv1.Deployment{})`, `.Owns(&corev1.Service{})`, … mean "when one of
these children changes, reconcile its owner." No polling required for the common
case. The periodic resync (`RequeueAfter: 5m`) is a backstop for changes that
don't emit a watch event the controller sees (e.g. an operator-CRD edit).

## The hard part: telling real drift from defaulting noise

`controllerutil.CreateOrUpdate` returns `Updated` whenever the stored object
differs from what the mutate function produces — **including** fields the API
server defaults that our `build*` functions don't set (Deployment `strategy`,
`revisionHistoryLimit`, container `terminationMessagePath`, …). If we treated
every `Updated` as drift, we'd emit `DriftCorrected` on *every* reconcile,
forever. That's a false positive, and it's the trap M9 is really about.

So the drift signal is **not** the raw `CreateOrUpdate` result. It is:

```
drift  ==  resource was recreated (it was missing)
       OR  a MEANINGFUL managed field actually changed
```

- **Deployment** — compares `replicas` and the primary container image (field-level: `replicas 5->3`).
- **Service** — compares the set of exposed port numbers.
- **ConfigMap / Secret / HPA / PDB** — flag only *recreation* (deletion recovery); their `Updated` is dominated by server defaulting, so we don't flag it. Correction still happens regardless — we just don't raise a false drift alarm.

### The spec-generation discriminator

Even a meaningful change is only *drift* if **we didn't ask for it**. A user
editing the Application spec also produces `Updated` children — but that's
expected, not drift. The discriminator:

```go
specUnchanged := app.Generation == app.Status.ObservedGeneration
```

`ObservedGeneration` is written only at the end of a successful reconcile, so it
trails `Generation` by exactly one spec edit. When they're equal, the desired
state is one we've already reconciled — so any meaningful child change came from
outside. Drift is reported only when `specUnchanged` holds.

## What you observe

- **Event** `DriftCorrected` (Normal): `Restored child resources to desired state (Deployment (replicas 99->1))`
- **Condition** `DriftDetected`: `True` on the pass that corrected drift (message lists what), flipping back to `False` on the next clean reconcile. It's intentionally transient — a tripwire, not a steady state.

## Infrastructure drift (operator CRDs) — detect, don't fight

Operator CRDs (CNPG `Cluster`, Spotahome `RedisFailover`, RabbitMQ
`RabbitmqCluster`) are owned by *their* operators. We implement the optional
`DriftDetector` capability on `KubernetesProvider` to **report** when they've
been edited away from what the spec implies:

- database: `spec.instances` (HA → 3 else 1), `spec.storage.size`
- cache: `spec.redis.replicas`
- queue: `spec.replicas`

This is **read-only**. We surface it as an `InfrastructureDriftDetected` warning
event and fold it into the `DriftDetected` condition. We don't aggressively
overwrite on detection because `Provision()` already re-applies desired state on
its own cadence, and the operators reconcile their own resources — fighting them
mid-flight causes churn. Detection gives visibility within that window.

## How real platforms compare

- **Crossplane** continuously reconciles managed resources back to spec — same level-triggered idea, per-resource controllers.
- **Argo CD** surfaces drift as `OutOfSync` and can auto-sync or wait for human approval (detect-vs-correct, configurable) — analogous to our auto-correct (K8s children) vs detect-only (infra) split.
- **Flux** uses server-side apply with field ownership to avoid exactly the defaulting-noise problem described above.

## Trying it

```bash
kubectl scale deployment <app> --replicas=9     # introduce drift
# within a reconcile, replicas return to desired and:
kubectl describe application <app>              # see DriftDetected + DriftCorrected event

kubectl delete service <app>                    # delete a child
kubectl get service <app> -w                    # watch it get recreated
```
