# GoPlatform Development Progress

## Status: Phase 2 - Real-World Operator

**Target Milestones**: 12
**Completed**: 9
**Current**: Phase 2 complete — Milestone 10 next (Phase 3, deferred)

---

## Phase Overview

| Phase | Description | Milestones | Status |
|-------|-------------|------------|--------|
| Phase 1 | Solid Foundation | M1-M5 | ✅ Complete |
| Phase 2 | Real-World Operator | M6-M9 | ✅ Complete |
| Phase 3 | Advanced Patterns | M10-M12 | 📋 Planned |

---

## Learning Progression

Each milestone builds on the previous, taking you from "basic operator" to "production-grade operator expertise":

```
M1-M5: "I can write a basic operator"
  ↓
M6: "I can make it work with real infrastructure"
  ↓
M7: "I can intercept and validate API requests"
  ↓
M8: "I can integrate with the monitoring ecosystem"
  ↓
M9: "I understand deep reconciliation patterns"
  ↓
M10: "I can evolve APIs without breaking users"
  ↓
M11: "I can integrate with the policy ecosystem"
  ↓
M12: "I can ship a production-grade operator"
```

---

## Phase 1: Solid Foundation

### Milestone 1: Project Setup & CRD Design - ✅ COMPLETED

**Goal:** Set up the operator project structure and design the core Application CRD.

**What You Learned:**
- How kubebuilder scaffolds operators
- CRD schema design with OpenAPI validation
- Why structural schemas matter for Kubernetes

**Concepts:**
```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         KUBEBUILDER PROJECT STRUCTURE                       │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  api/                       # CRD type definitions (Go structs)             │
│  └── v1alpha1/              # Version directory                             │
│      ├── application_types.go    # Application struct + markers             │
│      ├── groupversion_info.go    # API group registration                   │
│      └── zz_generated.deepcopy.go  # Auto-generated (make generate)         │
│                                                                             │
│  internal/controller/       # Reconciliation logic                          │
│  └── application_controller.go   # Reconciler implementation                │
│                                                                             │
│  config/                    # Generated Kubernetes manifests                │
│  ├── crd/                   # CRD YAML (make manifests)                     │
│  ├── rbac/                  # RBAC for controller                           │
│  └── webhook/               # Webhook configuration                         │
│                                                                             │
│  WHY THIS STRUCTURE:                                                        │
│  - Follows controller-runtime conventions                                   │
│  - Code generation expects specific paths                                   │
│  - Same pattern as Kubernetes itself                                        │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Deliverables:**
- [x] kubebuilder project initialization with domain `platform.goplatform.io`
- [x] Application CRD v1alpha1 type definitions
- [x] OpenAPI structural schema with validations
- [x] Generated CRD YAML and RBAC manifests
- [x] Helm chart for CRD installation

**CRD Design (Cloud-Agnostic):**
```yaml
apiVersion: platform.goplatform.io/v1alpha1
kind: Application
metadata:
  name: payments-api
spec:
  team: payments
  owner: alice@company.com
  tier: critical  # critical/standard/development

  workload:
    image: ghcr.io/company/payments-api:v1.0.0
    replicas: 3

  # Cloud-agnostic infrastructure requests
  database:
    type: postgres
    size: small      # small/medium/large → provider maps to specific sizes
    highAvailability: true

  cache:
    type: redis
    size: small

  queue:
    type: rabbitmq
    deadLetterQueue:
      enabled: true

  storage:
    type: pvc
    size: 10Gi

status:
  phase: Ready  # Pending/Provisioning/Ready/Failed/Deleting
  conditions:
    - type: Ready
      status: "True"
    - type: DatabaseReady
      status: "True"
    - type: CacheReady
      status: "True"
```

---

### Milestone 2: Basic Controller Reconciliation - ✅ COMPLETED

**Goal:** Implement the core reconciliation loop that watches Applications and creates Kubernetes resources.

**What You Learned:**
- Controller-runtime architecture (informers, work queues)
- Reconciliation pattern (level-triggered vs edge-triggered)
- Idempotent operations
- Error handling and requeueing

**Concepts:**
```
┌─────────────────────────────────────────────────────────────────────────────┐
│                      KUBERNETES CONTROLLER ARCHITECTURE                     │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                             INFORMER                                  │  │
│  │  ┌───────────────┐    ┌───────────────┐    ┌───────────────┐          │  │
│  │  │   Reflector   │───►│   DeltaFIFO   │───►│   Indexer     │          │  │
│  │  │ - List+Watch  │    │ - Add/Update  │    │ - Local cache │          │  │
│  │  │ - From API    │    │   /Delete     │    │   of objects  │          │  │
│  │  └───────────────┘    └───────┬───────┘    └───────────────┘          │  │
│  │                                │  events             ▲ read cache     │  │
│  │                                ▼                     │                │  │
│  │                       ┌───────────────┐              │                │  │
│  │                       │   Handler     │──────────────┘                │  │
│  │                       └───────┬───────┘                               │  │
│  └────────────────────────────────┼──────────────────────────────────────┘  │
│                                   ▼                                         │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                           WORK QUEUE                                  │  │
│  │  - Rate limiting, exponential backoff, deduplication, fair queuing    │  │
│  └─────────────────────────────────┬─────────────────────────────────────┘  │
│                                    ▼                                        │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                           RECONCILER                                  │  │
│  │  Reconcile(ctx, Request{Name, Namespace})                             │  │
│  │    ├─► Get current state (from cache)                                 │  │
│  │    ├─► Compare to desired state (spec)                                │  │
│  │    ├─► Take action (create/update/delete)                             │  │
│  │    ├─► Update status                                                  │  │
│  │    └─► Return Result (done / requeue / requeue after / error)         │  │
│  └───────────────────────────────────────────────────────────────────────┘  │
│                                                                             │
│  WHY LEVEL-TRIGGERED (not edge-triggered):                                  │
│  - Edge: "Something changed" → might miss events                           │
│  - Level: "Make actual = desired" → idempotent, self-healing               │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Deliverables:**
- [x] ApplicationReconciler struct with controller-runtime setup
- [x] SetupWithManager() with watches and predicates
- [x] Basic reconcile loop structure
- [x] Create Deployment from Application spec
- [x] Apply labels (app, team, managed-by)
- [x] Handle create/update/delete events (with finalizers)
- [x] Proper structured logging (controller-runtime log)
- [x] Error handling with requeue strategies
- [x] Unit tests with envtest (66.9% coverage)

---

### Milestone 3: Kubernetes Resource Generation - ✅ COMPLETED

**Goal:** Generate all necessary Kubernetes resources from Application spec.

**What You Learned:**
- Building K8s resources programmatically with Go client types
- Owner references for garbage collection
- ConfigMap and Secret generation patterns
- HPA and PDB for production readiness

**Deliverables:**
- [x] Deployment generation with pod spec from Application
- [x] Service generation (ClusterIP, port mapping)
- [x] ConfigMap generation for application configuration
- [x] Secret placeholder (for credential references)
- [x] HPA generation from scaling spec
- [x] PodDisruptionBudget for availability guarantees
- [x] Owner references on all created resources
- [x] Resource update logic (handle spec changes)
- [x] Unit tests for resource generation

---

### Milestone 4: Status Conditions & Finalizers - ✅ COMPLETED

**Goal:** Implement status reporting with conditions following K8s conventions, and safe deletion with finalizers.

*(Previously milestones 4, 5, 6 in the old plan. Consolidated because all were implemented together.)*

**What You Learned:**
- Status subresource pattern (spec vs status)
- Kubernetes conditions convention (positive polarity, observed generation)
- Finalizer pattern for blocking deletion until cleanup is complete
- `retry.RetryOnConflict` for status updates
- Event recording for operational visibility

**Deliverables:**
- [x] Status subresource with `+kubebuilder:subresource:status`
- [x] Condition types: Ready, WorkloadReady, DatabaseReady, CacheReady, QueueReady
- [x] Condition helper functions (SetCondition, GetCondition, IsReady) in `conditions.go`
- [x] Phase field: Pending/Provisioning/Ready/Failed/Deleting
- [x] Finalizer `platform.goplatform.io/finalizer` on create
- [x] Deletion detection via `DeletionTimestamp != nil`
- [x] Infrastructure cleanup before finalizer removal
- [x] Deletion grace period (30 minutes)
- [x] Event recording for key state transitions
- [x] Tests for condition transitions and cleanup

---

### Milestone 5: Provider Interface & Kubernetes Provider - ✅ COMPLETED

**Goal:** Design the pluggable provider abstraction and implement Kubernetes-native provisioning.

*(Previously milestones 6, 7 in the old plan. Consolidated because they form one logical unit.)*

**What You Learned:**
- Go interface design for abstraction
- Factory pattern for provider selection
- Strategy pattern for different infrastructure backends
- Working with third-party operator CRDs (CNPG, Redis, RabbitMQ)
- CRD discovery and validation
- Typed error systems in Go

**Deliverables:**
- [x] `InfrastructureProvider` interface (`Provision`, `GetStatus`, `Destroy`, `Healthy`)
- [x] Optional capability interfaces: `CostEstimator`, `DriftDetector`, `StateManager`
- [x] `ProviderFactory` with registration and instantiation
- [x] Typed error system: `NotReadyError`, `NotFoundError`, `QuotaExceededError`, `InvalidConfigError`, `ProvisioningError`, `TimeoutError`, `AuthenticationError`
- [x] `MockProvider` with lifecycle simulation, delay support, error injection, invocation tracking
- [x] `KubernetesProvider` implementation:
  - CloudNativePG Cluster CRD for PostgreSQL
  - Spotahome RedisFailover CRD for Redis
  - RabbitMQ Cluster Operator CRD for queues
  - PVC-based storage provisioning
- [x] Operator CRD discovery checks with clear error messages
- [x] Credential Secrets with connection strings per resource type
- [x] Cleanup logic for spec removal and full `Destroy()`
- [x] Unit tests: 32 provider tests + Kubernetes provider tests (fake client + discovery)

---

## Phase 2: Real-World Operator

### Milestone 6: End-to-End Integration - ✅ COMPLETED

**Goal:** Validate the controller↔provider wiring with RBAC, integration tests, and real Kind cluster setup for CNPG.

**What You Learned:**
- How `SetControllerReference` requires API-server-assigned UIDs (envtest UID bug)
- Shallow copy of `metav1.Condition` slices: `meta.SetStatusCondition` mutates in-place, causing `DeepEqual` to return true when comparing original vs modified status (the DeepCopy bug)
- RBAC marker generation for third-party CRDs (`controller-gen` reads comment markers to produce `ClusterRole` YAML)
- Ginkgo test filtering uses `--ginkgo.focus`, not Go's `-run` flag
- MockProvider state management: `ProvisionDelay` prevents `Provision()` from overwriting `SetState()` values
- Kind cluster setup with CNPG operator for real integration testing

**Bugs Discovered & Fixed:**
1. **Status DeepCopy bug**: `originalStatus := app.Status` creates a shallow copy where the `Conditions` slice shares the same backing array. `meta.SetStatusCondition` modifies conditions in-place, making `DeepEqual` always true → status updates silently skipped after initial set. Fixed by using `*app.Status.DeepCopy()` at all 3 locations in the controller.
2. **Envtest UID bug**: `KubernetesProvider_Envtest_CRDProvisioning` passed an in-memory Application (no UID) to `Provision()`, which calls `SetControllerReference` requiring a non-empty UID. Fixed by creating the Application in the envtest API server first.
3. **Dead code removed**: `setInfrastructureCondition` function was never called anywhere in the codebase.
4. **Destroy cleanup bug**: `KubernetesProvider.Destroy()` unconditionally cleaned up all 4 resource types even when only some were in the spec. When an operator CRD wasn't installed (e.g., RabbitmqCluster), the delete failed, blocking the finalizer forever. Fixed by guarding each cleanup with a nil-check on the corresponding spec field.
5. **Dockerfile fixes**: Go version mismatch (1.22→1.25), hardcoded GOARCH=amd64 on ARM, wrong binary path, non-numeric USER breaking runAsNonRoot.
6. **Missing provider env var**: Deployment manifest didn't set `GOPLATFORM_PROVIDER=kubernetes`, so controller defaulted to MockProvider.

**Deliverables:**
- [x] Controller calls `provider.Provision()` during reconciliation (already wired in M5)
- [x] Controller calls `provider.GetStatus()` and maps to Application conditions (already wired in M5)
- [x] Controller calls `provider.Destroy()` during finalizer cleanup (already wired in M5)
- [x] RBAC markers for all third-party CRDs (CNPG, Redis, RabbitMQ, PVCs, Pods)
- [x] Kind cluster setup script (`hack/setup-dev-cluster.sh`) with CNPG operator
- [x] End-to-end validation script (`hack/validate-e2e.sh`) for lifecycle testing
- [x] 7 envtest integration tests for controller↔provider flow (happy path, partial readiness, InvalidConfig, retryable error, destroy on delete, no-infra, status mapping)
- [x] Documentation: dev cluster setup guide (`docs/dev-cluster-setup.md`)
- [x] Live Kind cluster validation: 13/13 checks passed (create → provision → ready → delete → cleanup)

**Test Coverage:** Controller 75.8%, Provider 61.6% — all tests passing

---

### Credential Injection - ✅ COMPLETED (between M6 and M7)

**Goal:** Auto-inject infrastructure credentials as environment variables into workload containers.

**What You Learned:**
- The 12-factor app pattern for credential injection (DATABASE_URL, PGHOST, etc.)
- How `secretKeyRef` keeps credentials out of pod specs (only references, never values)
- User-defined env vars take precedence over auto-injected ones (appendIfNotDefined pattern)
- How Heroku, Railway, and Render handle credential injection vs Crossplane's manual approach

**Deliverables:**
- [x] `InjectCredentials *bool` field on WorkloadSpec (default: true, opt-out)
- [x] Type-specific env var injection: PostgreSQL (PGHOST, DATABASE_URL), MySQL, Redis, RabbitMQ
- [x] User-defined env var precedence (skip injection if user already defined the var)
- [x] `EnvFrom` field for bulk-mounting external Secrets/ConfigMaps

---

### Milestone 7: Admission Webhooks - ✅ COMPLETED

**Goal:** Add validating and mutating admission webhooks to intercept Application CRDs before they hit etcd.

**Why This Milestone Matters:**
Webhooks are how Kubernetes enforces rules at the API level — before objects are stored. Every production operator uses them. Validating webhooks reject invalid resources (preventing bad state from ever existing), and mutating webhooks inject defaults or modify resources transparently. This is fundamental to understanding how K8s extensibility works.

**What You Learned:**
- How the K8s API server calls webhooks during admission (the admission chain: authn → authz → schema → mutate → re-validate → validate → persist)
- The kubebuilder v4 `CustomValidator` + `CustomDefaulter` pattern (separate structs from API types)
- Why conditional defaults need webhooks (kubebuilder markers can only set static values)
- `field.ErrorList` for returning ALL validation errors at once (not first-error-wins)
- `apierrors.NewInvalid()` for structured Kubernetes API error formatting
- Immutable field protection pattern (used by CNPG, AWS ACK, Crossplane)
- cert-manager integration for webhook TLS (self-signed Issuer → Certificate → Secret)
- Envtest webhook support: `WebhookInstallOptions`, manager-based webhook server, TLS dial wait loop
- The `ENABLE_WEBHOOKS` env var pattern for disabling webhooks in development (kubebuilder convention)
- Kustomize replacements for cert-manager CA injection into webhook configurations

**Concepts:**
```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         ADMISSION WEBHOOK FLOW                             │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  kubectl apply ──► API Server ──► Schema Validation (CRD markers)          │
│                                          │                                  │
│                                          ▼                                  │
│                                   Mutating Webhooks                        │
│                                   (inject defaults)                        │
│                                          │                                  │
│                                          ▼                                  │
│                                   Re-validate Schema                       │
│                                   (catch bad mutations)                    │
│                                          │                                  │
│                                          ▼                                  │
│                                   Validating Webhooks                      │
│                                   (cross-field rules)                      │
│                                          │                                  │
│                                          ▼                                  │
│                                     Persist to etcd                        │
│                                                                             │
│  WHAT WE VALIDATE (webhooks):              WHAT MARKERS VALIDATE:          │
│  - critical tier → HA required             - field enums (tier values)     │
│  - postgres version 13-17                  - field patterns (team regex)   │
│  - mysql version 5 or 8                    - field ranges (maxReplicas≥1)  │
│  - minReplicas ≤ maxReplicas               - required fields              │
│  - immutable: database.type on update      - default values               │
│  - immutable: queue.type on update                                         │
│  - immutable: cache.type on update                                         │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Deliverables:**
- [x] Validating webhook with cross-field validation rules (critical HA, version ranges, scaling)
- [x] Mutating webhook with smart defaults (labels, conditional database version, backup for critical)
- [x] Immutable field protection on updates (database.type, queue.type, cache.type)
- [x] cert-manager configuration for webhook TLS (Issuer, Certificate, CA injection)
- [x] 35 unit tests for webhook validation and defaulting logic (98.5% coverage)
- [x] Envtest webhook integration suite (manager-based webhook server with TLS)
- [x] Kustomize config: webhook + cert-manager sections enabled
- [x] `ENABLE_WEBHOOKS` env var guard in `cmd/main.go`
- [x] Rich inline documentation: admission flow diagrams, comparison tables, pattern explanations

**Test Coverage:** Controller 77.1%, Provider 61.8%, **Webhook 98.5%** — all tests passing

---

### Milestone 8: Observability Integration - ✅ COMPLETED

**Goal:** Auto-generate Prometheus ServiceMonitors, PrometheusRules, and expose custom controller metrics for every Application.

**Why This Milestone Matters:**
Observability is not a nice-to-have — it's how you know your operator is working. Production operators expose metrics about their own performance (reconcile duration, error rate, queue depth) and generate monitoring resources for the applications they manage. This milestone teaches you the Prometheus operator ecosystem — the standard for Kubernetes monitoring.

**What You'll Learn:**
- How Prometheus discovers targets in Kubernetes (ServiceMonitor CRDs)
- How alerting rules work (PrometheusRule CRDs)
- Exposing custom metrics from a Go controller (prometheus client_golang)
- The Prometheus operator ecosystem (what it does, how it works)
- How production operators like CNPG and ArgoCD expose their own metrics

**How to Build It:**

1. **Controller Metrics** (internal observability — how is OUR operator performing?)
   - Use controller-runtime's built-in metrics registry
   - Add custom metrics:
     ```go
     // How long reconciliation takes
     reconcileDuration = prometheus.NewHistogramVec(...)
     // How many apps are in each phase
     applicationPhaseGauge = prometheus.NewGaugeVec(...)
     // How many errors have occurred
     reconcileErrors = prometheus.NewCounterVec(...)
     // How many infrastructure resources are managed
     managedResources = prometheus.NewGaugeVec(...)
     ```
   - These are served on the controller's metrics endpoint (`:8443/metrics` by default)

2. **ServiceMonitor Generation** (for the APPLICATION's metrics, not the controller's)
   - When `spec.observability.metrics.enabled: true`:
     - Create a `monitoring.coreos.com/v1` ServiceMonitor CR
     - Target the Application's Service on the metrics port
     - Add labels for team, app, tier (so Prometheus can filter/group)
     - Set owner reference to the Application for cleanup
   - Detect if Prometheus operator is installed (check for ServiceMonitor CRD)
   - If not installed, skip ServiceMonitor creation and set a condition

3. **PrometheusRule Generation** (SLA-based alerting per Application)
   - Generate alerts based on `spec.tier`:
     ```
     critical tier → alert if error rate > 0.1% or P99 > 100ms
     standard tier → alert if error rate > 0.5% or P99 > 500ms
     development  → alert if error rate > 5% or P99 > 2s
     ```
   - Standard alerts for every app:
     - Pod crash looping
     - High restart count
     - Container OOM killed
     - Deployment stuck (not progressing)
   - Create `monitoring.coreos.com/v1` PrometheusRule CR with owner reference

4. **RBAC for Monitoring Resources**
   - Add kubebuilder RBAC markers for `monitoring.coreos.com` group
   - Run `make manifests` to update role.yaml

**Deliverables:**
- [x] Custom Prometheus metrics for controller performance (reconcile duration, error count, app phase gauge, managed resources, application total by tier)
- [x] Aggregate gauges (managed resources, application total) recomputed from List each reconcile and wired into the reconcile + deletion paths
- [x] ServiceMonitor generation from Application observability spec
- [x] PrometheusRule generation with tier-based alerting thresholds
- [x] Prometheus operator CRD detection (skip if not installed, set condition)
- [x] Owner references on all monitoring resources
- [x] Cleanup when `spec.observability` is removed
- [x] RBAC markers for monitoring.coreos.com resources
- [x] Unit tests for ServiceMonitor and PrometheusRule generation
- [x] Envtest wiring test proving aggregate gauges reflect real cluster state
- [x] Documentation: how the Prometheus operator ecosystem works (`docs/observability.md`)

---

### Milestone 9: Drift Detection & Self-Healing - ✅ COMPLETED

**Goal:** Detect when child resources (Deployments, Services, operator CRDs) are modified or deleted externally, and automatically restore them to match the Application spec.

**Why This Milestone Matters:**
This is where you go from "basic operator" to "deeply understanding reconciliation." Real-world drift happens constantly — someone manually scales a deployment, edits a service port, or deletes a secret. A robust operator detects these changes and reconciles back to desired state. This is the core value proposition of the operator pattern, and implementing it properly teaches you watch mechanics, informer caching, and conflict resolution at a deep level.

**What You'll Learn:**
- How Kubernetes watches propagate changes (owner reference → parent reconcile)
- The difference between spec drift (someone changed a field) and state drift (something crashed)
- Conflict resolution strategies (always overwrite vs merge vs detect-and-alert)
- How `controllerutil.CreateOrUpdate` handles drift naturally
- Watching secondary resources (Deployments, Services, Secrets owned by Application)
- DriftDetector optional interface from the provider system

**How to Build It:**

1. **Watch Owned Resources**
   - Ensure `SetupWithManager` uses `.Owns()` for all child resource types:
     ```go
     ctrl.NewControllerManagedBy(mgr).
       For(&v1alpha1.Application{}).
       Owns(&appsv1.Deployment{}).
       Owns(&corev1.Service{}).
       Owns(&corev1.ConfigMap{}).
       Owns(&corev1.Secret{}).
       Owns(&policyv1.PodDisruptionBudget{}).
       Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
       Complete(r)
     ```
   - When an owned resource changes, the owner (Application) gets re-reconciled automatically

2. **K8s Resource Drift Detection & Repair**
   - During reconciliation, `CreateOrUpdate` already handles this:
     - If someone manually changed a Deployment's replicas, the mutate function overwrites it
     - If someone deleted a Service, `CreateOrUpdate` recreates it
   - Add event recording when drift is detected and corrected:
     ```
     Event: Normal  DriftCorrected  "Restored Deployment spec.replicas from 5 to 3 (manual change detected)"
     ```
   - Add a `DriftDetected` condition that is briefly True when drift is found, then transitions back to False after correction

3. **Infrastructure Drift Detection**
   - Implement the `DriftDetector` interface on `KubernetesProvider`:
     ```go
     type DriftDetector interface {
         DetectDrift(ctx context.Context, app *v1alpha1.Application) (*DriftReport, error)
     }
     ```
   - Compare the actual state of operator-managed CRDs against what the Application spec expects
   - Example: Application says `database.size: small` (1 instance, 1Gi RAM) but the CNPG Cluster has been manually edited to 3 instances
   - Report drift in status conditions with details

4. **Periodic Re-Sync**
   - The controller already requeues on success (`RequeueAfter: 5m`)
   - During periodic re-sync, run full drift detection
   - This catches external changes that don't trigger watches (e.g., someone edited a CRD directly)

5. **Deleted Resource Recovery**
   - If an owned resource is deleted, the watch triggers reconciliation
   - The reconcile loop should detect the missing resource and recreate it
   - Test: delete a Deployment owned by an Application → verify it's recreated within seconds

**Deliverables:**
- [x] `.Owns()` watches for all child resource types (Deployment, Service, ConfigMap, Secret, HPA, PDB)
- [x] Drift detection events (`DriftCorrected`) when a meaningful managed field is corrected
- [x] `DriftDetected` status condition (True when found, False after correction)
- [x] Meaningful-field drift signal (replicas/image/ports + recovery) instead of noisy CreateOrUpdate result — avoids false positives from server defaulting
- [x] `DriftDetector` interface implementation on KubernetesProvider (all three components)
- [x] Infrastructure drift reporting (CNPG instances/storage, Redis replicas, RabbitMQ replicas vs expected)
- [x] Deleted resource recovery (owned resource deleted → recreated)
- [x] Periodic re-sync validates all resources match desired state (5m requeue)
- [x] Envtest: manually modify Deployment → reconcile corrects it + flags drift; delete Service → recreated
- [x] Provider unit tests: DetectDrift returns items for drifted CRDs
- [x] Documentation: `docs/drift-detection.md` (watch propagation, spec vs state drift, defaulting-noise trap)

---

## Phase 3: Advanced Patterns

### Milestone 10: Multi-Version CRD & Conversion Webhooks - NOT STARTED

**Goal:** Add a `v1beta1` version of the Application CRD alongside `v1alpha1`, with a conversion webhook that translates between them.

**Why This Milestone Matters:**
API evolution is one of the most complex and least-documented aspects of building Kubernetes operators. Every production operator eventually needs to change its API — adding fields, renaming things, restructuring. The hub-and-spoke conversion webhook pattern is how Kubernetes itself handles this (e.g., `apps/v1beta1` Deployment → `apps/v1` Deployment). Most tutorials skip this entirely. Building it teaches you rare, valuable knowledge.

**What You'll Learn:**
- How Kubernetes stores and serves multiple API versions simultaneously
- The hub-and-spoke conversion pattern (one "storage" version, others convert to/from it)
- Conversion webhooks: when they're called, what they must guarantee (round-trip fidelity)
- How to evolve a CRD without breaking existing users
- `// +kubebuilder:storageversion` marker and what it means
- How Kubernetes controllers handle watching resources across versions

**How to Build It:**

1. **Design v1beta1 API Changes**
   - v1beta1 represents a more stable, refined API. Example changes:
     ```go
     // v1alpha1 (current):
     //   spec.database.size: "small"            (string enum)
     //   spec.database.highAvailability: true    (flat bool)
     //   spec.observability.metrics.enabled: true

     // v1beta1 (new, more structured):
     //   spec.infrastructure.database.sizing: { cpu: "500m", memory: "1Gi", storage: "10Gi" }
     //   spec.infrastructure.database.replicas: 3
     //   spec.infrastructure.database.backup.schedule: "0 3 * * *"
     //   spec.monitoring.prometheus.enabled: true
     //   spec.monitoring.prometheus.scrapeInterval: "30s"
     ```
   - The key: v1beta1 is more explicit, v1alpha1 uses abstractions (size: small)

2. **Scaffold v1beta1**
   ```bash
   kubebuilder create api --group platform --version v1beta1 --kind Application \
     --controller=false --resource=true
   ```
   - Define the new types in `api/v1beta1/application_types.go`
   - Mark v1alpha1 as the storage version initially (or v1beta1 if preferred)

3. **Implement Conversion Webhook**
   ```bash
   kubebuilder create webhook --group platform --version v1alpha1 --kind Application \
     --conversion --spoke v1beta1
   ```
   - Implement `ConvertTo(hub)` and `ConvertFrom(hub)` on the spoke version
   - Hub version stores the canonical representation
   - Conversion must be lossless: `v1alpha1 → hub → v1alpha1` must produce identical output
   - Handle fields that exist in one version but not the other (annotations for overflow data)

4. **Size Abstraction Mapping**
   - v1alpha1 `size: small` → v1beta1 `sizing: { cpu: "250m", memory: "512Mi", storage: "5Gi" }`
   - v1beta1 explicit sizing → v1alpha1 closest match ("small" / "medium" / "large") + annotation with exact values
   - This is the hard part: lossy conversion requires annotation-based storage

5. **Testing Conversion**
   - Round-trip test: create v1alpha1 → read as v1beta1 → write back → read as v1alpha1 → compare
   - Ensure no data loss through conversion cycle
   - Test with envtest (supports conversion webhooks)
   - Test kubectl with both versions: `kubectl get applications.v1alpha1.platform.goplatform.io`

**Deliverables:**
- [ ] `api/v1beta1/application_types.go` with evolved Application spec
- [ ] Storage version marker on hub version
- [ ] Conversion webhook implementation (ConvertTo/ConvertFrom)
- [ ] Size abstraction ↔ explicit sizing mapping with annotation overflow
- [ ] Round-trip fidelity tests (v1alpha1 → hub → v1alpha1 = identical)
- [ ] Envtest tests with both API versions simultaneously
- [ ] Controller works correctly regardless of which version is submitted
- [ ] Documentation: hub-and-spoke pattern, API evolution strategy, how K8s handles multi-version CRDs

---

### Milestone 11: Policy Integration with Kyverno - NOT STARTED

**Goal:** Integrate with Kyverno to enforce organizational policies on Application resources, and implement pre-provisioning quota checks.

**Why This Milestone Matters:**
Policy enforcement is how platform teams maintain guardrails without blocking developers. Instead of building a custom policy engine (which would be reinventing the wheel), you'll integrate with Kyverno — a CNCF project purpose-built for K8s policy. This teaches you the policy ecosystem, how admission policies work, and how to combine external policy engines with your operator's own validation.

**What You'll Learn:**
- How Kyverno policies work (ClusterPolicy, Policy CRDs)
- The difference between operator validation (webhooks) and external policy (Kyverno)
- When to use which: webhook validation for CRD-specific rules, Kyverno for organizational rules
- ResourceQuota and LimitRange — K8s built-in resource governance
- How to make your operator "policy-aware" (check policies before provisioning)

**How to Build It:**

1. **Bundled Kyverno Policies**
   - Ship a set of ClusterPolicy CRDs in `config/policies/`:
     ```yaml
     # Require team label on all Applications
     apiVersion: kyverno.io/v1
     kind: ClusterPolicy
     metadata:
       name: require-team-label
     spec:
       validationFailureAction: Enforce
       rules:
         - name: check-team
           match:
             resources:
               kinds: ["Application"]
           validate:
             message: "All Applications must have spec.team set"
             pattern:
               spec:
                 team: "?*"
     ```
   - Policies to include:
     - Require `spec.team` on all Applications
     - Require `spec.owner` on critical-tier Applications
     - Enforce `highAvailability: true` for critical tier databases
     - Restrict `spec.database.size` to "small" for development tier
     - Require backup enabled for production namespaces

2. **Pre-Provisioning Quota Check**
   - Before calling `provider.Provision()`, check if the namespace has ResourceQuota
   - Calculate estimated resource usage for the new Application
   - If it would exceed quota, set a `QuotaExceeded` condition and don't provision
   - This is operator-level checking (complementary to Kyverno's admission-level)

3. **Policy Status Reporting**
   - Add a `PolicyCompliant` condition to Application status
   - If Kyverno is installed, report whether the Application passes all policies
   - If Kyverno is not installed, skip policy checking (optional integration)

4. **Kyverno Detection**
   - Check if Kyverno CRDs exist in the cluster
   - If present, install bundled policies during operator startup
   - If absent, log a warning and skip policy features

**Deliverables:**
- [ ] Bundled Kyverno ClusterPolicy CRDs in `config/policies/`
- [ ] Pre-provisioning ResourceQuota check in reconciler
- [ ] `QuotaExceeded` condition when namespace quota would be exceeded
- [ ] `PolicyCompliant` condition in Application status
- [ ] Kyverno CRD detection (optional integration)
- [ ] Policy installation during operator startup (if Kyverno present)
- [ ] Tests for quota checking logic
- [ ] Documentation: how Kyverno works, webhook vs policy validation, when to use which

---

### Milestone 12: E2E Testing & CI Hardening - NOT STARTED

**Goal:** Build a comprehensive end-to-end test suite that validates the operator on a real Kind cluster, and harden the CI pipeline for production-grade releases.

**Why This Milestone Matters:**
This is the capstone milestone. Everything you've built needs to work together in a real cluster. E2E tests catch integration issues that unit tests and envtest miss — networking, timing, RBAC, operator interactions. A hardened CI pipeline means you can confidently release changes. This is what separates a learning project from a production-grade operator.

**What You'll Learn:**
- E2E testing patterns for Kubernetes operators
- Kind cluster management in CI
- GitHub Actions for Go + Kubernetes projects
- Container image building and testing
- Release workflows (goreleaser, semantic versioning)
- How production operators like cert-manager and ArgoCD do CI/CD

**How to Build It:**

1. **E2E Test Framework**
   - Use the existing `test/e2e/` directory
   - Create a Kind cluster with all required operators:
     ```bash
     kind create cluster --name goplatform-test-e2e
     # Install CNPG operator
     kubectl apply -f https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/main/releases/cnpg-1.25.0.yaml
     # Install Redis operator (Spotahome)
     helm install redis-operator ...
     # Install RabbitMQ operator
     kubectl apply -f https://github.com/rabbitmq/cluster-operator/releases/latest/download/cluster-operator.yml
     # Install Prometheus operator (for ServiceMonitor/PrometheusRule tests)
     helm install prometheus-operator ...
     # Install goplatform controller
     make docker-build IMG=goplatform:e2e
     kind load docker-image goplatform:e2e --name goplatform-test-e2e
     make deploy IMG=goplatform:e2e
     ```

2. **E2E Test Scenarios**
   - **Full lifecycle**: Create Application with all resource types → wait for Ready → verify all child resources → delete → verify cleanup
   - **Partial spec**: Application with only database → verify only database provisioned
   - **Spec update**: Change database size from small to medium → verify CNPG Cluster updated
   - **Drift recovery**: Manually delete a child Deployment → verify it's recreated
   - **Invalid Application**: Submit invalid spec → verify webhook rejects it
   - **Webhook defaults**: Submit minimal spec → verify defaults are injected
   - **Status accuracy**: Verify conditions reflect actual resource state at each stage
   - **Finalizer cleanup**: Delete Application during provisioning → verify graceful cleanup

3. **CI Pipeline (GitHub Actions)**
   ```yaml
   # .github/workflows/ci.yml
   jobs:
     lint:
       - golangci-lint
     unit-test:
       - make test (envtest)
     e2e-test:
       - Create Kind cluster
       - Install operators
       - Build and load controller image
       - Run e2e tests
       - Cleanup
     build:
       - Build Docker image
       - (on tag) Push to GHCR
   ```

4. **Release Workflow**
   - Semantic versioning (v0.1.0, v0.2.0, etc.)
   - GitHub Release with changelog
   - Container image pushed to GHCR on tag
   - Generated install manifest (`dist/install.yaml`)

**Deliverables:**
- [ ] Kind cluster setup script with all required operators
- [ ] E2E test suite covering full lifecycle, updates, drift, webhooks
- [ ] GitHub Actions CI: lint + unit test + e2e test
- [ ] Docker image build and push workflow
- [ ] Release workflow with semantic versioning
- [ ] `make test-e2e` target that runs the full suite
- [ ] CI runs on every PR and push to main
- [ ] Documentation: how to run e2e tests locally, CI architecture

---

## Architectural Decisions

| Decision | Options Considered | Choice | Reasoning |
|----------|-------------------|--------|-----------|
| Cloud abstraction | Direct AWS calls, XRD-style, Interface pattern | Interface pattern | Simpler than XRDs, more flexible than direct calls |
| Infrastructure | Terraform subprocess, Crossplane, ACK, K8s-native operators | K8s-native (CNPG, Redis, RabbitMQ) | Aligned with K8s learning goal, no subprocess complexity |
| Developer interface | kubectl, CLI (gpctl), REST API | kubectl only | Keep focus on operator patterns, kubectl is sufficient |
| Credential passing | K8s Secrets, ESO, CSI driver | K8s Secrets | Simple, works everywhere, ESO can be added later |
| Policy engine | Custom validation, OPA, Kyverno | Kyverno integration | Don't reinvent, learn the ecosystem instead |
| Local Kubernetes | minikube, kind, k3s, Colima | Kind (testing) + Colima (dev) | Kind for CI, Colima for daily dev |

---

## Concepts Learned

| Concept | Description | Where Applied |
|---------|-------------|---------------|
| Reconciliation Pattern | Level-triggered, idempotent state management | M2 |
| Owner References | Automatic garbage collection of child resources | M3 |
| Conditions | Multi-dimensional status reporting | M4 |
| Finalizer Pattern | Block deletion until cleanup complete | M4 |
| Provider Interface | Pluggable infrastructure abstraction | M5 |
| Factory Pattern | Configuration-driven provider instantiation | M5 |
| Typed Errors | Domain-specific error types for infrastructure failures | M5 |
| Operator CRD Discovery | Checking for third-party CRDs before creating resources | M5 |
| RBAC Marker Generation | Comment-based annotations → generated ClusterRole YAML | M6 |
| Status DeepCopy Pattern | Shallow copy of Conditions shares backing array; use DeepCopy | M6 |
| Kind Cluster Dev Setup | Local Kubernetes with real operators for integration testing | M6 |
| Controller↔Provider Integration Testing | MockProvider with envtest for verifying wiring | M6 |
| Credential Injection | Auto-inject env vars from provider Secrets; user precedence | M6.5 |
| Admission Webhooks | API-level validation before objects reach etcd | M7 |
| CustomValidator/CustomDefaulter | Kubebuilder v4 webhook pattern separating logic from types | M7 |
| field.ErrorList | Accumulate and return all validation errors at once | M7 |
| Immutable Field Protection | Block destructive changes (database.type) on update | M7 |
| cert-manager Integration | Auto-provision TLS certificates for webhook server | M7 |
| Conditional Defaulting | Webhook-based defaults that markers can't express | M7 |

---

## Known Issues

| Issue | Severity | Status | Notes |
|-------|----------|--------|-------|
| PROGRESS.md had milestone numbering mismatch | Low | ✅ Fixed | Renumbered in scope revision |
| M4/M5 marked "NOT STARTED" but code exists | Low | ✅ Fixed | Properly marked as completed |
| KubernetesProvider not wired into controller | Medium | ✅ Fixed | Wired in M5, validated in M6 |
| Status DeepCopy bug (shallow copy of Conditions) | High | ✅ Fixed | Discovered in M6, fixed with DeepCopy |
| Envtest UID bug (ownerReferences.uid empty) | Medium | ✅ Fixed | Application needs API server creation for UID |
| Destroy cleanup of non-requested resources | High | ✅ Fixed | Guard each cleanup with spec nil-check |
| Dockerfile Go version and binary path | Medium | ✅ Fixed | Bumped to 1.25, fixed /manager path, numeric USER |
| golangci-lint v2 config migration | Low | ✅ Fixed | Added `version: "2"`, moved gofmt/goimports to formatters, removed deprecated gosimple/exportloopref |
