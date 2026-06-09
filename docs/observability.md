# Observability (Milestone 8)

GoPlatform's observability has two distinct halves, and conflating them is the
most common source of confusion:

| Half | Question it answers | Where it lives |
|------|---------------------|----------------|
| **Controller metrics** | "Is *our operator* healthy?" | Custom Prometheus metrics on the controller's `/metrics` endpoint |
| **Application monitoring** | "Is *the user's app* healthy?" | `ServiceMonitor` + `PrometheusRule` CRs we generate per Application |

The controller emits the first; it *generates resources that configure* the
second. Keep that boundary in mind throughout.

---

## 1. The Prometheus Operator Ecosystem

We don't run Prometheus or write scrape configs. We hand the Prometheus
Operator declarative CRs and let it reconcile the actual Prometheus
configuration. This is the same pattern CNPG, ArgoCD, and Strimzi use.

```
  ┌────────────────┐   creates    ┌─────────────────────┐  watches  ┌────────────┐
  │ GoPlatform     │─────────────►│ ServiceMonitor CR   │──────────►│ Prometheus │
  │ controller     │              │ PrometheusRule CR   │           │ Operator   │
  └────────────────┘              └─────────────────────┘           └─────┬──────┘
                                                                          │ writes config
                                                                          ▼
                                                                   ┌────────────┐
                                                                   │ Prometheus │
                                                                   │  (scrape + │
                                                                   │   alert)   │
                                                                   └────────────┘
```

- **ServiceMonitor** — selects a `Service` by label and tells Prometheus which
  port/path to scrape. Prometheus discovers the pods behind that Service and
  scrapes `/metrics` on each.
- **PrometheusRule** — a set of PromQL alerting expressions. Prometheus
  evaluates them on an interval; when an expression is true for its `for:`
  duration, the alert fires to Alertmanager → Slack/PagerDuty/email.

### Graceful degradation when Prometheus isn't installed

The Prometheus Operator is optional. Before creating any monitoring CR, the
controller checks whether the `monitoring.coreos.com/v1` API group exists
(`isMonitoringCRDAvailable`, via a discovery client). If it doesn't, we **skip**
creation and set a `MonitoringReady=False` condition with reason
`PrometheusOperatorNotInstalled` — no crash, no error spam. This is how a good
operator behaves on a cluster that hasn't adopted the monitoring stack yet.

---

## 2. Controller Metrics

Registered with controller-runtime's registry (`init()` in
`internal/controller/metrics.go`) and served alongside the built-in
controller-runtime metrics on the manager's metrics endpoint.

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `goplatform_controller_reconcile_duration_seconds` | Histogram | `name, namespace, result` | How long each reconcile takes; watch p99 |
| `goplatform_controller_reconcile_errors_total` | Counter | `name, namespace, error_type` | Reconcile failures by category |
| `goplatform_application_phase_info` | Gauge | `name, namespace, phase, tier, team` | `1` for the Application's current phase |
| `goplatform_controller_managed_resources` | Gauge | `namespace, resource_type` | Count of controller-owned child resources |
| `goplatform_application_total` | Gauge | `namespace, tier` | Count of Applications per tier |

### Per-app vs. aggregate gauges — a subtle but important distinction

`phase_info` is **per-Application**: a single reconcile knows its own phase and
sets it directly. But `managed_resources` and `application_total` are
**namespace aggregates** — keyed by namespace, not by app name. A single
Application's reconcile cannot "set" them without clobbering its peers in the
same namespace.

So those two are **recomputed from a `List` on every reconcile**
(`updateManagedResourceMetrics`, `updateApplicationTotalMetrics`), not
incremented. This trades a List call for correctness, and it's self-healing:

```
  incremental (Inc/Dec)            recompute-from-List (what we do)
  ─────────────────────            ────────────────────────────────
  drifts on missed events          always matches actual cluster state
  needs delete bookkeeping         self-corrects when objects vanish
```

Every known tier is set explicitly each pass — **including zero**. A Prometheus
gauge retains its last value, so if the final `critical` app is deleted and we
only set tiers we currently see, the gauge would stay stuck at its old count.
This mirrors how `kube-state-metrics` derives gauges from cache state rather
than trusting event deltas.

---

## 3. Generated Application Monitoring

When an Application sets `spec.observability.metrics.enabled: true` (the default
when the block is present), the controller generates:

### ServiceMonitor (`<app-name>`)
Targets the Application's Service on the metrics port (`metrics` / `/metrics` by
default, overridable via `spec.observability.metrics.{port,path}`). Scrape
interval is tier-driven (critical scrapes fastest).

### PrometheusRule (`<app-name>-alerts`)
Two rule groups:

- **`<app>.sla`** — tier-based SLA alerts (error rate, P99 latency).
- **`<app>.health`** — universal health alerts: crash-looping, high restart
  count, OOM-killed, deployment stuck.

#### Tier thresholds

| Threshold | `critical` | `standard` | `development` |
|-----------|-----------|-----------|--------------|
| Error rate | > 0.1% for 5m | > 0.5% for 10m | > 5% for 15m |
| P99 latency | > 100ms for 5m | > 500ms for 10m | > 2s for 15m |
| Scrape interval | 15s | 30s | 60s |
| Alert severity | `critical` | `warning` | `info` |

### Ownership & cleanup
Every generated CR gets an owner reference to its Application, so Kubernetes
garbage-collects them when the Application is deleted. If `spec.observability` is
removed (or metrics disabled), the controller explicitly deletes the
ServiceMonitor/PrometheusRule and drops the `MonitoringReady` condition.

---

## 4. RBAC

The controller needs to manage monitoring CRs. The kubebuilder markers in
`application_controller.go` grant it:

```
monitoring.coreos.com: servicemonitors, prometheusrules → get;list;watch;create;update;patch;delete
```

Run `make manifests` after changing these to regenerate `config/rbac/role.yaml`.

---

## 5. Trying it locally

```bash
# Install the Prometheus Operator CRDs + stack (one option)
helm install kube-prom prometheus-community/kube-prometheus-stack -n monitoring --create-namespace

# Apply an Application with observability enabled, then inspect the generated CRs
kubectl get servicemonitor,prometheusrule -l app.kubernetes.io/managed-by=goplatform -A

# Controller's own metrics
kubectl port-forward -n goplatform-system deploy/goplatform-controller-manager 8443:8443
curl -sk https://localhost:8443/metrics | grep goplatform_
```

Without the Prometheus Operator installed, the controller runs fine and reports
`MonitoringReady=False / PrometheusOperatorNotInstalled` — by design.
