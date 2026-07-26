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

// =============================================================================
// ADMISSION WEBHOOKS FOR APPLICATION CRD
// =============================================================================
//
// This file implements two admission webhooks for the Application resource:
//
//   1. MUTATING WEBHOOK (ApplicationCustomDefaulter)
//      - Runs FIRST in the admission chain
//      - Injects smart defaults and standard labels
//      - Cannot reject requests — only modify them
//
//   2. VALIDATING WEBHOOK (ApplicationCustomValidator)
//      - Runs AFTER the mutating webhook
//      - Enforces cross-field validation rules that kubebuilder markers can't express
//      - Can reject requests with detailed error messages
//
// HOW ADMISSION WORKS IN KUBERNETES:
//
//   ┌──────────┐    ┌──────────────┐    ┌────────────────┐    ┌───────┐
//   │ kubectl  │───►│  API Server  │───►│   Mutating WH  │───►│  etcd │
//   │  apply   │    │  (authn/z)   │    │   Validating WH│    │       │
//   └──────────┘    └──────────────┘    └────────────────┘    └───────┘
//
//   1. Request arrives at API server
//   2. Authentication & authorization checked
//   3. CRD schema validation (kubebuilder markers)
//   4. Mutating webhooks called (can modify the object)
//   5. Object re-validated against schema (catches mutations that break schema)
//   6. Validating webhooks called (can reject but not modify)
//   7. Object persisted to etcd
//
// WHY NOT JUST USE KUBEBUILDER MARKERS:
//
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │ Validation Type          │ Markers  │ Webhook  │ Example                │
//   ├──────────────────────────┼──────────┼──────────┼────────────────────────┤
//   │ Single-field enum        │ ✅ Yes   │ Overkill │ tier: critical/standard│
//   │ Single-field range       │ ✅ Yes   │ Overkill │ maxReplicas >= 1       │
//   │ Cross-field dependency   │ ❌ No    │ ✅ Yes   │ critical → HA required │
//   │ Conditional defaults     │ ❌ No    │ ✅ Yes   │ postgres → version "16"│
//   │ Immutable fields         │ ❌ No    │ ✅ Yes   │ database.type on update│
//   │ Version-specific ranges  │ ❌ No    │ ✅ Yes   │ postgres: 13-17        │
//   └──────────────────────────┴──────────┴──────────┴────────────────────────┘
//
// HOW OTHER OPERATORS HANDLE THIS:
//   - CNPG: Validates PostgreSQL version ranges, blocks engine changes
//   - cert-manager: Validates issuer references exist, blocks algorithm changes
//   - ArgoCD: Validates repo URLs are reachable, injects default sync policies
//   - Crossplane: Validates provider configs, blocks schema-breaking changes
//
// KUBEBUILDER V4 PATTERN (CustomValidator + CustomDefaulter):
//   Unlike the older pattern where the CRD type itself implements webhook.Validator,
//   the v4 pattern uses separate structs. Benefits:
//   1. API types stay pure data definitions
//   2. Webhook structs can hold dependencies (client, logger)
//   3. Independently testable without webhook server
//   4. Clear separation: types ≠ validation logic
//
// =============================================================================

package v1alpha1

import (
	"context"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// applicationlog is the logger for this webhook package.
// Named "application-resource" to match kubebuilder conventions.
var applicationlog = logf.Log.WithName("application-resource")

// SetupApplicationWebhookWithManager registers the webhook for Application in the manager.
//
// HOW THIS WORKS UNDER THE HOOD:
//
//	ctrl.NewWebhookManagedBy does several things:
//	1. Creates HTTP handlers for the mutating and validating webhook paths
//	2. Registers those handlers with the manager's webhook server
//	3. The paths are derived from the kubebuilder markers above each struct
//	4. When a request arrives, controller-runtime deserializes it, calls our methods,
//	   and serializes the response back to the API server
//
// The `&platformv1alpha1.Application{}` argument tells controller-runtime which
// GVK (Group-Version-Kind) this webhook handles. It uses reflection to determine
// the resource type and match it against incoming admission requests.
func SetupApplicationWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &platformv1alpha1.Application{}).
		WithValidator(&ApplicationCustomValidator{}).
		WithDefaulter(&ApplicationCustomDefaulter{}).
		Complete()
}

// =============================================================================
// MUTATING WEBHOOK — SMART DEFAULTS
// =============================================================================
//
// The mutating webhook runs BEFORE the validating webhook. This ordering matters:
//   1. User submits Application with tier=critical, no backup config
//   2. Mutating webhook injects backup defaults (enabled=true, retention=14)
//   3. Validating webhook sees the complete object WITH defaults applied
//
// This means the validator never sees "missing" defaults — they've already been
// injected. This is the same pattern the API server uses for built-in resources
// (e.g., Service defaults to ClusterIP, Pod defaults to RestartPolicy=Always).
//
// =============================================================================

// +kubebuilder:webhook:path=/mutate-platform-steward-sh-v1alpha1-application,mutating=true,failurePolicy=fail,sideEffects=None,groups=platform.steward.sh,resources=applications,verbs=create;update,versions=v1alpha1,name=mapplication-v1alpha1.kb.io,admissionReviewVersions=v1

// ApplicationCustomDefaulter is responsible for setting default values on the Application
// resource when it is created or updated.
//
// WHY A SEPARATE STRUCT (not methods on Application):
//
//	In kubebuilder v3, the Application type itself implemented webhook.Defaulter.
//	In v4, we use a separate struct because:
//	1. Application stays a pure data type (no logic)
//	2. This struct could hold a client for dynamic defaults (e.g., lookup cluster config)
//	3. Unit tests can instantiate this struct directly without a webhook server
type ApplicationCustomDefaulter struct{}

// Default implements webhook.CustomDefaulter. Called by the API server for every
// CREATE and UPDATE operation on Application resources.
//
// MUTATION RULES:
//  1. Inject standard Kubernetes labels (managed-by, team)
//  2. Set database version defaults based on engine type
//  3. Enable backup with sensible retention for critical tier
//
// IMPORTANT: Mutations must be idempotent. Running Default() twice on the same
// object must produce the same result. This is because the API server may retry
// admission requests.
func (d *ApplicationCustomDefaulter) Default(_ context.Context, obj *platformv1alpha1.Application) error {
	applicationlog.Info("Defaulting for Application", "name", obj.GetName())

	// -------------------------------------------------------------------------
	// 1. INJECT STANDARD LABELS
	// -------------------------------------------------------------------------
	//
	// WHY LABELS ON MUTATION (not in the controller):
	//   Labels set by the mutating webhook appear on the object BEFORE it's stored
	//   in etcd. This means:
	//   - kubectl get shows them immediately
	//   - Label selectors work from the moment of creation
	//   - No delay waiting for the controller to reconcile
	//
	// The controller also sets labels on child resources (Deployments, Services),
	// but the Application itself gets labels here at admission time.
	//
	// STANDARD KUBERNETES LABELS (https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/):
	//   app.kubernetes.io/managed-by — identifies the tool managing the resource
	//   This is used by tools like Helm, ArgoCD, and kubectl to identify ownership.
	//
	if obj.Labels == nil {
		obj.Labels = make(map[string]string)
	}

	// app.kubernetes.io/managed-by tells other tools (Helm, ArgoCD, etc.) that
	// Steward manages this resource. We don't overwrite if already set — this
	// respects cases where a higher-level tool (e.g., Flux) creates the Application.
	if _, exists := obj.Labels["app.kubernetes.io/managed-by"]; !exists {
		obj.Labels["app.kubernetes.io/managed-by"] = "steward"
	}

	// platform.steward.sh/team enables filtering and grouping by team:
	//   kubectl get applications -l platform.steward.sh/team=payments
	// Always sync from spec.team to ensure consistency.
	if obj.Spec.Team != "" {
		obj.Labels["platform.steward.sh/team"] = obj.Spec.Team
	}

	// -------------------------------------------------------------------------
	// 2. DATABASE VERSION DEFAULTS
	// -------------------------------------------------------------------------
	//
	// WHY CONDITIONAL DEFAULTS NEED A WEBHOOK:
	//   kubebuilder markers can only set static defaults:
	//     +kubebuilder:default="16"  ← always "16", regardless of database type
	//
	//   But we need conditional logic:
	//     postgres → default version "16" (latest stable as of 2024)
	//     mysql    → default version "8"  (latest GA branch)
	//
	//   This is impossible with markers alone. The webhook bridges the gap.
	//
	// VERSION SELECTION RATIONALE:
	//   ┌──────────┬─────────┬──────────────────────────────────────────────┐
	//   │ Engine   │ Default │ Why                                          │
	//   ├──────────┼─────────┼──────────────────────────────────────────────┤
	//   │ postgres │ "16"    │ Latest stable, CNPG default, best features   │
	//   │ mysql    │ "8"     │ MySQL 8.0 GA, widely supported               │
	//   └──────────┴─────────┴──────────────────────────────────────────────┘
	//
	if obj.Spec.Database != nil && obj.Spec.Database.Version == "" {
		switch obj.Spec.Database.Type {
		case platformv1alpha1.DatabasePostgres:
			obj.Spec.Database.Version = "16"
			applicationlog.Info("Defaulted database version", "type", "postgres", "version", "16")
		case platformv1alpha1.DatabaseMySQL:
			obj.Spec.Database.Version = "8"
			applicationlog.Info("Defaulted database version", "type", "mysql", "version", "8")
		}
	}

	// -------------------------------------------------------------------------
	// 3. BACKUP DEFAULTS FOR CRITICAL TIER
	// -------------------------------------------------------------------------
	//
	// WHY AUTO-ENABLE BACKUPS FOR CRITICAL:
	//   Critical tier means 99.99% uptime SLA. A database without backups
	//   violates that SLA because data loss = downtime. Rather than rejecting
	//   the request (which would annoy users), we inject sensible defaults.
	//
	//   This follows the "pit of success" pattern — make the right thing easy.
	//   Users can still override by explicitly setting backup config.
	//
	// 14-DAY RETENTION:
	//   - AWS RDS default: 7 days
	//   - GCP Cloud SQL default: 7 days
	//   - We use 14 for critical tier (2x industry default, covers 2 weekly cycles)
	//
	if obj.Spec.Tier == platformv1alpha1.TierCritical && obj.Spec.Database != nil && obj.Spec.Database.Backup == nil {
		enabled := true
		retentionDays := int32(14)
		obj.Spec.Database.Backup = &platformv1alpha1.BackupSpec{
			Enabled:       &enabled,
			RetentionDays: &retentionDays,
		}
		applicationlog.Info("Defaulted backup for critical tier", "retention", 14)
	}

	return nil
}

// =============================================================================
// VALIDATING WEBHOOK — CROSS-FIELD RULES
// =============================================================================
//
// The validating webhook enforces rules that kubebuilder markers cannot express.
// It runs AFTER the mutating webhook, so defaults have already been injected.
//
// KEY DESIGN PRINCIPLE: Return ALL errors at once.
//   Don't return the first error and make the user fix-and-retry in a loop.
//   Collect all validation errors into a field.ErrorList and return them together.
//   This matches kubectl behavior — "kubectl apply" shows all validation errors.
//
// USING field.ErrorList:
//   Kubernetes uses a structured error format: field.Error{Type, Field, Detail}.
//   This produces user-friendly error messages like:
//     spec.database.version: Invalid value: "12": PostgreSQL version must be between 13 and 17
//   The field path (spec.database.version) tells the user exactly WHERE the error is.
//
// =============================================================================

// +kubebuilder:webhook:path=/validate-platform-steward-sh-v1alpha1-application,mutating=false,failurePolicy=fail,sideEffects=None,groups=platform.steward.sh,resources=applications,verbs=create;update,versions=v1alpha1,name=vapplication-v1alpha1.kb.io,admissionReviewVersions=v1

// ApplicationCustomValidator is responsible for validating the Application resource
// when it is created, updated, or deleted.
type ApplicationCustomValidator struct{}

// ValidateCreate validates the Application on creation.
// Runs shared validation rules (cross-field checks, version ranges, scaling).
func (v *ApplicationCustomValidator) ValidateCreate(_ context.Context, obj *platformv1alpha1.Application) (admission.Warnings, error) {
	applicationlog.Info("Validation for Application upon creation", "name", obj.GetName())

	return validateApplication(obj)
}

// ValidateUpdate validates the Application on update.
// Runs shared validation rules PLUS immutability checks for destructive changes.
//
// WHY IMMUTABILITY ON UPDATE:
//
//	Some field changes are not just config changes — they're destructive operations.
//	Changing database.type from postgres to mysql means:
//	1. The PostgreSQL instance would be destroyed
//	2. All data would be lost (no cross-engine migration)
//	3. A new MySQL instance would be provisioned (empty)
//
//	This is NEVER what a user intends when editing a YAML field. The webhook blocks
//	it and forces the user to explicitly delete and recreate, which makes the
//	data-loss consequence visible and intentional.
//
//	This is the same pattern used by:
//	- CNPG: blocks changing postgresql engine type
//	- AWS ACK: blocks changing RDS engine family
//	- Crossplane: blocks changing provider references
func (v *ApplicationCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *platformv1alpha1.Application) (admission.Warnings, error) {
	applicationlog.Info("Validation for Application upon update", "name", newObj.GetName())

	// Run shared validation rules on the new object first.
	warnings, err := validateApplication(newObj)

	// Collect immutability errors alongside any shared validation errors.
	// We start with any errors from validateApplication, then append immutability errors.
	var allErrs field.ErrorList
	if err != nil {
		// Extract the field.ErrorList from the API error if possible.
		if statusErr, ok := err.(*apierrors.StatusError); ok {
			for _, cause := range statusErr.Status().Details.Causes {
				allErrs = append(allErrs, &field.Error{
					Type:   field.ErrorType(cause.Type),
					Field:  cause.Field,
					Detail: cause.Message,
				})
			}
		}
	}

	// -------------------------------------------------------------------------
	// IMMUTABILITY CHECKS
	// -------------------------------------------------------------------------
	//
	// Check each infrastructure type field for changes. Only check when BOTH
	// old and new have the field — adding or removing infrastructure is allowed.
	//
	// EDGE CASES:
	//   old: database=postgres, new: database=nil     → ALLOWED (removal)
	//   old: database=nil,      new: database=postgres → ALLOWED (addition)
	//   old: database=postgres, new: database=mysql    → BLOCKED (type change)
	//   old: database=postgres, new: database=postgres → ALLOWED (no change)
	//

	// Database type immutability
	if oldObj.Spec.Database != nil && newObj.Spec.Database != nil {
		if oldObj.Spec.Database.Type != newObj.Spec.Database.Type {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "database", "type"),
				fmt.Sprintf(
					"database type is immutable after creation (was %q, attempted change to %q); "+
						"delete and recreate the Application to change database engine",
					oldObj.Spec.Database.Type, newObj.Spec.Database.Type,
				),
			))
		}
	}

	// Queue type immutability
	if oldObj.Spec.Queue != nil && newObj.Spec.Queue != nil {
		if oldObj.Spec.Queue.Type != newObj.Spec.Queue.Type {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "queue", "type"),
				fmt.Sprintf(
					"queue type is immutable after creation (was %q, attempted change to %q); "+
						"delete and recreate the Application to change queue system",
					oldObj.Spec.Queue.Type, newObj.Spec.Queue.Type,
				),
			))
		}
	}

	// Cache type immutability
	if oldObj.Spec.Cache != nil && newObj.Spec.Cache != nil {
		if oldObj.Spec.Cache.Type != newObj.Spec.Cache.Type {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "cache", "type"),
				fmt.Sprintf(
					"cache type is immutable after creation (was %q, attempted change to %q); "+
						"delete and recreate the Application to change cache engine",
					oldObj.Spec.Cache.Type, newObj.Spec.Cache.Type,
				),
			))
		}
	}

	if len(allErrs) > 0 {
		return warnings, apierrors.NewInvalid(
			platformv1alpha1.GroupVersion.WithKind("Application").GroupKind(),
			newObj.Name,
			allErrs,
		)
	}

	return warnings, nil
}

// ValidateDelete validates the Application on deletion.
// No validation needed — the finalizer in the controller handles cleanup.
//
// WHY NO DELETE VALIDATION:
//
//	Some operators validate deletes to warn about dependents or prevent
//	accidental deletion of critical resources. We don't need this because:
//	1. The finalizer blocks actual deletion until cleanup completes
//	2. The controller handles graceful infrastructure teardown
//	3. kubectl delete --force bypasses webhooks anyway
func (v *ApplicationCustomValidator) ValidateDelete(_ context.Context, obj *platformv1alpha1.Application) (admission.Warnings, error) {
	applicationlog.Info("Validation for Application upon deletion", "name", obj.GetName())

	return nil, nil
}

// =============================================================================
// SHARED VALIDATION LOGIC
// =============================================================================

// validateApplication runs cross-field validation rules shared between
// ValidateCreate and ValidateUpdate.
//
// RETURNS ALL ERRORS AT ONCE:
//
//	Uses field.ErrorList to collect multiple errors, then converts to a single
//	apierrors.StatusError. This gives users a complete list of things to fix,
//	rather than making them play whack-a-mole fixing one error at a time.
//
//	Example output:
//	  The Application "my-app" is invalid:
//	  * spec.database.highAvailability: Forbidden: critical tier requires highAvailability for database
//	  * spec.database.version: Invalid value: "12": PostgreSQL version must be between 13 and 17
func validateApplication(app *platformv1alpha1.Application) (admission.Warnings, error) {
	var allErrs field.ErrorList

	// -------------------------------------------------------------------------
	// RULE 1: Critical tier requires HA for database
	// -------------------------------------------------------------------------
	//
	// WHY THIS RULE:
	//   Critical tier promises 99.99% uptime. A single-instance database is a
	//   single point of failure — if the pod or node goes down, the database is
	//   unavailable until Kubernetes reschedules it (minutes, not seconds).
	//   HA mode enables streaming replication with automatic failover (<30s).
	//
	//   We enforce this at admission time (not at reconcile time) because:
	//   - Fail-fast: user sees the error immediately on kubectl apply
	//   - Never enters a broken state in etcd
	//   - Clear error message guides the fix
	//
	if app.Spec.Tier == platformv1alpha1.TierCritical && app.Spec.Database != nil {
		if !app.Spec.Database.HighAvailability {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "database", "highAvailability"),
				"critical tier requires highAvailability to be enabled for database; "+
					"single-instance databases do not meet the 99.99% uptime SLA",
			))
		}
	}

	// -------------------------------------------------------------------------
	// RULE 2: Critical tier requires HA for cache
	// -------------------------------------------------------------------------
	//
	// Same reasoning as database HA — a single Redis instance is a SPOF.
	// Redis Sentinel or Cluster mode provides automatic failover.
	//
	if app.Spec.Tier == platformv1alpha1.TierCritical && app.Spec.Cache != nil {
		if !app.Spec.Cache.HighAvailability {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "cache", "highAvailability"),
				"critical tier requires highAvailability to be enabled for cache; "+
					"single-instance caches do not meet the 99.99% uptime SLA",
			))
		}
	}

	// -------------------------------------------------------------------------
	// RULE 3 & 4: Database version range validation
	// -------------------------------------------------------------------------
	//
	// WHY VERSION RANGES:
	//   The KubernetesProvider creates CNPG Cluster CRs with the version as the
	//   PostgreSQL image tag. If the user specifies version "1", CNPG would try
	//   to pull postgres:1 and fail with a cryptic image pull error.
	//
	//   Validating the version at admission time gives clear, actionable errors
	//   instead of letting invalid versions propagate to infrastructure provisioning.
	//
	// SUPPORTED RANGES:
	//   PostgreSQL: 13-17 (13 = oldest supported, 17 = latest stable)
	//   MySQL: 5 (5.7.x) and 8 (8.0.x) — the two maintained branches
	//
	if app.Spec.Database != nil {
		versionPath := field.NewPath("spec", "database", "version")
		version := app.Spec.Database.Version

		if version != "" {
			versionNum, err := strconv.Atoi(version)
			if err != nil {
				allErrs = append(allErrs, field.Invalid(
					versionPath,
					version,
					"database version must be a numeric string (e.g., \"16\" for PostgreSQL 16)",
				))
			} else {
				switch app.Spec.Database.Type {
				case platformv1alpha1.DatabasePostgres:
					if versionNum < 13 || versionNum > 17 {
						allErrs = append(allErrs, field.Invalid(
							versionPath,
							version,
							fmt.Sprintf(
								"PostgreSQL version must be between 13 and 17 (got %d); "+
									"versions below 13 are end-of-life, above 17 is not yet released",
								versionNum,
							),
						))
					}
				case platformv1alpha1.DatabaseMySQL:
					if versionNum != 5 && versionNum != 8 {
						allErrs = append(allErrs, field.Invalid(
							versionPath,
							version,
							fmt.Sprintf(
								"MySQL version must be 5 (for 5.7.x) or 8 (for 8.0.x) (got %d); "+
									"these are the only maintained MySQL branches",
								versionNum,
							),
						))
					}
				}
			}
		}
	}

	// -------------------------------------------------------------------------
	// RULE 5: Scaling minReplicas ≤ maxReplicas
	// -------------------------------------------------------------------------
	//
	// WHY THIS CHECK:
	//   HPA (HorizontalPodAutoscaler) will malfunction if minReplicas > maxReplicas.
	//   The Kubernetes HPA controller silently clamps to maxReplicas, which means
	//   the user's "minimum 5 replicas" intent would be silently ignored if
	//   maxReplicas is 3. Better to reject upfront.
	//
	if app.Spec.Scaling != nil && app.Spec.Scaling.MinReplicas != nil {
		if *app.Spec.Scaling.MinReplicas > app.Spec.Scaling.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec", "scaling", "minReplicas"),
				*app.Spec.Scaling.MinReplicas,
				fmt.Sprintf(
					"minReplicas (%d) must be less than or equal to maxReplicas (%d)",
					*app.Spec.Scaling.MinReplicas, app.Spec.Scaling.MaxReplicas,
				),
			))
		}
	}

	// Convert collected errors to a Kubernetes API error.
	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(
			platformv1alpha1.GroupVersion.WithKind("Application").GroupKind(),
			app.Name,
			allErrs,
		)
	}

	return nil, nil
}
