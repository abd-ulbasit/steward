/*
Copyright 2026 GoPlatform Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// =============================================================================
// APPLICATION CONTROLLER - CORE RECONCILIATION LOGIC
// =============================================================================
//
// This is the brain of GoPlatform's operator. It watches Application CRDs
// and reconciles the cluster state to match the desired specification.
//
// CONTROLLER-RUNTIME ARCHITECTURE:
// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
//
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │                          KUBERNETES API SERVER                          │
//   │                                                                         │
//   │   Applications     Deployments      Services       Secrets              │
//   │   (our CRD)        (we create)      (we create)    (we create)          │
//   └───────┬─────────────────┬──────────────┬──────────────┬─────────────────┘
//           │ watch            │ watch         │ watch         │ watch
//           ▼                  ▼               ▼               ▼
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │                           SHARED INFORMER CACHE                         │
//   │                                                                         │
//   │  WHY CACHE?                                                             │
//   │  - Reduces API server load (read from local cache, not etcd)            │
//   │  - Enables list/watch pattern (initial list, then deltas)               │
//   │  - Cache is eventually consistent (few ms lag acceptable)               │
//   │                                                                         │
//   │  HOW IT WORKS:                                                          │
//   │  1. Reflector calls List() to get all objects                           │
//   │  2. Then Watch() for changes (efficient long-poll)                      │
//   │  3. Changes update the local Indexer cache                              │
//   │  4. Our reconciler reads from cache, writes to API server               │
//   └───────────────────────────────────────────────────────────────────────┬─┘
//                                                                           │
//           ┌────────────────┬────────────────┬────────────────┐            │
//           ▼                ▼                ▼                ▼            │
//   ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐      │
//   │ Application │  │ Deployment  │  │  Service    │  │  Secret     │      │
//   │  Handler    │  │  Handler    │  │  Handler    │  │  Handler    │      │
//   │             │  │(owner ref)  │  │(owner ref)  │  │(owner ref)  │      │
//   └──────┬──────┘  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘      │
//          │                │                │                │             │
//          └────────────────┴────────────────┴────────────────┘             │
//                                      │                                    │
//                                      ▼                                    │
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │                             WORK QUEUE                                  │
//   │                                                                         │
//   │  ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐                           │
//   │  │app/a │ │app/b │ │app/a │ │app/c │ │app/a │  → deduplicated           │
//   │  └──────┘ └──────┘ └──────┘ └──────┘ └──────┘                           │
//   │                                                                         │
//   │  FEATURES:                                                              │
//   │  • Deduplication: Multiple events for same object = one reconcile       │
//   │  • Rate limiting: Prevents thundering herd on mass changes              │
//   │  • Exponential backoff: Failed reconciles wait longer before retry      │
//   │  • Fair queuing: No single object can starve others                     │
//   └───────────────────────────────────────────────────────────────────────┬─┘
//                                                                           │
//                                      ▼                                    │
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │                      RECONCILE LOOP (this file)                         │
//   │                                                                         │
//   │  func Reconcile(ctx, Request{Name, Namespace}) (Result, error)          │
//   │    │                                                                    │
//   │    ├─► 1. GET: Fetch current Application from cache                     │
//   │    │      └─ If not found, object was deleted → cleanup & return        │
//   │    │                                                                    │
//   │    ├─► 2. FINALIZE: Handle deletion if deletionTimestamp set            │
//   │    │      └─ Remove external resources, then remove finalizer           │
//   │    │                                                                    │
//   │    ├─► 3. RECONCILE: Make actual state match desired state              │
//   │    │      ├─ Create/Update Deployment                                   │
//   │    │      ├─ Create/Update Service                                      │
//   │    │      └─ (Later: Create infrastructure via providers)               │
//   │    │                                                                    │
//   │    ├─► 4. STATUS: Update Application.Status with current state          │
//   │    │      └─ Set conditions, phase, observed generation                 │
//   │    │                                                                    │
//   │    └─► 5. RETURN: Signal completion or requeue                          │
//   │          ├─ Result{} → Done, no requeue                                 │
//   │          ├─ Result{RequeueAfter: 5m} → Check again in 5 minutes         │
//   │          └─ error → Requeue with exponential backoff                    │
//   │                                                                         │
//   └─────────────────────────────────────────────────────────────────────────┘
//
// LEVEL-TRIGGERED VS EDGE-TRIGGERED:
// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
//
//   EDGE-TRIGGERED (what we DON'T do):
//     "Application X was updated" → React to the event
//     ❌ Problem: What if we miss an event? What if processing fails?
//     ❌ Need complex event ordering and exactly-once semantics
//
//   LEVEL-TRIGGERED (what we DO):
//     "Make cluster match Application X's spec" → Compare and sync
//     ✅ Idempotent: Running twice gives same result
//     ✅ Self-healing: If state drifts, next reconcile fixes it
//     ✅ Miss an event? No problem, next reconcile catches up
//
//   ANALOGY:
//     Edge: "The thermostat was turned up" (event)
//     Level: "The room should be 72°F" (desired state)
//     The furnace doesn't care HOW you set 72°F, it just maintains it.
//
// HOW CROSSPLANE/BACKSTAGE COMPARE:
// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
//
//   Crossplane:
//   - Same controller-runtime pattern
//   - One controller per Managed Resource type
//   - External controller watches Crossplane, provisions cloud
//
//   Backstage:
//   - Not a K8s operator (Node.js app)
//   - Uses polling to refresh catalog
//   - No reconciliation loop, just reads/displays
//
//   ArgoCD:
//   - Controller-runtime based
//   - Reconciles Application → Git → Cluster
//   - We're similar but for infrastructure
//
// =============================================================================

package controller

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
	"github.com/abd-ulbasit/goplatform/internal/provider"
)

// =============================================================================
// CONSTANTS
// =============================================================================

const (
	// applicationFinalizer is attached to Applications to ensure cleanup
	// before deletion. See handleDeletion() for detailed explanation.
	applicationFinalizer = "platform.goplatform.io/finalizer"

	// requeueAfterError is the delay before retrying after a transient error.
	// We use a fixed delay here; the work queue adds exponential backoff.
	requeueAfterError = 10 * time.Second

	// requeueAfterSuccess is the delay for periodic re-sync.
	// Even if nothing changed, we recheck to catch drift.
	requeueAfterSuccess = 5 * time.Minute

	// deletionGracePeriod is the maximum time we wait for external resources
	// to be cleaned up before giving up. After this, manual intervention needed.
	// This prevents objects from being stuck in "Deleting" state forever.
	deletionGracePeriod = 30 * time.Minute

	// deletionStartAnnotation tracks when deletion cleanup started.
	// Used to detect stuck deletions that exceed the grace period.
	deletionStartAnnotation = "platform.goplatform.io/deletion-started"
)

// =============================================================================
// RECONCILER STRUCT
// =============================================================================
//
// The reconciler holds dependencies needed for reconciliation.
//
// WHY client.Client (not direct API client):
//   - Reads from cache (fast, reduces API server load)
//   - Writes go directly to API server
//   - Handles serialization, pagination, retries
//   - Type-safe with generated methods
//
// WHY runtime.Scheme:
//   - Maps Go types ↔ GVK (GroupVersionKind)
//   - Required for owner references (garbage collection)
//   - Used by client for encoding/decoding
//
// =============================================================================

// ApplicationReconciler reconciles Application objects.
type ApplicationReconciler struct {
	// Client reads from cache, writes to API server.
	// Injected by controller-runtime manager.
	client.Client

	// Scheme maps Go types to Kubernetes GVKs.
	// Needed for setting owner references.
	Scheme *runtime.Scheme

	// Recorder emits Kubernetes Events for important operations.
	// =========================================================================
	// WHY EVENTS:
	//   - Events provide operational visibility into controller actions
	//   - kubectl describe shows events (last ~1 hour)
	//   - Monitoring systems can alert on event patterns
	//   - Users understand what the controller is doing
	//
	// WHEN TO EMIT EVENTS:
	//   - Resource creation: "Created Deployment my-app"
	//   - Errors: "Failed to create RDS instance: quota exceeded"
	//   - State transitions: "Scaling from 2 to 4 replicas"
	//   - External actions: "Terraform apply started"
	//
	// EVENT TYPES:
	//   Normal: Routine operations (create, update, scale)
	//   Warning: Errors, degraded state, temporary failures
	//
	// HOW CROSSPLANE/ARGOCD DO IT:
	//   - Crossplane: Events for every resource lifecycle event
	//   - ArgoCD: Events for sync operations, health changes
	//   - Prometheus Operator: Events for config reloads, errors
	// =========================================================================
	Recorder record.EventRecorder

	// CleanupExternalResources is an optional hook for deleting external
	// infrastructure (Terraform, cloud resources, etc.) during finalization.
	//
	// WHY THIS EXISTS:
	//   - M5 requires explicit cleanup on delete, before finalizer removal
	//   - We don't have real providers yet, so this hook keeps the controller
	//     production-ready while enabling tests to simulate failure modes
	//
	// HOW IT'S USED:
	//   - If nil: no-op, proceed to finalizer removal
	//   - If set and returns error: keep finalizer, requeue, emit Warning event
	//
	// HOW REAL PLATFORMS DO IT:
	//   - Crossplane: provider-specific external delete in managed resource
	//   - AWS ACK: calls AWS Delete APIs and waits for terminal state
	//
	CleanupExternalResources func(ctx context.Context, app *platformv1alpha1.Application) error

	// ProviderFactory creates InfrastructureProvider instances for provisioning.
	//
	// WHY A FACTORY:
	//   - Decouples controller from specific provider implementations
	//   - Allows config-driven selection (AWS/GCP/K8s/Mock)
	//   - Supports dependency injection in tests
	//
	// If nil, the controller will lazily create a default factory.
	ProviderFactory *provider.Factory

	// DiscoveryClient checks which API resources are available in the cluster.
	// Used to detect if Prometheus operator CRDs (ServiceMonitor, PrometheusRule)
	// are installed before attempting to create monitoring resources.
	// If nil, monitoring resource creation is skipped.
	DiscoveryClient DiscoveryInterface
}

// =============================================================================
// RBAC MARKERS
// =============================================================================
//
// These markers generate ClusterRole rules in config/rbac/role.yaml.
// The controller needs permission to manage these resources.
//
// FORMAT: +kubebuilder:rbac:groups=X,resources=Y,verbs=Z
//
// PRINCIPLE OF LEAST PRIVILEGE:
//   - Only request permissions we actually need
//   - Separate permissions for status subresource
//   - Be explicit about verbs (don't use *)
//
// =============================================================================

// RBAC for our CRD
// +kubebuilder:rbac:groups=platform.platform.goplatform.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.platform.goplatform.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.platform.goplatform.io,resources=applications/finalizers,verbs=update

// RBAC for Kubernetes resources we create
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// RBAC for third-party operator CRDs managed by KubernetesProvider
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=databases.spotahome.com,resources=redisfailovers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rabbitmq.com,resources=rabbitmqclusters,verbs=get;list;watch;create;update;patch;delete

// RBAC for Prometheus operator monitoring resources (ServiceMonitor, PrometheusRule)
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch;create;update;patch;delete

// RBAC for PVCs and Pods managed/observed by KubernetesProvider
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list

// =============================================================================
// RECONCILE - THE MAIN LOOP
// =============================================================================
//
// This is called every time something relevant changes:
//   - Application is created/updated/deleted
//   - Owned Deployment/Service changes (via owner reference)
//   - Periodic resync (default every ~10 hours)
//   - After requeue from previous reconcile
//
// CONTRACT:
//   - MUST be idempotent (running N times = same result as running once)
//   - MUST be level-triggered (compare and sync, don't react to events)
//   - SHOULD update status every reconcile
//   - SHOULD return quickly (offload slow work to background)
//
// =============================================================================

// Reconcile moves the cluster state toward the desired state specified
// in the Application spec.
//
// The reconciliation flow:
//  1. Fetch the Application from cache
//  2. Handle deletion (finalizer pattern)
//  3. Ensure finalizer is present
//  4. Reconcile child resources (Deployment, Service)
//  5. Update Application status
//  6. Return result (requeue or done)
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// =========================================================================
	// STEP 0: SETUP LOGGING + METRICS TIMING
	// =========================================================================
	//
	// Structured logging with request context. Every log line includes:
	//   - namespace/name of the Application
	//   - reconcileID for tracing this specific reconcile
	//
	// We also start a timer for the reconcile duration metric.
	//
	// =========================================================================

	reconcileStart := time.Now()
	logger := log.FromContext(ctx)
	logger.Info("starting reconciliation")

	// =========================================================================
	// STEP 1: FETCH THE APPLICATION
	// =========================================================================
	//
	// Get the Application that triggered this reconcile.
	//
	// IMPORTANT: We read from CACHE, not directly from etcd.
	//   - Faster (no network call if cached)
	//   - Reduces API server load
	//   - Eventually consistent (few ms lag is OK)
	//
	// If not found, the Application was deleted:
	//   - If it had a finalizer, deletion is blocked → we should clean up
	//   - If no finalizer, it's already gone → nothing to do
	//
	// =========================================================================

	var app platformv1alpha1.Application
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		if apierrors.IsNotFound(err) {
			// Application was deleted. If we had external resources to clean up,
			// they should have been handled by the finalizer before deletion
			// actually happened. Safe to ignore.
			logger.Info("application not found, likely deleted")
			recordReconcileDuration(req.Name, req.Namespace, "not_found", time.Since(reconcileStart).Seconds())
			return ctrl.Result{}, nil
		}
		// Real error (network, RBAC, etc.). Requeue with backoff.
		logger.Error(err, "failed to fetch application")
		incrementReconcileErrors(req.Name, req.Namespace, "fetch_failed")
		recordReconcileDuration(req.Name, req.Namespace, "error", time.Since(reconcileStart).Seconds())
		return ctrl.Result{}, err
	}

	// =========================================================================
	// STEP 2: HANDLE DELETION (FINALIZER PATTERN)
	// =========================================================================
	//
	// If deletionTimestamp is set, Kubernetes wants to delete this object.
	// But if we have a finalizer, deletion is blocked until we remove it.
	//
	// This gives us a chance to:
	//   1. Clean up external resources (Terraform destroy, delete cloud resources)
	//   2. Remove our finalizer
	//   3. Kubernetes then actually deletes the object
	//
	// WHY FINALIZERS:
	//
	//   Without finalizer:
	//   ┌──────────────┐     ┌──────────────┐
	//   │ User deletes │────►│ Object gone  │  ← AWS resources orphaned! 💸
	//   │ kubectl del  │     │ immediately  │
	//   └──────────────┘     └──────────────┘
	//
	//   With finalizer:
	//   ┌──────────────┐     ┌──────────────┐     ┌──────────────┐
	//   │ User deletes │────►│ deletionTS   │────►│ Reconciler   │
	//   │ kubectl del  │     │ set, blocked │     │ sees delete  │
	//   └──────────────┘     └──────────────┘     └──────┬───────┘
	//                                                    │
	//   ┌──────────────┐     ┌──────────────┐     ┌──────▼───────┐
	//   │ Object gone  │◄────│ Remove       │◄────│ Clean up     │
	//   │ from etcd    │     │ finalizer    │     │ AWS/TF       │
	//   └──────────────┘     └──────────────┘     └──────────────┘
	//
	// =========================================================================

	if !app.DeletionTimestamp.IsZero() {
		result, err := r.handleDeletion(ctx, &app)
		if err != nil {
			incrementReconcileErrors(req.Name, req.Namespace, "deletion_failed")
		} else {
			deleteApplicationMetrics(req.Name, req.Namespace, string(app.Spec.Tier), app.Spec.Team)
			// Recompute namespace aggregates so the removed Application and its
			// child resources drop out of the managed-resource and tier totals.
			r.updateManagedResourceMetrics(ctx, req.Namespace)
			r.updateApplicationTotalMetrics(ctx, req.Namespace)
		}
		recordReconcileDuration(req.Name, req.Namespace, "deletion", time.Since(reconcileStart).Seconds())
		return result, err
	}

	// =========================================================================
	// STEP 3: ENSURE FINALIZER EXISTS
	// =========================================================================
	//
	// If object doesn't have our finalizer, add it.
	// This must happen before we create any external resources.
	//
	// ORDER MATTERS:
	//   1. Add finalizer FIRST
	//   2. Create external resources SECOND
	//   Otherwise deletion can slip through between 1 and 2.
	//
	// =========================================================================

	if !controllerutil.ContainsFinalizer(&app, applicationFinalizer) {
		logger.Info("adding finalizer")
		controllerutil.AddFinalizer(&app, applicationFinalizer)
		if err := r.Update(ctx, &app); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		// Requeue to continue with reconciliation
		return ctrl.Result{Requeue: true}, nil
	}

	// =========================================================================
	// STEP 4: RECONCILE CHILD RESOURCES
	// =========================================================================
	//
	// Create or update the Kubernetes resources that implement this Application.
	// Currently: Deployment + Service
	// Later: Also trigger Terraform for infrastructure
	//
	// IDEMPOTENCY:
	//   Each function checks if resource exists, creates if not, updates if changed.
	//   Running twice produces the same result.
	//
	// =========================================================================

	// Track if this is initial provisioning (no Ready condition yet)
	isInitialProvisioning := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeReady) == nil

	// Set phase to Provisioning during initial work
	if isInitialProvisioning {
		if err := r.updatePhase(ctx, &app, platformv1alpha1.ApplicationProvisioning); err != nil {
			logger.Error(err, "failed to update phase to Provisioning")
			return ctrl.Result{}, err
		}
	}

	// =========================================================================
	// M9: DRIFT DETECTION SETUP
	// =========================================================================
	//
	// specUnchanged is the discriminator between "expected change" and "drift".
	// ObservedGeneration is only written at the END of a successful reconcile,
	// so reading it here reflects the LAST generation we fully reconciled. If it
	// equals the current Generation, the desired state is unchanged — any child
	// that still needs Create/Update was altered externally (drift). The tracker
	// accumulates each child's apply outcome for evaluation after all children.
	// =========================================================================
	specUnchanged := app.Generation == app.Status.ObservedGeneration
	dt := &driftTracker{}

	// Reconcile Deployment (if workload specified)
	var deploymentReady bool
	if app.Spec.Workload != nil {
		var err error
		deploymentReady, err = r.reconcileDeployment(ctx, &app, dt)
		if err != nil {
			logger.Error(err, "failed to reconcile deployment")
			r.setFailedCondition(ctx, &app, "DeploymentFailed", err.Error())
			incrementReconcileErrors(req.Name, req.Namespace, "deployment")
			recordReconcileDuration(req.Name, req.Namespace, "error", time.Since(reconcileStart).Seconds())
			return ctrl.Result{RequeueAfter: requeueAfterError}, nil
		}
	} else {
		// No workload specified, consider workload "ready" (nothing to do)
		deploymentReady = true
	}

	// Reconcile Service (if workload has ports)
	var serviceReady bool
	if app.Spec.Workload != nil && len(app.Spec.Workload.Ports) > 0 {
		var err error
		serviceReady, err = r.reconcileService(ctx, &app, dt)
		if err != nil {
			logger.Error(err, "failed to reconcile service")
			r.setFailedCondition(ctx, &app, "ServiceFailed", err.Error())
			incrementReconcileErrors(req.Name, req.Namespace, "service")
			recordReconcileDuration(req.Name, req.Namespace, "error", time.Since(reconcileStart).Seconds())
			return ctrl.Result{RequeueAfter: requeueAfterError}, nil
		}
	} else {
		// No ports, no service needed
		serviceReady = true
	}

	// Reconcile ConfigMap
	if _, err := r.reconcileConfigMap(ctx, &app, dt); err != nil {
		logger.Error(err, "failed to reconcile configmap")
		r.setFailedCondition(ctx, &app, "ConfigMapFailed", err.Error())
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	// Reconcile Secret
	if _, err := r.reconcileSecret(ctx, &app, dt); err != nil {
		logger.Error(err, "failed to reconcile secret")
		r.setFailedCondition(ctx, &app, "SecretFailed", err.Error())
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	// Reconcile HPA
	if _, err := r.reconcileHPA(ctx, &app, dt); err != nil {
		logger.Error(err, "failed to reconcile HPA")
		r.setFailedCondition(ctx, &app, "HPAFailed", err.Error())
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	// Reconcile PDB
	if _, err := r.reconcilePDB(ctx, &app, dt); err != nil {
		logger.Error(err, "failed to reconcile PDB")
		r.setFailedCondition(ctx, &app, "PDBFailed", err.Error())
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	// Reconcile monitoring resources (ServiceMonitor + PrometheusRule)
	if err := r.reconcileMonitoring(ctx, &app); err != nil {
		logger.Error(err, "failed to reconcile monitoring resources")
		// Monitoring failures are non-fatal — don't block the main reconciliation
		r.Recorder.Event(&app, corev1.EventTypeWarning, "MonitoringFailed",
			fmt.Sprintf("Failed to reconcile monitoring resources: %v", err))
	}

	// =========================================================================
	// STEP 4.5: PROVISION INFRASTRUCTURE VIA PROVIDER
	// =========================================================================
	//
	// If the Application requests infrastructure (database/cache/queue/storage),
	// we delegate to the InfrastructureProvider implementation.
	//
	// This is the core of our Layer 4 abstraction:
	//   Application spec → Provider (K8s/AWS/GCP/Mock) → ResourceState
	//
	// HOW CROSSPLANE DOES IT:
	//   - Crossplane creates Managed Resources per component
	//   - Each Managed Resource has its own controller
	//   - We use a single provider to keep things simpler for now
	//
	// =========================================================================

	requeueAfter := requeueAfterSuccess
	var infraState *provider.ResourceState
	var infraDrift []provider.DriftItem
	infraRequested := app.Spec.Database != nil || app.Spec.Cache != nil || app.Spec.Queue != nil || app.Spec.Storage != nil
	if infraRequested {
		prov, err := r.getProvider()
		if err != nil {
			logger.Error(err, "failed to get infrastructure provider")
			r.setFailedCondition(ctx, &app, "ProviderUnavailable", err.Error())
			return ctrl.Result{}, nil
		}

		infraState, err = prov.Provision(ctx, &app)
		if err != nil {
			// NotReady is expected during provisioning; continue to update status.
			if provider.IsNotReady(err) {
				logger.Info("infrastructure still provisioning", "error", err.Error())
				requeueAfter = requeueAfterError
			} else if provider.IsInvalidConfig(err) || provider.IsProviderNotConfigured(err) {
				// Invalid config or missing provider should not be retried automatically.
				r.setFailedCondition(ctx, &app, "InfrastructureInvalid", err.Error())
				return ctrl.Result{}, nil
			} else if provider.IsRetryable(err) {
				// Retryable errors get requeued; treat as provisioning.
				logger.Info("retryable infrastructure error", "error", err.Error())
				requeueAfter = requeueAfterError
			} else {
				return ctrl.Result{}, err
			}
		}

		// M9: infrastructure drift detection (read-only). The operators own these
		// CRDs, so we report drift rather than overwrite aggressively; Provision()
		// re-applies desired state on its own cadence. Best-effort — detection
		// failure must never fail the reconcile.
		if dd, ok := prov.(provider.DriftDetector); ok {
			if items, derr := dd.DetectDrift(ctx, &app); derr != nil {
				logger.V(1).Info("infrastructure drift detection failed", "error", derr.Error())
			} else {
				infraDrift = items
			}
		}
	}

	// =========================================================================
	// M9: EVALUATE & REPORT DRIFT
	// =========================================================================
	//
	// K8s child drift was already CORRECTED above (CreateOrUpdate overwrote any
	// external edit). Here we only decide whether what happened counts as drift —
	// a child needed Create/Update while the spec was unchanged — and surface it.
	// Infra drift is detect-only. Events are emitted ONCE here, never inside the
	// status retry loop below (which may run several times on conflict).
	// =========================================================================
	var driftMessages []string
	if correctedChildren := dt.corrected(); specUnchanged && len(correctedChildren) > 0 {
		msg := driftCorrectionMessage(correctedChildren)
		driftMessages = append(driftMessages, "corrected child resources: "+msg)
		logger.Info("drift corrected", "resources", msg)
		r.Recorder.Event(&app, corev1.EventTypeNormal, "DriftCorrected",
			fmt.Sprintf("Restored child resources to desired state (%s)", msg))
	}
	if len(infraDrift) > 0 {
		msg := infraDriftMessage(infraDrift)
		driftMessages = append(driftMessages, "infrastructure drift: "+msg)
		logger.Info("infrastructure drift detected", "items", msg)
		r.Recorder.Event(&app, corev1.EventTypeWarning, "InfrastructureDriftDetected",
			fmt.Sprintf("Infrastructure drifted from desired state (%s)", msg))
	}
	driftDetectedNow := len(driftMessages) > 0
	driftMessage := strings.Join(driftMessages, "; ")

	// =========================================================================
	// STEP 5: UPDATE STATUS WITH RETRY
	// =========================================================================
	//
	// Report the current state of the Application.
	// This is crucial for users to understand what's happening.
	//
	// STATUS UPDATES ARE CHEAP:
	//   - Status is a separate subresource (/status endpoint)
	//   - Updating status doesn't trigger reconcile (no spec change)
	//   - Users can watch status for progress
	//
	// WHY RETRY:
	// =========================================================================
	//   Status updates can fail due to conflicts (another reconcile updated
	//   the object). Instead of failing the entire reconcile, we retry with
	//   exponential backoff.
	//
	//   CONFLICT SCENARIO:
	//   1. Reconcile A reads app (resourceVersion: 100)
	//   2. Reconcile B reads app (resourceVersion: 100)
	//   3. Reconcile A updates status (resourceVersion: 101)
	//   4. Reconcile B tries to update status → CONFLICT! (rv 100 < 101)
	//   5. With retry, B re-fetches (rv 101) and updates → Success
	//
	//   HOW CROSSPLANE/ARGOCD HANDLE IT:
	//     - Crossplane: Uses RetryOnConflict for all status updates
	//     - ArgoCD: Similar pattern with retry wrapper
	//     - Prometheus Operator: Implements retry loop
	// =========================================================================

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch the app to get latest version
		if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
			return err
		}

		originalStatus := *app.Status.DeepCopy()

		// Update infrastructure status from provider state
		r.applyInfrastructureStatus(&app, infraState)

		// Update conditions (includes infrastructure readiness)
		infraReady := r.updateInfrastructureConditions(ctx, &app, infraState)
		r.updateWorkloadConditions(ctx, &app, deploymentReady)
		r.updateOverallReadyCondition(ctx, &app, serviceReady, infraReady)

		// M9: reflect drift. True on the pass where drift was found/corrected,
		// False once observed state matches desired again.
		if driftDetectedNow {
			meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
				Type:               platformv1alpha1.ConditionTypeDriftDetected,
				Status:             metav1.ConditionTrue,
				Reason:             "DriftDetected",
				Message:            driftMessage,
				ObservedGeneration: app.Generation,
			})
		} else {
			meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
				Type:               platformv1alpha1.ConditionTypeDriftDetected,
				Status:             metav1.ConditionFalse,
				Reason:             "InSync",
				Message:            "All managed resources match desired state",
				ObservedGeneration: app.Generation,
			})
		}

		// Set overall phase
		if deploymentReady && serviceReady && infraReady {
			app.Status.Phase = platformv1alpha1.ApplicationReady
		} else {
			app.Status.Phase = platformv1alpha1.ApplicationProvisioning
		}

		// Set observed generation
		app.Status.ObservedGeneration = app.Generation

		// Persist status only when something changed
		if reflect.DeepEqual(originalStatus, app.Status) {
			return nil
		}

		// Persist status
		return r.Status().Update(ctx, &app)
	})

	if retryErr != nil {
		logger.Error(retryErr, "failed to update status after retries")
		incrementReconcileErrors(req.Name, req.Namespace, "status_update")
		recordReconcileDuration(req.Name, req.Namespace, "error", time.Since(reconcileStart).Seconds())
		return ctrl.Result{}, retryErr
	}

	// =========================================================================
	// STEP 6: RETURN RESULT
	// =========================================================================
	//
	// Signal to the work queue what to do next.
	//
	// OPTIONS:
	//   Result{} + nil          → Done, don't requeue
	//   Result{Requeue: true}   → Requeue immediately (rate-limited)
	//   Result{RequeueAfter: X} → Requeue after duration X
	//   Result{} + error        → Requeue with exponential backoff
	//
	// We always requeue after some time to catch drift (e.g., someone
	// manually deleted the Deployment). This is "reconciliation" - always
	// working toward desired state.
	//
	// =========================================================================

	logger.Info("reconciliation complete",
		"phase", app.Status.Phase,
		"deploymentReady", deploymentReady,
		"serviceReady", serviceReady,
	)

	// Record metrics
	reconcileDurationSec := time.Since(reconcileStart).Seconds()
	recordReconcileDuration(req.Name, req.Namespace, "success", reconcileDurationSec)
	setApplicationPhase(req.Name, req.Namespace, string(app.Status.Phase), string(app.Spec.Tier), app.Spec.Team)

	// Recompute namespace-aggregate gauges (managed-resource counts + Application
	// totals by tier). Unlike the per-app gauges above, these are keyed only by
	// namespace, so they cannot be "set" from a single Application's view without
	// clobbering peers. We recompute them from a List on every reconcile — this is
	// self-healing: counts naturally fall when resources are deleted out-of-band.
	r.updateManagedResourceMetrics(ctx, req.Namespace)
	r.updateApplicationTotalMetrics(ctx, req.Namespace)

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// =============================================================================
// AGGREGATE METRIC COLLECTORS
// =============================================================================
//
// These populate the namespace-level gauges defined in metrics.go
// (managedResourcesGauge, applicationTotal). They are deliberately List-based
// rather than incremental:
//
//   incremental (Inc/Dec)        recompute-from-List (what we do)
//   ─────────────────────        ────────────────────────────────
//   drifts on missed events      always reflects actual cluster state
//   needs delete bookkeeping     self-corrects when objects vanish
//   cheap                        one List per type per reconcile
//
// For a controller managing a modest number of Applications, the List cost is
// negligible and correctness wins. This mirrors how kube-state-metrics derives
// its gauges from List/Watch caches rather than trusting event deltas.

// updateManagedResourceMetrics counts the controller-owned Deployments and
// Services in a namespace and publishes them to managedResourcesGauge. Selection
// is by the shared managed-by label that every child resource carries.
//
// Each kind is set explicitly every pass (a len-0 List still sets 0), so an
// emptied namespace does not leave a stale, "stuck" gauge value behind.
func (r *ApplicationReconciler) updateManagedResourceMetrics(ctx context.Context, namespace string) {
	logger := log.FromContext(ctx)
	selector := client.MatchingLabels{"app.kubernetes.io/managed-by": "goplatform"}

	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(namespace), selector); err != nil {
		// Metrics are best-effort — never fail reconciliation over them.
		logger.V(1).Info("failed to list Deployments for managed-resource metric", "error", err.Error())
	} else {
		setManagedResources(namespace, "Deployment", float64(len(deployments.Items)))
	}

	var services corev1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(namespace), selector); err != nil {
		logger.V(1).Info("failed to list Services for managed-resource metric", "error", err.Error())
	} else {
		setManagedResources(namespace, "Service", float64(len(services.Items)))
	}
}

// updateApplicationTotalMetrics counts Applications by tier in a namespace and
// publishes them to applicationTotal. Every known tier is set explicitly so a
// tier dropping to zero resets its gauge instead of retaining a stale value.
func (r *ApplicationReconciler) updateApplicationTotalMetrics(ctx context.Context, namespace string) {
	logger := log.FromContext(ctx)

	var apps platformv1alpha1.ApplicationList
	if err := r.List(ctx, &apps, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("failed to list Applications for total metric", "error", err.Error())
		return
	}

	// Seed all known tiers at zero so emptied tiers are reset, not left stale.
	counts := map[platformv1alpha1.ServiceTier]int{
		platformv1alpha1.TierCritical:    0,
		platformv1alpha1.TierStandard:    0,
		platformv1alpha1.TierDevelopment: 0,
	}
	for i := range apps.Items {
		counts[apps.Items[i].Spec.Tier]++
	}
	for tier, count := range counts {
		setApplicationTotal(namespace, string(tier), float64(count))
	}
}

// =============================================================================
// HANDLE DELETION - CLEANUP BEFORE OBJECT REMOVAL
// =============================================================================
//
// Called when deletionTimestamp is set. The object is marked for deletion
// but blocked by our finalizer.
//
// FLOW:
//   1. Perform cleanup (Terraform destroy, delete cloud resources)
//   2. Remove our finalizer
//   3. Return - Kubernetes will delete the object
//
// FAILURE HANDLING:
//   If cleanup fails, we return an error → requeue → retry
//   Object stays in "Deleting" state until cleanup succeeds
//   User can see why via kubectl describe (status conditions)
//
// TIMEOUT HANDLING:
// =============================================================================
//   Objects stuck in deleting state (e.g., Terraform keeps failing) can
//   cause operational issues. We track when deletion started and emit
//   warning events if it exceeds the grace period.
//
//   This doesn't force-delete (that would orphan resources), but provides
//   visibility for operators to investigate.
// =============================================================================
//
// =============================================================================

func (r *ApplicationReconciler) handleDeletion(ctx context.Context, app *platformv1alpha1.Application) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(app, applicationFinalizer) {
		// No finalizer to remove, nothing to do
		return ctrl.Result{}, nil
	}

	logger.Info("handling deletion, cleaning up resources")

	// =========================================================================
	// TRACK DELETION START TIME
	// =========================================================================
	//
	// We add an annotation to track when cleanup started.
	// If cleanup takes longer than the grace period, we emit warnings.
	//
	// =========================================================================

	if app.Annotations == nil {
		app.Annotations = make(map[string]string)
	}

	if _, exists := app.Annotations[deletionStartAnnotation]; !exists {
		app.Annotations[deletionStartAnnotation] = time.Now().Format(time.RFC3339)
		if err := r.Update(ctx, app); err != nil {
			logger.Error(err, "failed to set deletion start annotation")
			return ctrl.Result{}, err
		}
		// Requeue to continue after annotation is set
		return ctrl.Result{Requeue: true}, nil
	}

	// Check if deletion is taking too long
	if startTimeStr, exists := app.Annotations[deletionStartAnnotation]; exists {
		startTime, err := time.Parse(time.RFC3339, startTimeStr)
		if err == nil && time.Since(startTime) > deletionGracePeriod {
			logger.Info("deletion exceeds grace period",
				"gracePeriod", deletionGracePeriod,
				"elapsed", time.Since(startTime))
			r.Recorder.Event(app, corev1.EventTypeWarning, "DeletionStuck",
				fmt.Sprintf("Deletion cleanup exceeds %v grace period. Manual intervention may be required.",
					deletionGracePeriod))
		}
	}

	// Update phase to Deleting
	app.Status.Phase = platformv1alpha1.ApplicationDeleting
	if err := r.Status().Update(ctx, app); err != nil {
		logger.Error(err, "failed to update phase to Deleting")
		// Don't return error - we still want to try cleanup
	}

	// =========================================================================
	// CLEANUP EXTERNAL RESOURCES
	// =========================================================================
	//
	// We call the provider's Destroy() to remove external resources.
	// This is the primary cleanup path for infrastructure created by providers.
	//
	// =========================================================================

	if r.ProviderFactory != nil {
		prov, err := r.getProvider()
		if err != nil {
			logger.Error(err, "failed to get provider during deletion")
			return ctrl.Result{RequeueAfter: requeueAfterError}, err
		}

		if err := prov.Destroy(ctx, app); err != nil {
			logger.Error(err, "provider cleanup failed")
			if r.Recorder != nil {
				r.Recorder.Event(app, corev1.EventTypeWarning, "CleanupFailed",
					fmt.Sprintf("Provider cleanup failed: %v", err))
			}
			if provider.IsNotReady(err) || provider.IsRetryable(err) {
				return ctrl.Result{RequeueAfter: requeueAfterError}, nil
			}
			return ctrl.Result{}, err
		}
	} else if r.CleanupExternalResources != nil {
		if err := r.CleanupExternalResources(ctx, app); err != nil {
			logger.Error(err, "external cleanup failed")
			if r.Recorder != nil {
				r.Recorder.Event(app, corev1.EventTypeWarning, "CleanupFailed",
					fmt.Sprintf("External cleanup failed: %v", err))
			}
			// Keep finalizer, requeue to retry cleanup
			return ctrl.Result{RequeueAfter: requeueAfterError}, err
		}
	}

	logger.Info("cleanup complete, removing finalizer")

	// Remove our finalizer
	controllerutil.RemoveFinalizer(app, applicationFinalizer)
	if err := r.Update(ctx, app); err != nil {
		logger.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}

	// Don't requeue - object will be deleted
	return ctrl.Result{}, nil
}

// =============================================================================
// RECONCILE DEPLOYMENT
// =============================================================================
//
// Creates or updates the Deployment for this Application's workload.
//
// DESIGN DECISIONS:
//
// 1. OWNER REFERENCE:
//    We set ownerReferences on the Deployment pointing to our Application.
//    This enables:
//    - Garbage collection: Delete Application → Deployment auto-deleted
//    - Watch propagation: Deployment changes → Application reconciled
//
// 2. LABELS:
//    We apply consistent labels for:
//    - Selection (app.kubernetes.io/name matches selector)
//    - Filtering (team label for cost allocation queries)
//    - Tooling (managed-by for GitOps tools to ignore)
//
// 3. CreateOrUpdate PATTERN:
//    =========================================================================
//    WHY CreateOrUpdate:
//      - Atomic: No race condition between check and create/update
//      - Idempotent: Safe to call multiple times with same result
//      - Clean: One function handles both create and update paths
//
//    HOW IT WORKS:
//      1. Fetches existing object (or creates empty template)
//      2. Calls our mutate function to set desired spec
//      3. Either Creates (if new) or Updates (if existing)
//      4. Returns operation result (Created, Updated, Unchanged)
//
//    ALTERNATIVES:
//    ┌─────────────────────────────────────────────────────────────────────┐
//    │ Approach          │ Pros                 │ Cons                     │
//    ├───────────────────┼──────────────────────┼──────────────────────────┤
//    │ Get/Create/Update │ Fine control         │ Race conditions possible │
//    │ (what we had)     │                      │ between Get and Create   │
//    ├───────────────────┼──────────────────────┼──────────────────────────┤
//    │ Server-Side Apply │ Best for conflicts   │ More complex, requires   │
//    │ (SSA)             │ Multi-owner support  │ field manager setup      │
//    ├───────────────────┼──────────────────────┼──────────────────────────┤
//    │ ✅ CreateOrUpdate │ Atomic, simple,      │ Full replacement, not    │
//    │                   │ idempotent           │ merge                    │
//    └─────────────────────────────────────────────────────────────────────┘
//
//    HOW CROSSPLANE/ARGOCD DO IT:
//      - Crossplane: Uses SSA for fine-grained field ownership
//      - ArgoCD: Uses client-side apply with special diff logic
//      - Prometheus Operator: Uses CreateOrUpdate like us
//    =========================================================================
//
// =============================================================================

func (r *ApplicationReconciler) reconcileDeployment(ctx context.Context, app *platformv1alpha1.Application, dt *driftTracker) (bool, error) {
	logger := log.FromContext(ctx)

	// =========================================================================
	// BUILD DESIRED STATE
	// =========================================================================
	desired := r.buildDeployment(app)

	// Set owner reference for garbage collection
	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return false, fmt.Errorf("failed to set owner reference: %w", err)
	}

	// =========================================================================
	// CREATE OR UPDATE ATOMICALLY
	// =========================================================================
	//
	// CreateOrUpdate implements the "upsert" pattern:
	//   1. Try to Get the existing object
	//   2. If not found, Create it
	//   3. If found, call the mutate function, then Update
	//
	// The mutate function (second arg) modifies the object in-place.
	// We copy desired spec into the existing object.
	//
	// IMPORTANT: The object passed to CreateOrUpdate must have ObjectMeta
	// (Name, Namespace) set. The mutate function receives the existing
	// object (or empty template) and should update it to desired state.
	//
	// =========================================================================

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	// Capture the live managed fields BEFORE the mutate overwrites them, so we can
	// report field-level drift (e.g. "replicas 5->3") when an external actor
	// scaled or re-imaged the Deployment out from under us. We compare these
	// specific fields rather than trusting CreateOrUpdate's result, which reports
	// "Updated" even on harmless server-side defaulting.
	var oldReplicas *int32
	var oldImage string

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		// This function mutates 'deployment' to match desired state
		// It's called after Get (if exists) or with empty object (if new)

		oldReplicas = deployment.Spec.Replicas
		if len(deployment.Spec.Template.Spec.Containers) > 0 {
			oldImage = deployment.Spec.Template.Spec.Containers[0].Image
		}

		// Copy spec from desired
		deployment.Spec = desired.Spec
		deployment.Labels = desired.Labels

		// Set owner reference (needed inside mutate for create case)
		return controllerutil.SetControllerReference(app, deployment, r.Scheme)
	})

	if err != nil {
		return false, fmt.Errorf("failed to create/update deployment: %w", err)
	}

	// Emit event based on operation result
	switch opResult {
	case controllerutil.OperationResultCreated:
		logger.Info("created deployment", "deployment", deployment.Name)
		r.Recorder.Event(app, corev1.EventTypeNormal, "DeploymentCreated",
			fmt.Sprintf("Created Deployment %s", deployment.Name))
	case controllerutil.OperationResultUpdated:
		logger.Info("updated deployment", "deployment", deployment.Name)
		r.Recorder.Event(app, corev1.EventTypeNormal, "DeploymentUpdated",
			fmt.Sprintf("Updated Deployment %s", deployment.Name))
	case controllerutil.OperationResultNone:
		// No changes needed
	}

	// Drift = the resource was recreated (was missing) OR a meaningful managed
	// field actually changed. Replicas is the field-level "showcase"; we also
	// catch image changes. Anything else (server defaulting) is NOT drift.
	drifted := false
	detail := ""
	switch opResult {
	case controllerutil.OperationResultCreated:
		drifted, detail = true, "recreated"
	case controllerutil.OperationResultUpdated:
		if oldReplicas != nil {
			desiredReplicas := int32(1)
			if desired.Spec.Replicas != nil {
				desiredReplicas = *desired.Spec.Replicas
			}
			if *oldReplicas != desiredReplicas {
				drifted = true
				detail = fmt.Sprintf("replicas %d->%d", *oldReplicas, desiredReplicas)
			}
		}
		if oldImage != "" && oldImage != app.Spec.Workload.Image {
			drifted = true
			if detail == "" {
				detail = "image changed"
			}
		}
	}
	dt.record("Deployment", drifted, detail)

	// Check if deployment is ready
	return r.isDeploymentReady(deployment), nil
}

// buildDeployment creates a Deployment from the Application spec.
func (r *ApplicationReconciler) buildDeployment(app *platformv1alpha1.Application) *appsv1.Deployment {
	labels := r.buildLabels(app)
	replicas := int32(1)
	if app.Spec.Workload.Replicas != nil {
		replicas = *app.Spec.Workload.Replicas
	}

	// Build container ports
	containerPorts := make([]corev1.ContainerPort, 0, len(app.Spec.Workload.Ports))
	for _, p := range app.Spec.Workload.Ports {
		containerPorts = append(containerPorts, corev1.ContainerPort{
			Name:          p.Name,
			ContainerPort: p.ContainerPort,
			Protocol:      p.Protocol,
		})
	}

	// Build probes if health check specified
	var livenessProbe, readinessProbe *corev1.Probe
	if app.Spec.Workload.HealthCheck != nil {
		hc := app.Spec.Workload.HealthCheck
		probe := &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: hc.Path,
					Port: intstr.FromInt32(hc.Port),
				},
			},
			InitialDelaySeconds: hc.InitialDelaySeconds,
			PeriodSeconds:       hc.PeriodSeconds,
			FailureThreshold:    hc.FailureThreshold,
		}
		livenessProbe = probe
		readinessProbe = probe.DeepCopy()
	}

	// Build environment variables
	// =========================================================================
	// CREDENTIAL INJECTION:
	//   The platform auto-injects well-known env vars for provisioned infra.
	//   This is the Heroku/Railway pattern: declare a database in your spec,
	//   and DATABASE_URL + PG* vars appear in your container automatically.
	//
	//   Flow:
	//     1. Start with user-defined env vars (always take precedence)
	//     2. Collect user-defined names into a set for conflict detection
	//     3. If injectCredentials != false, append infra credential vars
	//        (skipping any that conflict with user-defined names)
	//     4. Pass envFrom directly for bulk Secret/ConfigMap mounts
	//
	//   WHY USER VARS WIN:
	//     If a user explicitly sets DATABASE_URL in their env list, they
	//     probably have a reason (external DB, connection pooler, etc.).
	//     Silently overwriting it would cause hard-to-debug failures.
	// =========================================================================
	env := r.buildEnvVars(app)

	// Build resource requirements (with nil-safe handling)
	// =========================================================================
	// NIL SAFETY:
	//   Resources might be nil if user didn't specify limits/requests.
	//   Instead of panicking, we use an empty ResourceRequirements.
	//   Kubernetes will use defaults from LimitRange or none.
	//
	// BEST PRACTICE:
	//   Always check for nil before dereferencing pointers.
	//   This prevents runtime panics and makes tests more robust.
	// =========================================================================
	var resources corev1.ResourceRequirements
	if app.Spec.Workload.Resources != nil {
		resources = *app.Spec.Workload.Resources
	}

	// Build the container
	container := corev1.Container{
		Name:           "app",
		Image:          app.Spec.Workload.Image,
		Ports:          containerPorts,
		Resources:      resources,
		Env:            env,
		EnvFrom:        app.Spec.Workload.EnvFrom,
		LivenessProbe:  livenessProbe,
		ReadinessProbe: readinessProbe,
	}

	if len(app.Spec.Workload.Command) > 0 {
		container.Command = app.Spec.Workload.Command
	}
	if len(app.Spec.Workload.Args) > 0 {
		container.Args = app.Spec.Workload.Args
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     app.Name,
					"app.kubernetes.io/instance": app.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
				},
			},
		},
	}
}

// =============================================================================
// CREDENTIAL INJECTION
// =============================================================================
//
// buildEnvVars constructs the final env var list for the workload container.
// It merges user-defined env vars with auto-injected infrastructure credentials.
//
// INJECTION RULES:
//   1. User-defined env vars (from spec.workload.env) always take precedence
//   2. If injectCredentials is true (default), append credential env vars
//      for each provisioned infrastructure type (database, cache, queue)
//   3. Skip any auto-injected var whose name conflicts with a user-defined var
//
// The injected vars use secretKeyRef to reference the credential Secrets
// created by the InfrastructureProvider. This means:
//   - No credential values are stored in the Application spec
//   - Credential rotation (Secret update) takes effect on next pod restart
//   - The controller never needs to read actual credential values
//
// =============================================================================

func (r *ApplicationReconciler) buildEnvVars(app *platformv1alpha1.Application) []corev1.EnvVar {
	// Start with user-defined env vars — these always take precedence.
	env := make([]corev1.EnvVar, len(app.Spec.Workload.Env))
	copy(env, app.Spec.Workload.Env)

	// Check if injection is disabled. Default is true (inject).
	if app.Spec.Workload.InjectCredentials != nil && !*app.Spec.Workload.InjectCredentials {
		return env
	}

	// Build a set of user-defined env var names for conflict detection.
	userDefined := make(map[string]struct{}, len(env))
	for _, e := range env {
		userDefined[e.Name] = struct{}{}
	}

	// Inject database credentials if database is in the spec.
	if app.Spec.Database != nil {
		secretName := fmt.Sprintf("%s-db-credentials", app.Name)
		env = appendIfNotDefined(env, userDefined, buildDatabaseEnvVars(secretName, app.Spec.Database.Type)...)
	}

	// Inject cache credentials if cache is in the spec.
	if app.Spec.Cache != nil {
		secretName := fmt.Sprintf("%s-cache-credentials", app.Name)
		env = appendIfNotDefined(env, userDefined, buildCacheEnvVars(secretName)...)
	}

	// Inject queue credentials if queue is in the spec.
	if app.Spec.Queue != nil {
		secretName := fmt.Sprintf("%s-queue-credentials", app.Name)
		env = appendIfNotDefined(env, userDefined, buildQueueEnvVars(secretName)...)
	}

	return env
}

// buildDatabaseEnvVars returns the env vars for a database credential Secret.
//
// For PostgreSQL, the var names follow libpq conventions (PGHOST, PGPORT, etc.)
// which are natively understood by every PostgreSQL client library — psql, libpq,
// psycopg2, node-postgres, JDBC, etc. No application code changes needed.
//
// DATABASE_URL follows the 12-factor app convention used by Rails, Django,
// SQLAlchemy, Prisma, Sequelize, and most modern ORMs.
func buildDatabaseEnvVars(secretName string, dbType platformv1alpha1.DatabaseType) []corev1.EnvVar {
	// Connection URL — universal across all database types.
	vars := []corev1.EnvVar{
		secretEnvVar("DATABASE_URL", secretName, "connectionString"),
	}

	// Type-specific well-known env var names.
	switch dbType {
	case platformv1alpha1.DatabasePostgres:
		vars = append(vars,
			secretEnvVar("PGHOST", secretName, "host"),
			secretEnvVar("PGPORT", secretName, "port"),
			secretEnvVar("PGUSER", secretName, "username"),
			secretEnvVar("PGPASSWORD", secretName, "password"),
			secretEnvVar("PGDATABASE", secretName, "database"),
		)
	case platformv1alpha1.DatabaseMySQL:
		vars = append(vars,
			secretEnvVar("MYSQL_HOST", secretName, "host"),
			secretEnvVar("MYSQL_PORT", secretName, "port"),
			secretEnvVar("MYSQL_USER", secretName, "username"),
			secretEnvVar("MYSQL_PASSWORD", secretName, "password"),
			secretEnvVar("MYSQL_DATABASE", secretName, "database"),
		)
	}

	return vars
}

// buildCacheEnvVars returns the env vars for a cache credential Secret.
// REDIS_URL follows the convention used by Sidekiq, Bull, ioredis, and redis-py.
func buildCacheEnvVars(secretName string) []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnvVar("REDIS_URL", secretName, "connectionString"),
		secretEnvVar("REDIS_HOST", secretName, "host"),
		secretEnvVar("REDIS_PORT", secretName, "port"),
		secretEnvVar("REDIS_PASSWORD", secretName, "password"),
	}
}

// buildQueueEnvVars returns the env vars for a queue credential Secret.
// AMQP_URL follows the convention used by Celery, Bunny, amqplib, and pika.
func buildQueueEnvVars(secretName string) []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnvVar("AMQP_URL", secretName, "connectionString"),
		secretEnvVar("RABBITMQ_HOST", secretName, "host"),
		secretEnvVar("RABBITMQ_PORT", secretName, "port"),
		secretEnvVar("RABBITMQ_USER", secretName, "username"),
		secretEnvVar("RABBITMQ_PASSWORD", secretName, "password"),
	}
}

// secretEnvVar builds a corev1.EnvVar that reads from a Secret key.
// This is the Kubernetes-native way to inject credentials without exposing
// them in the pod spec — the kubelet reads the Secret at pod startup.
func secretEnvVar(envName, secretName, secretKey string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: secretName,
				},
				Key: secretKey,
			},
		},
	}
}

// appendIfNotDefined appends env vars to the list, skipping any whose name
// already exists in the userDefined set. This ensures user-specified env vars
// always take precedence over auto-injected credentials.
func appendIfNotDefined(env []corev1.EnvVar, userDefined map[string]struct{}, vars ...corev1.EnvVar) []corev1.EnvVar {
	for _, v := range vars {
		if _, exists := userDefined[v.Name]; !exists {
			env = append(env, v)
		}
	}
	return env
}

// isDeploymentReady checks if deployment has reached desired state.
func (r *ApplicationReconciler) isDeploymentReady(deployment *appsv1.Deployment) bool {
	// A deployment is ready when:
	// 1. ObservedGeneration matches Generation (controller has seen spec)
	// 2. Replicas == ReadyReplicas == AvailableReplicas
	// 3. UpdatedReplicas == Replicas (rollout complete)

	if deployment.Status.ObservedGeneration < deployment.Generation {
		return false
	}

	desiredReplicas := int32(1)
	if deployment.Spec.Replicas != nil {
		desiredReplicas = *deployment.Spec.Replicas
	}

	return deployment.Status.ReadyReplicas == desiredReplicas &&
		deployment.Status.AvailableReplicas == desiredReplicas &&
		deployment.Status.UpdatedReplicas == desiredReplicas
}

// =============================================================================
// RECONCILE SERVICE
// =============================================================================
//
// Creates or updates the Service for this Application.
// The Service provides stable networking for the Deployment's pods.
//
// SERVICE VS INGRESS:
//   - Service: Internal cluster networking (ClusterIP)
//   - Ingress: External HTTP/HTTPS traffic (later milestone)
//
// =============================================================================

func (r *ApplicationReconciler) reconcileService(ctx context.Context, app *platformv1alpha1.Application, dt *driftTracker) (bool, error) {
	logger := log.FromContext(ctx)

	desired := r.buildService(app)

	// Set owner reference
	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return false, fmt.Errorf("failed to set owner reference: %w", err)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	// Capture the live port numbers before overwrite so we can detect a real
	// port edit (vs. server defaulting noise from CreateOrUpdate).
	var oldPorts []int32

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		oldPorts = servicePortNumbers(service.Spec.Ports)

		// Preserve ClusterIP on update (immutable field)
		clusterIP := service.Spec.ClusterIP

		service.Spec = desired.Spec
		service.Labels = desired.Labels

		// Restore ClusterIP if it was set (empty string means new service)
		if clusterIP != "" {
			service.Spec.ClusterIP = clusterIP
		}

		return controllerutil.SetControllerReference(app, service, r.Scheme)
	})

	if err != nil {
		return false, fmt.Errorf("failed to create/update service: %w", err)
	}

	switch opResult {
	case controllerutil.OperationResultCreated:
		logger.Info("created service", "service", service.Name)
		r.Recorder.Event(app, corev1.EventTypeNormal, "ServiceCreated",
			fmt.Sprintf("Created Service %s", service.Name))
	case controllerutil.OperationResultUpdated:
		logger.Info("updated service", "service", service.Name)
		r.Recorder.Event(app, corev1.EventTypeNormal, "ServiceUpdated",
			fmt.Sprintf("Updated Service %s", service.Name))
	}

	drifted := false
	detail := ""
	switch opResult {
	case controllerutil.OperationResultCreated:
		drifted, detail = true, "recreated"
	case controllerutil.OperationResultUpdated:
		if !equalInt32Slices(oldPorts, servicePortNumbers(desired.Spec.Ports)) {
			drifted, detail = true, "ports changed"
		}
	}
	dt.record("Service", drifted, detail)

	return true, nil // Services are "ready" immediately
}

// buildService creates a Service from the Application spec.
func (r *ApplicationReconciler) buildService(app *platformv1alpha1.Application) *corev1.Service {
	labels := r.buildLabels(app)

	ports := make([]corev1.ServicePort, 0, len(app.Spec.Workload.Ports))
	for _, p := range app.Spec.Workload.Ports {
		ports = append(ports, corev1.ServicePort{
			Name:       p.Name,
			Port:       p.ContainerPort,
			TargetPort: intstr.FromInt32(p.ContainerPort),
			Protocol:   p.Protocol,
		})
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name":     app.Name,
				"app.kubernetes.io/instance": app.Name,
			},
			Ports: ports,
			Type:  corev1.ServiceTypeClusterIP,
		},
	}
}

// =============================================================================
// RECONCILE CONFIGMAP
// =============================================================================
//
// Creates or updates the ConfigMap for this Application.
// Used for non-sensitive configuration.
//
// =============================================================================

func (r *ApplicationReconciler) reconcileConfigMap(ctx context.Context, app *platformv1alpha1.Application, dt *driftTracker) (bool, error) {
	logger := log.FromContext(ctx)

	desired := r.buildConfigMap(app)

	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return false, fmt.Errorf("failed to set owner reference: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = desired.Data
		cm.Labels = desired.Labels
		return controllerutil.SetControllerReference(app, cm, r.Scheme)
	})

	if err != nil {
		return false, fmt.Errorf("failed to create/update configmap: %w", err)
	}

	switch opResult {
	case controllerutil.OperationResultCreated:
		logger.Info("created configmap", "configmap", cm.Name)
		r.Recorder.Event(app, corev1.EventTypeNormal, "ConfigMapCreated",
			fmt.Sprintf("Created ConfigMap %s", cm.Name))
	case controllerutil.OperationResultUpdated:
		logger.Info("updated configmap", "configmap", cm.Name)
	}

	cmDrifted, cmDetail := recoveredOrChanged(opResult)
	dt.record("ConfigMap", cmDrifted, cmDetail)

	return true, nil
}

func (r *ApplicationReconciler) buildConfigMap(app *platformv1alpha1.Application) *corev1.ConfigMap {
	labels := r.buildLabels(app)

	data := map[string]string{
		"APP_NAME": app.Name,
		"APP_TEAM": app.Spec.Team,
		"APP_TIER": string(app.Spec.Tier),
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Data: data,
	}
}

// =============================================================================
// RECONCILE SECRET
// =============================================================================
//
// Creates the Secret for this Application.
// Acts as a placeholder for sensitive data and infrastructure credentials.
//
// NOTE: We only create secrets, we don't update them after initial creation.
// This prevents overwriting secrets that were populated by external systems
// (like Terraform outputs, external-secrets operator, etc.)
//
// =============================================================================

func (r *ApplicationReconciler) reconcileSecret(ctx context.Context, app *platformv1alpha1.Application, dt *driftTracker) (bool, error) {
	logger := log.FromContext(ctx)

	desired := r.buildSecret(app)

	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return false, fmt.Errorf("failed to set owner reference: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		// Only set data on creation, not update
		// This prevents overwriting secrets populated by external systems
		if secret.CreationTimestamp.IsZero() {
			secret.Data = desired.Data
			secret.Type = desired.Type
		}
		secret.Labels = desired.Labels
		return controllerutil.SetControllerReference(app, secret, r.Scheme)
	})

	if err != nil {
		return false, fmt.Errorf("failed to create/update secret: %w", err)
	}

	if opResult == controllerutil.OperationResultCreated {
		logger.Info("created secret", "secret", secret.Name)
		r.Recorder.Event(app, corev1.EventTypeNormal, "SecretCreated",
			fmt.Sprintf("Created Secret %s", secret.Name))
	}

	secretDrifted, secretDetail := recoveredOrChanged(opResult)
	dt.record("Secret", secretDrifted, secretDetail)

	return true, nil
}

func (r *ApplicationReconciler) buildSecret(app *platformv1alpha1.Application) *corev1.Secret {
	labels := r.buildLabels(app)

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			// Placeholder - normally would be populated by infrastructure providers
		},
	}
}

// =============================================================================
// RECONCILE HPA
// =============================================================================
//
// Creates or updates the HorizontalPodAutoscaler.
// Deletes HPA if scaling spec is removed (consistent cleanup).
//
// =============================================================================

func (r *ApplicationReconciler) reconcileHPA(ctx context.Context, app *platformv1alpha1.Application, dt *driftTracker) (bool, error) {
	logger := log.FromContext(ctx)

	// =========================================================================
	// CLEANUP LOGIC: Delete HPA if scaling spec is removed
	// =========================================================================
	//
	// WHY WE NEED THIS:
	//   Owner references handle deletion when the Application is deleted,
	//   but if user just removes spec.Scaling, the HPA would be orphaned.
	//   We explicitly delete it to maintain consistency.
	//
	// =========================================================================

	if app.Spec.Scaling == nil {
		existingHPA := &autoscalingv2.HorizontalPodAutoscaler{}
		err := r.Get(ctx, client.ObjectKey{
			Name:      app.Name,
			Namespace: app.Namespace,
		}, existingHPA)

		if err == nil {
			// Found HPA but scaling spec is nil -> Delete it
			logger.Info("removing HPA as scaling spec is nil", "hpa", app.Name)
			if err := r.Delete(ctx, existingHPA); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("failed to delete HPA: %w", err)
			}
			r.Recorder.Event(app, corev1.EventTypeNormal, "HPADeleted",
				fmt.Sprintf("Deleted HPA %s (scaling spec removed)", app.Name))
		}
		return true, nil
	}

	desired := r.buildHPA(app)

	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return false, fmt.Errorf("failed to set owner reference: %w", err)
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
		hpa.Spec = desired.Spec
		hpa.Labels = desired.Labels
		return controllerutil.SetControllerReference(app, hpa, r.Scheme)
	})

	if err != nil {
		return false, fmt.Errorf("failed to create/update HPA: %w", err)
	}

	switch opResult {
	case controllerutil.OperationResultCreated:
		logger.Info("created HPA", "hpa", hpa.Name)
		r.Recorder.Event(app, corev1.EventTypeNormal, "HPACreated",
			fmt.Sprintf("Created HPA %s (min: %d, max: %d)",
				hpa.Name, *app.Spec.Scaling.MinReplicas, app.Spec.Scaling.MaxReplicas))
	case controllerutil.OperationResultUpdated:
		logger.Info("updated HPA", "hpa", hpa.Name)
	}

	hpaDrifted, hpaDetail := recoveredOrChanged(opResult)
	dt.record("HorizontalPodAutoscaler", hpaDrifted, hpaDetail)

	return true, nil
}

func (r *ApplicationReconciler) buildHPA(app *platformv1alpha1.Application) *autoscalingv2.HorizontalPodAutoscaler {
	labels := r.buildLabels(app)
	minReplicas := int32(1)
	if app.Spec.Scaling.MinReplicas != nil {
		minReplicas = *app.Spec.Scaling.MinReplicas
	}

	var metrics []autoscalingv2.MetricSpec
	for _, m := range app.Spec.Scaling.Metrics {
		switch m.Type {
		case "cpu":
			metrics = append(metrics, autoscalingv2.MetricSpec{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &m.Target,
					},
				},
			})
		case "memory":
			metrics = append(metrics, autoscalingv2.MetricSpec{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceMemory,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &m.Target,
					},
				},
			})
		}
	}

	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       app.Name,
			},
			MinReplicas: &minReplicas,
			MaxReplicas: app.Spec.Scaling.MaxReplicas,
			Metrics:     metrics,
		},
	}
}

// =============================================================================
// RECONCILE PDB
// =============================================================================
//
// Creates or updates the PodDisruptionBudget.
// Ensures availability during voluntary disruptions (upgrades, draining).
// Deletes PDB if tier changes to development (consistent cleanup).
//
// PDB SPEC MUTABILITY (K8s 1.27+):
//   PDB spec is now mutable! We can update minAvailable/maxUnavailable.
//   Previously (K8s < 1.27), PDB spec was immutable after creation.
//
// =============================================================================

func (r *ApplicationReconciler) reconcilePDB(ctx context.Context, app *platformv1alpha1.Application, dt *driftTracker) (bool, error) {
	logger := log.FromContext(ctx)

	// =========================================================================
	// CLEANUP LOGIC: Delete PDB if tier is development or no workload
	// =========================================================================
	//
	// WHY:
	//   - Development tier doesn't need availability guarantees
	//   - No workload means no pods to protect
	//   - Consistent cleanup like HPA
	//
	// =========================================================================

	if app.Spec.Tier == platformv1alpha1.TierDevelopment || app.Spec.Workload == nil {
		existingPDB := &policyv1.PodDisruptionBudget{}
		err := r.Get(ctx, client.ObjectKey{
			Name:      app.Name,
			Namespace: app.Namespace,
		}, existingPDB)

		if err == nil {
			// Found PDB but tier is development or no workload -> Delete it
			logger.Info("removing PDB (tier=development or no workload)", "pdb", app.Name)
			if err := r.Delete(ctx, existingPDB); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("failed to delete PDB: %w", err)
			}
			r.Recorder.Event(app, corev1.EventTypeNormal, "PDBDeleted",
				fmt.Sprintf("Deleted PDB %s (tier changed to development)", app.Name))
		}
		return true, nil
	}

	desired := r.buildPDB(app)

	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return false, fmt.Errorf("failed to set owner reference: %w", err)
	}

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Spec = desired.Spec
		pdb.Labels = desired.Labels
		return controllerutil.SetControllerReference(app, pdb, r.Scheme)
	})

	if err != nil {
		return false, fmt.Errorf("failed to create/update PDB: %w", err)
	}

	switch opResult {
	case controllerutil.OperationResultCreated:
		logger.Info("created PDB", "pdb", pdb.Name)
		r.Recorder.Event(app, corev1.EventTypeNormal, "PDBCreated",
			fmt.Sprintf("Created PDB %s for %s tier", pdb.Name, app.Spec.Tier))
	case controllerutil.OperationResultUpdated:
		logger.Info("updated PDB", "pdb", pdb.Name)
	}

	pdbDrifted, pdbDetail := recoveredOrChanged(opResult)
	dt.record("PodDisruptionBudget", pdbDrifted, pdbDetail)

	return true, nil
}

func (r *ApplicationReconciler) buildPDB(app *platformv1alpha1.Application) *policyv1.PodDisruptionBudget {
	labels := r.buildLabels(app)

	// Default to minAvailable 1 for high availability
	minAvailable := intstr.FromInt(1)

	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     app.Name,
					"app.kubernetes.io/instance": app.Name,
				},
			},
		},
	}
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

// labelValueRegex matches invalid characters for Kubernetes label values.
// Labels must match: (([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?
var labelValueRegex = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// sanitizeLabelValue converts a string to a valid Kubernetes label value.
// Replaces invalid characters with underscores and truncates to 63 chars.
func sanitizeLabelValue(s string) string {
	// Replace @ and other invalid chars with underscore
	sanitized := labelValueRegex.ReplaceAllString(s, "_")

	// Trim leading/trailing non-alphanumeric
	sanitized = strings.Trim(sanitized, "_.-")

	// Truncate to 63 characters (K8s label value limit)
	if len(sanitized) > 63 {
		sanitized = sanitized[:63]
	}

	// If empty after sanitization, return a default
	if sanitized == "" {
		sanitized = "unknown"
	}

	return sanitized
}

// buildLabels creates standard Kubernetes labels for resources.
func (r *ApplicationReconciler) buildLabels(app *platformv1alpha1.Application) map[string]string {
	return map[string]string{
		// Standard K8s labels (kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/)
		"app.kubernetes.io/name":       app.Name,
		"app.kubernetes.io/instance":   app.Name,
		"app.kubernetes.io/managed-by": "goplatform",
		"app.kubernetes.io/part-of":    "goplatform",

		// GoPlatform custom labels (sanitized to be valid label values)
		"platform.goplatform.io/team":  sanitizeLabelValue(app.Spec.Team),
		"platform.goplatform.io/owner": sanitizeLabelValue(app.Spec.Owner),
		"platform.goplatform.io/tier":  string(app.Spec.Tier),
	}
}

// updateWorkloadConditions sets workload-related conditions based on Deployment state.
func (r *ApplicationReconciler) updateWorkloadConditions(ctx context.Context, app *platformv1alpha1.Application, deploymentReady bool) {
	observedGeneration := app.Generation

	workloadStatus := metav1.ConditionFalse
	workloadReason := "DeploymentNotReady"
	workloadMessage := "Deployment is still progressing"

	if app.Spec.Workload == nil {
		workloadStatus = metav1.ConditionTrue
		workloadReason = "NoWorkload"
		workloadMessage = "No workload specified"
	} else if deploymentReady {
		workloadStatus = metav1.ConditionTrue
		workloadReason = "DeploymentReady"
		workloadMessage = "Deployment has reached desired state"
	}

	workloadCondition := platformv1alpha1.NewCondition(
		platformv1alpha1.ConditionTypeWorkloadReady,
		workloadStatus,
		workloadReason,
		workloadMessage,
		observedGeneration,
	)
	r.applyCondition(ctx, app, workloadCondition)
}

// updateInfrastructureConditions maps provider state to Kubernetes conditions.
// Returns true if all requested infrastructure components are ready.
func (r *ApplicationReconciler) updateInfrastructureConditions(
	ctx context.Context,
	app *platformv1alpha1.Application,
	infraState *provider.ResourceState,
) bool {
	observedGeneration := app.Generation

	getPhase := func(statePhase provider.ResourcePhase) provider.ResourcePhase {
		return statePhase
	}

	// Database
	dbPhase := provider.ResourceProvisioning
	if infraState != nil && infraState.Database != nil {
		dbPhase = getPhase(infraState.Database.Phase)
	}
	databaseReady := r.applyInfrastructureCondition(
		ctx,
		app,
		platformv1alpha1.ConditionTypeDatabaseReady,
		"Database",
		app.Spec.Database != nil,
		dbPhase,
		infraStateMessage(infraState, "database"),
		observedGeneration,
	)

	// Cache
	cachePhase := provider.ResourceProvisioning
	if infraState != nil && infraState.Cache != nil {
		cachePhase = getPhase(infraState.Cache.Phase)
	}
	cacheReady := r.applyInfrastructureCondition(
		ctx,
		app,
		platformv1alpha1.ConditionTypeCacheReady,
		"Cache",
		app.Spec.Cache != nil,
		cachePhase,
		infraStateMessage(infraState, "cache"),
		observedGeneration,
	)

	// Queue
	queuePhase := provider.ResourceProvisioning
	if infraState != nil && infraState.Queue != nil {
		queuePhase = getPhase(infraState.Queue.Phase)
	}
	queueReady := r.applyInfrastructureCondition(
		ctx,
		app,
		platformv1alpha1.ConditionTypeQueueReady,
		"Queue",
		app.Spec.Queue != nil,
		queuePhase,
		infraStateMessage(infraState, "queue"),
		observedGeneration,
	)

	// Storage
	storagePhase := provider.ResourceProvisioning
	if infraState != nil && infraState.Storage != nil {
		storagePhase = getPhase(infraState.Storage.Phase)
	}
	storageReady := r.applyInfrastructureCondition(
		ctx,
		app,
		platformv1alpha1.ConditionTypeStorageReady,
		"Storage",
		app.Spec.Storage != nil,
		storagePhase,
		infraStateMessage(infraState, "storage"),
		observedGeneration,
	)

	// Ensure Infrastructure status object exists when any infra is requested
	r.ensureInfrastructureStatus(app)

	return databaseReady && cacheReady && queueReady && storageReady
}

// applyInfrastructureCondition maps a ResourcePhase to a Kubernetes Condition.
func (r *ApplicationReconciler) applyInfrastructureCondition(
	ctx context.Context,
	app *platformv1alpha1.Application,
	conditionType string,
	componentName string,
	requested bool,
	phase provider.ResourcePhase,
	message string,
	observedGeneration int64,
) bool {
	status := metav1.ConditionFalse
	reason := fmt.Sprintf("%sProvisioning", componentName)
	if message == "" {
		message = fmt.Sprintf("%s provisioning", componentName)
	}

	if !requested {
		status = metav1.ConditionTrue
		reason = fmt.Sprintf("%sNotRequested", componentName)
		message = fmt.Sprintf("%s not requested", componentName)
	} else {
		switch phase {
		case provider.ResourceReady:
			status = metav1.ConditionTrue
			reason = fmt.Sprintf("%sReady", componentName)
			if message == "" {
				message = fmt.Sprintf("%s ready", componentName)
			}
		case provider.ResourceFailed:
			reason = fmt.Sprintf("%sFailed", componentName)
			if message == "" {
				message = fmt.Sprintf("%s failed", componentName)
			}
		case provider.ResourceDeleting:
			reason = fmt.Sprintf("%sDeleting", componentName)
			if message == "" {
				message = fmt.Sprintf("%s deleting", componentName)
			}
		case provider.ResourceUpdating:
			reason = fmt.Sprintf("%sUpdating", componentName)
			if message == "" {
				message = fmt.Sprintf("%s updating", componentName)
			}
		case provider.ResourceNotFound:
			reason = fmt.Sprintf("%sNotFound", componentName)
			if message == "" {
				message = fmt.Sprintf("%s not found", componentName)
			}
		case provider.ResourceProvisioning, provider.ResourceUnknown:
			// keep defaults
		default:
			reason = fmt.Sprintf("%sUnknown", componentName)
			if message == "" {
				message = fmt.Sprintf("%s unknown", componentName)
			}
		}
	}

	condition := platformv1alpha1.NewCondition(
		conditionType,
		status,
		reason,
		message,
		observedGeneration,
	)
	r.applyCondition(ctx, app, condition)

	return status == metav1.ConditionTrue
}

// applyInfrastructureStatus maps provider state to ApplicationStatus.Infrastructure.
func (r *ApplicationReconciler) applyInfrastructureStatus(app *platformv1alpha1.Application, infraState *provider.ResourceState) {
	if app == nil {
		return
	}

	if app.Spec.Database == nil && app.Spec.Cache == nil && app.Spec.Queue == nil && app.Spec.Storage == nil {
		app.Status.Infrastructure = nil
		return
	}

	r.ensureInfrastructureStatus(app)

	if app.Spec.Database != nil && infraState != nil && infraState.Database != nil {
		app.Status.Infrastructure.Database = &platformv1alpha1.DatabaseStatus{
			Endpoint:  infraState.Database.Endpoint,
			Port:      infraState.Database.Port,
			SecretRef: infraState.Database.SecretRef,
		}
	} else if app.Spec.Database == nil {
		app.Status.Infrastructure.Database = nil
	}

	if app.Spec.Cache != nil && infraState != nil && infraState.Cache != nil {
		app.Status.Infrastructure.Cache = &platformv1alpha1.CacheStatus{
			Endpoint: infraState.Cache.Endpoint,
			Port:     infraState.Cache.Port,
		}
	} else if app.Spec.Cache == nil {
		app.Status.Infrastructure.Cache = nil
	}

	if app.Spec.Queue != nil && infraState != nil && infraState.Queue != nil {
		app.Status.Infrastructure.Queue = &platformv1alpha1.QueueStatus{
			URL:                infraState.Queue.URL,
			ARN:                infraState.Queue.ARN,
			DeadLetterQueueURL: infraState.Queue.DeadLetterQueueURL,
		}
	} else if app.Spec.Queue == nil {
		app.Status.Infrastructure.Queue = nil
	}

	if app.Spec.Storage != nil && infraState != nil && infraState.Storage != nil {
		app.Status.Infrastructure.Storage = &platformv1alpha1.StorageStatus{
			BucketName: infraState.Storage.BucketName,
			Region:     infraState.Storage.Region,
		}
	} else if app.Spec.Storage == nil {
		app.Status.Infrastructure.Storage = nil
	}
}

// updateOverallReadyCondition sets the Ready condition based on workload, service, and infra.
func (r *ApplicationReconciler) updateOverallReadyCondition(
	ctx context.Context,
	app *platformv1alpha1.Application,
	serviceReady bool,
	infraReady bool,
) {
	observedGeneration := app.Generation

	workloadCond := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeWorkloadReady)
	workloadReady := workloadCond != nil && workloadCond.Status == metav1.ConditionTrue

	overallReady := workloadReady && serviceReady && infraReady

	readyReason := "ResourcesNotReady"
	readyMessage := "Some resources are still being provisioned"
	if overallReady {
		readyReason = "AllResourcesReady"
		readyMessage = "All resources are ready"
	} else if !serviceReady {
		readyReason = "ServiceNotReady"
		readyMessage = "Service is not ready"
	} else if !workloadReady {
		readyReason = "WorkloadNotReady"
		readyMessage = "Workload is not ready"
	} else if !infraReady {
		readyReason = "InfrastructureNotReady"
		readyMessage = "Infrastructure provisioning is pending"
	}

	readyStatus := metav1.ConditionFalse
	if overallReady {
		readyStatus = metav1.ConditionTrue
	}

	readyCondition := platformv1alpha1.NewCondition(
		platformv1alpha1.ConditionTypeReady,
		readyStatus,
		readyReason,
		readyMessage,
		observedGeneration,
	)
	r.applyCondition(ctx, app, readyCondition)
}

// getProvider returns the configured InfrastructureProvider.
// Lazily creates a factory if none is provided.
func (r *ApplicationReconciler) getProvider() (provider.InfrastructureProvider, error) {
	if r.ProviderFactory == nil {
		r.ProviderFactory = provider.NewFactory()
	}
	return r.ProviderFactory.GetProvider()
}

func infraStateMessage(state *provider.ResourceState, component string) string {
	if state == nil {
		return ""
	}
	switch component {
	case "database":
		if state.Database != nil {
			return state.Database.Message
		}
	case "cache":
		if state.Cache != nil {
			return state.Cache.Message
		}
	case "queue":
		if state.Queue != nil {
			return state.Queue.Message
		}
	case "storage":
		if state.Storage != nil {
			return state.Storage.Message
		}
	}
	return ""
}

// setFailedCondition updates status to indicate a failure.
func (r *ApplicationReconciler) setFailedCondition(ctx context.Context, app *platformv1alpha1.Application, reason, message string) {
	if app == nil {
		return
	}

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest platformv1alpha1.Application
		if err := r.Get(ctx, client.ObjectKeyFromObject(app), &latest); err != nil {
			return err
		}

		originalStatus := *latest.Status.DeepCopy()

		latest.Status.Phase = platformv1alpha1.ApplicationFailed
		latest.Status.ObservedGeneration = latest.Generation

		failedCondition := platformv1alpha1.NewCondition(
			platformv1alpha1.ConditionTypeReady,
			metav1.ConditionFalse,
			reason,
			message,
			latest.Generation,
		)
		r.applyCondition(ctx, &latest, failedCondition)

		if reflect.DeepEqual(originalStatus, latest.Status) {
			return nil
		}

		return r.Status().Update(ctx, &latest)
	})

	if retryErr != nil {
		log.FromContext(ctx).Error(retryErr, "failed to update failed status")
	}
}

// updatePhase updates the status phase with retry-on-conflict handling.
func (r *ApplicationReconciler) updatePhase(ctx context.Context, app *platformv1alpha1.Application, phase platformv1alpha1.ApplicationPhase) error {
	if app == nil {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest platformv1alpha1.Application
		if err := r.Get(ctx, client.ObjectKeyFromObject(app), &latest); err != nil {
			return err
		}

		if latest.Status.Phase == phase {
			return nil
		}

		originalStatus := *latest.Status.DeepCopy()
		latest.Status.Phase = phase
		latest.Status.ObservedGeneration = latest.Generation

		if reflect.DeepEqual(originalStatus, latest.Status) {
			return nil
		}

		return r.Status().Update(ctx, &latest)
	})
}

// applyCondition sets a condition and emits an Event when the status changes.
func (r *ApplicationReconciler) applyCondition(ctx context.Context, app *platformv1alpha1.Application, condition metav1.Condition) {
	if app == nil {
		return
	}

	previous := platformv1alpha1.GetCondition(app.Status.Conditions, condition.Type)
	previousStatus := metav1.ConditionUnknown
	if previous != nil {
		previousStatus = previous.Status
	}

	changed := platformv1alpha1.SetCondition(&app.Status.Conditions, condition)
	if !changed || previousStatus == condition.Status {
		return
	}

	if r.Recorder == nil {
		return
	}

	eventType := corev1.EventTypeNormal
	if condition.Status == metav1.ConditionFalse {
		eventType = corev1.EventTypeWarning
	}

	// Use the condition reason as the event reason for consistency.
	r.Recorder.Event(app, eventType, condition.Reason, fmt.Sprintf("%s: %s", condition.Type, condition.Message))
}

// ensureInfrastructureStatus initializes Infrastructure status when needed.
func (r *ApplicationReconciler) ensureInfrastructureStatus(app *platformv1alpha1.Application) {
	if app == nil || app.Status.Infrastructure != nil {
		return
	}

	if app.Spec.Database != nil || app.Spec.Cache != nil || app.Spec.Queue != nil || app.Spec.Storage != nil {
		app.Status.Infrastructure = &platformv1alpha1.InfrastructureStatus{}
	}
}

// =============================================================================
// SETUP WITH MANAGER
// =============================================================================
//
// Called once at startup to register this controller with the manager.
//
// WHAT THIS SETS UP:
//
//   1. PRIMARY WATCH:
//      For(&Application{}) → watch our CRD
//      Any create/update/delete triggers Reconcile
//
//   2. SECONDARY WATCHES:
//      Owns(&Deployment{}) → watch Deployments we created
//      Owns(&Service{}) → watch Services we created
//      If these change, reconcile the OWNER Application
//
//   3. PREDICATES:
//      =========================================================================
//      Filter events to reduce unnecessary reconciles.
//
//      WHY PREDICATES MATTER:
//        Without predicates, we reconcile on ANY update, including:
//        - Status-only changes (we caused these!)
//        - Metadata-only changes (labels, annotations)
//        - resourceVersion changes (every API write)
//
//        With GenerationChangedPredicate:
//        - Only reconcile when spec changes (Generation increments)
//        - Status updates don't trigger reconcile (ObservedGeneration != Generation)
//        - Greatly reduces unnecessary work
//
//      ALTERNATIVES CONSIDERED:
//      ┌────────────────────────────────────────────────────────────────────────┐
//      │ Predicate                 │ Filters Out                                │
//      ├───────────────────────────┼────────────────────────────────────────────┤
//      │ GenerationChangedPred.    │ Status-only updates                        │
//      │ AnnotationChangedPred.    │ Label/annotation-only updates              │
//      │ ResourceVersionChangedP.  │ Almost nothing (too broad)                 │
//      │ Custom Predicate          │ Whatever logic you define                  │
//      └────────────────────────────────────────────────────────────────────────┘
//
//      HOW CROSSPLANE/ARGOCD DO IT:
//        - Crossplane: Uses GenerationChanged + custom predicates
//        - ArgoCD: Custom predicates for specific fields
//        - Prometheus Operator: GenerationChanged + annotation predicates
//      =========================================================================
//
// THE MANAGER:
//   - Runs all controllers (we might have multiple)
//   - Manages shared cache
//   - Handles leader election
//   - Provides metrics endpoint
//
// =============================================================================

// SetupWithManager sets up the controller with the Manager.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		// Primary watch: our CRD with generation predicate
		// Only reconcile when spec changes, not on status updates
		For(&platformv1alpha1.Application{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Secondary watches: resources we own
		// When these change, find the owner Application and reconcile it
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&policyv1.PodDisruptionBudget{})

	// Conditionally watch monitoring resources if Prometheus operator CRDs are installed.
	// Calling .Owns() for a type whose CRD doesn't exist panics at startup.
	if r.DiscoveryClient != nil && r.isMonitoringCRDAvailable() {
		b = b.Owns(&monitoringv1.ServiceMonitor{}).
			Owns(&monitoringv1.PrometheusRule{})
	}

	return b.
		// Controller name (for metrics, logging)
		Named("application").
		Complete(r)
}
