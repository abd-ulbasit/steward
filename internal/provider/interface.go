/*
Copyright 2026 Steward Authors.

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

package provider

import (
	"context"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// =============================================================================
// INFRASTRUCTURE PROVIDER INTERFACE
// =============================================================================
//
// InfrastructureProvider is the core abstraction for cloud infrastructure.
// Each cloud provider (AWS, GCP, Local) implements this interface.
//
// DESIGN PRINCIPLES:
// ━━━━━━━━━━━━━━━━━━
//
//   1. LEVEL-TRIGGERED (not event-triggered)
//      - Provision() compares desired state (spec) with actual state
//      - Call Provision() repeatedly → same result (idempotent)
//      - No need to track "what changed" - just make reality match spec
//
//   2. AGGREGATE OPERATIONS
//      - One Provision() call handles ALL infrastructure for an Application
//      - Avoids N requests for N resources
//      - Provider can optimize (batch Terraform operations)
//
//   3. SYNCHRONOUS WITH ASYNC AWARENESS
//      - Methods block until current state is determined
//      - But infrastructure provisioning IS async (RDS takes 10+ minutes)
//      - Returns ResourceState with Phase=Provisioning if still in progress
//      - Controller requeues and calls again later
//
//   4. CLOUD-AGNOSTIC INPUT, PROVIDER-SPECIFIC IMPLEMENTATION
//      - Input: Application spec (database.type=postgres, size=small)
//      - Output: ResourceState (endpoint, port, credentials)
//      - Mapping: Provider translates (small → db.t3.medium on AWS)
//
// LIFECYCLE FLOW:
// ━━━━━━━━━━━━━━━
//
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │                     APPLICATION LIFECYCLE                               │
//   │                                                                         │
//   │  CREATE/UPDATE:                                                         │
//   │  ┌──────────────┐                                                       │
//   │  │ Application  │                                                       │
//   │  │ spec:        │                                                       │
//   │  │   database:  │──► Provision(ctx, app) ──► ResourceState{             │
//   │  │     type: pg │                             Database: {               │
//   │  │     size: md │                                Phase: Provisioning    │
//   │  └──────────────┘                                Endpoint: ""           │
//   │                                                  }                      │
//   │         │         Provisioning...              }                        │
//   │         │         (requeue after 30s)                                   │
//   │         ▼                                                               │
//   │  ┌──────────────┐                                                       │
//   │  │ Reconcile    │──► Provision(ctx, app) ──► ResourceState{             │
//   │  │ (after 30s)  │                             Database: {               │
//   │  └──────────────┘                                Phase: Ready           │
//   │                                                  Endpoint: "rds.xxx"    │
//   │                                                  SecretRef: {...}       │
//   │                                                  }                      │
//   │                                                }                        │
//   │                                                                         │
//   │  DELETE:                                                                │
//   │  ┌──────────────┐                                                       │
//   │  │ Application  │                                                       │
//   │  │ deleting...  │──► Destroy(ctx, app) ──► nil (success)                │
//   │  │              │    or                                                 │
//   │  │              │──► Destroy(ctx, app) ──► NotReadyError (in progress)  │
//   │  └──────────────┘    (requeue and retry)                                │
//   │                                                                         │
//   └─────────────────────────────────────────────────────────────────────────┘
//
// HOW REAL PLATFORMS COMPARE:
// ━━━━━━━━━━━━━━━━━━━━━━━━━━━
//
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │ Platform       │ Pattern                    │ Notes                     │
//   ├────────────────┼────────────────────────────┼───────────────────────────┤
//   │ Crossplane     │ ExternalClient interface   │ Per-resource-type         │
//   │                │ Observe/Create/Update/Del  │ Most similar to us        │
//   │                │                            │                           │
//   │ AWS ACK        │ SDK wrappers               │ Per-resource-type         │
//   │                │ Direct AWS API calls       │ No abstraction layer      │
//   │                │                            │                           │
//   │ Terraform      │ Provider plugins           │ Declarative (not Go iface)│
//   │                │ gRPC protocol              │ We use TF under the hood  │
//   │                │                            │                           │
//   │ Cluster API    │ InfrastructureReconciler   │ Per-cluster reconciler    │
//   │                │                            │ Similar scope to us       │
//   └─────────────────────────────────────────────────────────────────────────┘
//
// ERROR HANDLING:
// ━━━━━━━━━━━━━━━
//
//   Provider methods return typed errors (see errors.go):
//   - NotReadyError: Resource still provisioning, requeue
//   - QuotaExceededError: Limit hit, needs human intervention
//   - InvalidConfigError: User error in spec, surface to status
//   - ProvisioningError: General failure, retry
//
//   Example controller usage:
//
//   state, err := r.Provider.Provision(ctx, &app)
//   switch {
//   case provider.IsNotReady(err):
//       return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
//   case provider.IsQuotaExceeded(err):
//       r.setFailedCondition(app, "QuotaExceeded", err.Error())
//       return ctrl.Result{}, nil  // Don't retry
//   case err != nil:
//       return ctrl.Result{}, err  // Retry with backoff
//   }
//   // Update status with state
//
// =============================================================================

// InfrastructureProvider defines the interface for infrastructure provisioning.
//
// Each cloud provider (AWS, GCP, Local) implements this interface.
// The ApplicationReconciler uses this interface without knowing which
// provider is in use.
type InfrastructureProvider interface {
	// =========================================================================
	// PROVISION
	// =========================================================================
	//
	// Provision ensures all infrastructure for an Application exists and is
	// configured according to the spec.
	//
	// BEHAVIOR:
	//   - If resources don't exist: Create them
	//   - If resources exist but differ from spec: Update them
	//   - If resources exist and match spec: No-op (return current state)
	//
	// RETURNS:
	//   - ResourceState: Current state of all infrastructure
	//   - error: nil on success, typed error on failure
	//
	// IDEMPOTENCY:
	//   Calling Provision() multiple times with the same spec MUST produce
	//   the same result. This is fundamental to level-triggered reconciliation.
	//
	// ASYNC RESOURCES:
	//   Cloud resources take time to provision (RDS: 5-15 min, ElastiCache: 3-5 min).
	//   If provisioning is in progress:
	//     - Return ResourceState with Phase=Provisioning
	//     - Controller requeues and calls Provision() again later
	//     - Eventually returns Phase=Ready with connection info
	//
	// =========================================================================
	Provision(ctx context.Context, app *platformv1alpha1.Application) (*ResourceState, error)

	// =========================================================================
	// GET STATUS
	// =========================================================================
	//
	// GetStatus returns the current state of infrastructure without making
	// any changes.
	//
	// USE CASES:
	//   - Health checks: Verify infrastructure is still healthy
	//   - Status refresh: Update Application status without modifying infra
	//   - Drift detection: Compare actual state with desired state
	//
	// RETURNS:
	//   - ResourceState: Current state of all infrastructure
	//   - NotFoundError: If Application has no associated infrastructure
	//   - Other errors: Provider-specific issues
	//
	// =========================================================================
	GetStatus(ctx context.Context, app *platformv1alpha1.Application) (*ResourceState, error)

	// =========================================================================
	// DESTROY
	// =========================================================================
	//
	// Destroy removes all infrastructure for an Application.
	//
	// BEHAVIOR:
	//   - Called during Application deletion (finalizer pattern)
	//   - Removes ALL resources: database, cache, queue, storage, IAM roles
	//   - Cleans up Terraform state
	//
	// RETURNS:
	//   - nil: All resources successfully destroyed
	//   - NotReadyError: Destruction in progress, call again later
	//   - Other errors: Destruction failed, retry
	//
	// IDEMPOTENCY:
	//   Calling Destroy() on already-destroyed resources MUST be a no-op.
	//   This handles the case where controller crashes mid-deletion.
	//
	// CLEANUP ORDER:
	//   Provider should handle dependency order:
	//   1. Remove IAM roles (so nothing can access resources)
	//   2. Remove application resources (database, cache, etc.)
	//   3. Remove networking (security groups, if dedicated)
	//   4. Clean up state files
	//
	// =========================================================================
	Destroy(ctx context.Context, app *platformv1alpha1.Application) error

	// =========================================================================
	// METADATA
	// =========================================================================

	// Name returns the provider name (e.g., "aws", "gcp", "local", "mock").
	// Used for logging and status reporting.
	Name() string

	// Type returns the provider type enum.
	Type() ProviderType

	// Healthy returns true if the provider is properly configured and can
	// accept provisioning requests.
	//
	// USE CASES:
	//   - Startup health check: Verify credentials before accepting work
	//   - Readiness probe: Kubernetes readiness check for the controller
	//   - Error diagnosis: Surface configuration issues early
	//
	// CHECKS:
	//   - Credentials are valid
	//   - Required configuration is present
	//   - Backend is accessible (S3, DynamoDB for AWS)
	//
	Healthy(ctx context.Context) bool
}

// =============================================================================
// OPTIONAL PROVIDER CAPABILITIES
// =============================================================================
//
// These interfaces extend InfrastructureProvider with optional capabilities.
// Providers can implement these for additional features.
//
// WHY OPTIONAL INTERFACES:
//   Not all providers support all features. For example:
//   - CostEstimator: AWS can estimate costs, mock provider can't
//   - DriftDetector: Requires comparing actual vs desired state
//
//   Using separate interfaces:
//   1. Core interface stays simple
//   2. Controllers can check for capabilities at runtime
//   3. New features don't break existing providers
//
//   Example:
//   if estimator, ok := provider.(provider.CostEstimator); ok {
//       cost, _ := estimator.EstimateCost(ctx, app)
//       app.Status.EstimatedMonthlyCost = cost
//   }
//
// =============================================================================

// CostEstimator can estimate monthly costs before provisioning.
type CostEstimator interface {
	// EstimateCost returns estimated monthly cost in USD.
	// Called before provisioning to show users expected costs.
	EstimateCost(ctx context.Context, app *platformv1alpha1.Application) (*CostEstimate, error)
}

// CostEstimate contains the estimated monthly cost breakdown.
type CostEstimate struct {
	// TotalMonthly is the total estimated monthly cost in USD
	TotalMonthly float64 `json:"totalMonthly"`

	// Currency is the currency code (always "USD" for now)
	Currency string `json:"currency"`

	// Breakdown shows cost per resource type
	Breakdown map[string]float64 `json:"breakdown"`

	// Notes contains any assumptions or caveats
	Notes []string `json:"notes,omitempty"`
}

// DriftDetector can detect when infrastructure has drifted from desired state.
type DriftDetector interface {
	// DetectDrift compares current infrastructure state with desired state.
	// Returns a list of differences, or empty if in sync.
	DetectDrift(ctx context.Context, app *platformv1alpha1.Application) ([]DriftItem, error)
}

// DriftItem represents a single configuration drift.
type DriftItem struct {
	// ResourceType is the type of resource (database, cache, etc.)
	ResourceType string `json:"resourceType"`

	// Field is the drifted field (e.g., "instanceClass", "version")
	Field string `json:"field"`

	// DesiredValue is what the spec says
	DesiredValue string `json:"desiredValue"`

	// ActualValue is what's actually provisioned
	ActualValue string `json:"actualValue"`

	// Severity indicates impact (info, warning, critical)
	Severity string `json:"severity"`
}

// StateManager can import existing infrastructure or export state.
// Useful for brownfield deployments.
type StateManager interface {
	// Import imports existing infrastructure into provider state.
	// Used when adopting existing cloud resources.
	Import(ctx context.Context, app *platformv1alpha1.Application, resourceIDs map[string]string) error

	// Export exports the infrastructure state for debugging or migration.
	Export(ctx context.Context, app *platformv1alpha1.Application) ([]byte, error)
}
