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
// CUSTOM CONTROLLER METRICS
// =============================================================================
//
// These metrics provide observability into the controller's own performance.
// They are registered with controller-runtime's metrics registry and served
// on the metrics endpoint alongside built-in metrics (workqueue depth,
// reconcile duration from controller-runtime, etc.).
//
// METRIC TYPES:
//   - Histogram: Distribution of values (reconcile duration)
//   - Gauge: Current value that can go up or down (app phase, managed resources)
//   - Counter: Monotonically increasing value (error count)
//
// NAMING CONVENTIONS (Prometheus best practices):
//   - Prefix with subsystem: goplatform_controller_*
//   - Use _total suffix for counters
//   - Use _seconds suffix for durations
//   - Use snake_case
//
// HOW PRODUCTION OPERATORS DO IT:
//   - CNPG: Exposes pg_* metrics per cluster + controller metrics
//   - ArgoCD: Exposes argocd_app_* metrics per Application
//   - cert-manager: Exposes certmanager_controller_* metrics
//
// =============================================================================

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// reconcileDuration tracks how long each reconciliation takes.
	// High p99 indicates the controller is struggling (slow API server, complex resources).
	// Labels allow filtering by app name, namespace, and result (success/error/requeue).
	reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "goplatform",
			Subsystem: "controller",
			Name:      "reconcile_duration_seconds",
			Help:      "Duration of Application reconciliation in seconds.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"name", "namespace", "result"},
	)

	// applicationPhaseGauge tracks the current phase of each Application.
	// Useful for dashboards showing how many apps are in each phase.
	// Value is 1 for the current phase, deleted when app is removed.
	applicationPhaseGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "goplatform",
			Subsystem: "application",
			Name:      "phase_info",
			Help:      "Current phase of an Application (1 = current phase).",
		},
		[]string{"name", "namespace", "phase", "tier", "team"},
	)

	// reconcileErrorsTotal counts reconciliation errors by type.
	// A spike in errors indicates something is broken.
	reconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "goplatform",
			Subsystem: "controller",
			Name:      "reconcile_errors_total",
			Help:      "Total number of reconciliation errors.",
		},
		[]string{"name", "namespace", "error_type"},
	)

	// managedResourcesGauge tracks how many child resources the controller manages.
	// Useful for capacity planning and understanding controller load.
	managedResourcesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "goplatform",
			Subsystem: "controller",
			Name:      "managed_resources",
			Help:      "Number of child resources managed by the controller.",
		},
		[]string{"namespace", "resource_type"},
	)

	// applicationTotal tracks the total number of Application resources by tier.
	applicationTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "goplatform",
			Subsystem: "application",
			Name:      "total",
			Help:      "Total number of Application resources by tier.",
		},
		[]string{"namespace", "tier"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		reconcileDuration,
		applicationPhaseGauge,
		reconcileErrorsTotal,
		managedResourcesGauge,
		applicationTotal,
	)
}

// recordReconcileDuration observes the reconciliation duration for an Application.
func recordReconcileDuration(name, namespace, result string, durationSeconds float64) {
	reconcileDuration.WithLabelValues(name, namespace, result).Observe(durationSeconds)
}

// setApplicationPhase updates the phase gauge for an Application.
// It deletes stale phase labels and sets the current one to 1.
func setApplicationPhase(name, namespace, phase, tier, team string) {
	// Delete all possible phase values for this app first to avoid stale gauges
	for _, p := range []string{"Pending", "Provisioning", "Ready", "Failed", "Deleting"} {
		applicationPhaseGauge.DeleteLabelValues(name, namespace, p, tier, team)
	}
	applicationPhaseGauge.WithLabelValues(name, namespace, phase, tier, team).Set(1)
}

// deleteApplicationMetrics removes all metrics for a deleted Application.
func deleteApplicationMetrics(name, namespace, tier, team string) {
	for _, p := range []string{"Pending", "Provisioning", "Ready", "Failed", "Deleting"} {
		applicationPhaseGauge.DeleteLabelValues(name, namespace, p, tier, team)
	}
}

// incrementReconcileErrors increments the error counter for an Application.
func incrementReconcileErrors(name, namespace, errorType string) {
	reconcileErrorsTotal.WithLabelValues(name, namespace, errorType).Inc()
}

// setManagedResources updates the managed resource count for a resource type.
func setManagedResources(namespace, resourceType string, count float64) {
	managedResourcesGauge.WithLabelValues(namespace, resourceType).Set(count)
}

// setApplicationTotal updates the count of Applications in a namespace for a tier.
//
// NOTE ON GAUGE SEMANTICS: this gauge is an aggregate keyed by (namespace, tier),
// not per-Application. Callers must recompute it from a List and set EVERY known
// tier explicitly — including zero — on each pass. A Prometheus gauge retains its
// last value, so a tier that drops to zero would otherwise stay "stuck" at its
// previous count forever. See updateApplicationTotalMetrics in the reconciler.
func setApplicationTotal(namespace, tier string, count float64) {
	applicationTotal.WithLabelValues(namespace, tier).Set(count)
}
