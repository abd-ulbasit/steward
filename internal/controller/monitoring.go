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
// MONITORING RESOURCE GENERATION
// =============================================================================
//
// This file generates Prometheus operator resources (ServiceMonitor, PrometheusRule)
// for Applications that have observability enabled.
//
// HOW THE PROMETHEUS OPERATOR ECOSYSTEM WORKS:
// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
//
//   The Prometheus operator watches for custom resources and configures
//   Prometheus instances accordingly:
//
//   ┌────────────────────────────────────────────────────────────────────────┐
//   │                     PROMETHEUS OPERATOR FLOW                          │
//   │                                                                       │
//   │  ServiceMonitor CR ──► Prometheus Operator ──► Prometheus Config      │
//   │  (our operator creates)  (watches CRs)         (scrape targets)      │
//   │                                                                       │
//   │  PrometheusRule CR ──► Prometheus Operator ──► Alerting Rules         │
//   │  (our operator creates)  (watches CRs)         (alert conditions)    │
//   │                                                                       │
//   │  HOW SCRAPING WORKS:                                                  │
//   │  1. ServiceMonitor selects a Service by label                        │
//   │  2. Prometheus discovers pods behind that Service                     │
//   │  3. Prometheus scrapes /metrics on each pod at the configured port    │
//   │  4. Metrics are stored in Prometheus TSDB                            │
//   │                                                                       │
//   │  HOW ALERTING WORKS:                                                  │
//   │  1. PrometheusRule defines PromQL conditions                         │
//   │  2. Prometheus evaluates rules at regular intervals                  │
//   │  3. If condition is true, alert fires → Alertmanager                 │
//   │  4. Alertmanager routes to Slack/PagerDuty/email                     │
//   └────────────────────────────────────────────────────────────────────────┘
//
// TIER-BASED ALERTING:
//
//   ┌──────────────┬──────────────┬──────────────┬──────────────┐
//   │  Threshold   │  critical    │  standard    │  development │
//   ├──────────────┼──────────────┼──────────────┼──────────────┤
//   │  Error rate  │  > 0.1% / 5m │ > 0.5% / 10m│  > 5% / 15m  │
//   │  P99 latency │  > 100ms / 5m│ > 500ms / 10m│ > 2s / 15m  │
//   │  Scrape int. │  15s         │  30s         │  60s         │
//   └──────────────┴──────────────┴──────────────┴──────────────┘
//
// HOW PRODUCTION OPERATORS DO IT:
//   - CNPG: Creates ServiceMonitor per PostgreSQL cluster with pg_* metrics
//   - ArgoCD: Creates ServiceMonitor for each ArgoCD component
//   - Strimzi (Kafka): Creates ServiceMonitor + PrometheusRule per cluster
//
// =============================================================================

package controller

import (
	"context"
	"fmt"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
)

// =============================================================================
// DISCOVERY INTERFACE
// =============================================================================

// DiscoveryInterface is a minimal interface for checking API resource availability.
// It matches the method signature from k8s.io/client-go/discovery.DiscoveryClient.
// We define our own interface to enable testing with fakes.
type DiscoveryInterface interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// monitoringGroupVersion is the API group for Prometheus operator CRDs.
const monitoringGroupVersion = "monitoring.coreos.com/v1"

// =============================================================================
// CRD DETECTION
// =============================================================================

// isMonitoringCRDAvailable checks if the Prometheus operator CRDs (ServiceMonitor,
// PrometheusRule) are installed in the cluster. This prevents panics from trying
// to create resources whose CRD doesn't exist.
func (r *ApplicationReconciler) isMonitoringCRDAvailable() bool {
	if r.DiscoveryClient == nil {
		return false
	}

	resources, err := r.DiscoveryClient.ServerResourcesForGroupVersion(monitoringGroupVersion)
	if err != nil {
		return false
	}

	hasServiceMonitor := false
	hasPrometheusRule := false
	for _, resource := range resources.APIResources {
		switch resource.Kind {
		case "ServiceMonitor":
			hasServiceMonitor = true
		case "PrometheusRule":
			hasPrometheusRule = true
		}
	}

	return hasServiceMonitor && hasPrometheusRule
}

// =============================================================================
// MONITORING ORCHESTRATOR
// =============================================================================

// reconcileMonitoring is the top-level orchestrator for all monitoring resources.
// It decides whether to create or clean up ServiceMonitor and PrometheusRule
// based on the Application's observability spec and CRD availability.
func (r *ApplicationReconciler) reconcileMonitoring(ctx context.Context, app *platformv1alpha1.Application) error {
	logger := log.FromContext(ctx)

	// Check if Prometheus operator CRDs are available
	if !r.isMonitoringCRDAvailable() {
		logger.V(1).Info("Prometheus operator CRDs not installed, skipping monitoring resource creation")
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               platformv1alpha1.ConditionTypeMonitoringReady,
			Status:             metav1.ConditionFalse,
			Reason:             "PrometheusOperatorNotInstalled",
			Message:            "Prometheus operator CRDs (monitoring.coreos.com/v1) not found in cluster",
			ObservedGeneration: app.Generation,
		})
		return nil
	}

	// Determine if monitoring is enabled
	metricsEnabled := isMetricsEnabled(app)

	if !metricsEnabled {
		// Cleanup any existing monitoring resources
		if err := r.cleanupMonitoringResources(ctx, app); err != nil {
			return fmt.Errorf("cleaning up monitoring resources: %w", err)
		}
		// Remove the MonitoringReady condition entirely when not requested
		meta.RemoveStatusCondition(&app.Status.Conditions, platformv1alpha1.ConditionTypeMonitoringReady)
		return nil
	}

	// Reconcile ServiceMonitor
	if err := r.reconcileServiceMonitor(ctx, app); err != nil {
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               platformv1alpha1.ConditionTypeMonitoringReady,
			Status:             metav1.ConditionFalse,
			Reason:             "ServiceMonitorFailed",
			Message:            fmt.Sprintf("Failed to reconcile ServiceMonitor: %v", err),
			ObservedGeneration: app.Generation,
		})
		return fmt.Errorf("reconciling ServiceMonitor: %w", err)
	}

	// Reconcile PrometheusRule
	if err := r.reconcilePrometheusRule(ctx, app); err != nil {
		meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               platformv1alpha1.ConditionTypeMonitoringReady,
			Status:             metav1.ConditionFalse,
			Reason:             "PrometheusRuleFailed",
			Message:            fmt.Sprintf("Failed to reconcile PrometheusRule: %v", err),
			ObservedGeneration: app.Generation,
		})
		return fmt.Errorf("reconciling PrometheusRule: %w", err)
	}

	// All monitoring resources are ready
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               platformv1alpha1.ConditionTypeMonitoringReady,
		Status:             metav1.ConditionTrue,
		Reason:             "MonitoringConfigured",
		Message:            "ServiceMonitor and PrometheusRule are configured",
		ObservedGeneration: app.Generation,
	})

	return nil
}

// isMetricsEnabled returns true if the Application has observability metrics enabled.
// Metrics are enabled by default (nil Enabled pointer = true).
func isMetricsEnabled(app *platformv1alpha1.Application) bool {
	if app.Spec.Observability == nil || app.Spec.Observability.Metrics == nil {
		return false
	}
	// If Enabled is nil, default to true (opt-out pattern)
	if app.Spec.Observability.Metrics.Enabled == nil {
		return true
	}
	return *app.Spec.Observability.Metrics.Enabled
}

// =============================================================================
// SERVICEMONITOR
// =============================================================================

// reconcileServiceMonitor creates or updates a ServiceMonitor for the Application's
// workload metrics endpoint.
func (r *ApplicationReconciler) reconcileServiceMonitor(ctx context.Context, app *platformv1alpha1.Application) error {
	logger := log.FromContext(ctx)

	sm := &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
		// Set owner reference for garbage collection
		if err := controllerutil.SetControllerReference(app, sm, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference: %w", err)
		}

		// Build desired state
		desired := buildServiceMonitor(app)
		sm.Labels = desired.Labels
		sm.Spec = desired.Spec

		return nil
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate ServiceMonitor: %w", err)
	}

	if result != controllerutil.OperationResultNone {
		logger.Info("ServiceMonitor reconciled", "operation", result)
		r.Recorder.Event(app, corev1.EventTypeNormal, "ServiceMonitorConfigured",
			fmt.Sprintf("ServiceMonitor %s %s", app.Name, result))
	}

	return nil
}

// buildServiceMonitor constructs the desired ServiceMonitor spec from the Application.
func buildServiceMonitor(app *platformv1alpha1.Application) *monitoringv1.ServiceMonitor {
	metricsPath := "/metrics"
	metricsPort := "metrics"
	scrapeInterval := scrapeIntervalForTier(app.Spec.Tier)

	if app.Spec.Observability != nil && app.Spec.Observability.Metrics != nil {
		if app.Spec.Observability.Metrics.Path != "" {
			metricsPath = app.Spec.Observability.Metrics.Path
		}
		if app.Spec.Observability.Metrics.Port != 0 {
			metricsPort = fmt.Sprintf("%d", app.Spec.Observability.Metrics.Port)
		}
	}

	return &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    monitoringLabels(app),
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":       app.Name,
					"app.kubernetes.io/managed-by": "goplatform",
				},
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:     metricsPort,
					Path:     metricsPath,
					Interval: monitoringv1.Duration(scrapeInterval),
				},
			},
			NamespaceSelector: monitoringv1.NamespaceSelector{
				MatchNames: []string{app.Namespace},
			},
		},
	}
}

// =============================================================================
// PROMETHEUSRULE
// =============================================================================

// reconcilePrometheusRule creates or updates a PrometheusRule with tier-based
// alerting thresholds for the Application.
func (r *ApplicationReconciler) reconcilePrometheusRule(ctx context.Context, app *platformv1alpha1.Application) error {
	logger := log.FromContext(ctx)

	pr := &monitoringv1.PrometheusRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name + "-alerts",
			Namespace: app.Namespace,
		},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, pr, func() error {
		if err := controllerutil.SetControllerReference(app, pr, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference: %w", err)
		}

		desired := buildPrometheusRule(app)
		pr.Labels = desired.Labels
		pr.Spec = desired.Spec

		return nil
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate PrometheusRule: %w", err)
	}

	if result != controllerutil.OperationResultNone {
		logger.Info("PrometheusRule reconciled", "operation", result)
		r.Recorder.Event(app, corev1.EventTypeNormal, "PrometheusRuleConfigured",
			fmt.Sprintf("PrometheusRule %s-alerts %s", app.Name, result))
	}

	return nil
}

// buildPrometheusRule constructs the desired PrometheusRule with tier-based
// SLA alerts and standard health alerts.
func buildPrometheusRule(app *platformv1alpha1.Application) *monitoringv1.PrometheusRule {
	thresholds := alertThresholdsForTier(app.Spec.Tier)
	appLabel := app.Name
	nsLabel := app.Namespace

	return &monitoringv1.PrometheusRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name + "-alerts",
			Namespace: app.Namespace,
			Labels:    monitoringLabels(app),
		},
		Spec: monitoringv1.PrometheusRuleSpec{
			Groups: []monitoringv1.RuleGroup{
				slaRuleGroup(appLabel, nsLabel, thresholds),
				healthRuleGroup(appLabel, nsLabel, thresholds),
			},
		},
	}
}

// alertThresholds holds tier-specific alerting configuration.
type alertThresholds struct {
	// ErrorRatePercent is the error rate threshold (e.g., 0.1 = 0.1%)
	ErrorRatePercent string
	// ErrorRateDuration is how long the error rate must exceed the threshold
	ErrorRateDuration string
	// P99LatencyMs is the P99 latency threshold in milliseconds
	P99LatencyMs string
	// LatencyDuration is how long latency must exceed the threshold
	LatencyDuration string
	// Severity for SLA alerts
	Severity string
}

// alertThresholdsForTier returns alerting thresholds based on the Application's tier.
func alertThresholdsForTier(tier platformv1alpha1.ServiceTier) alertThresholds {
	switch tier {
	case platformv1alpha1.TierCritical:
		return alertThresholds{
			ErrorRatePercent:  "0.1",
			ErrorRateDuration: "5m",
			P99LatencyMs:      "100",
			LatencyDuration:   "5m",
			Severity:          "critical",
		}
	case platformv1alpha1.TierDevelopment:
		return alertThresholds{
			ErrorRatePercent:  "5",
			ErrorRateDuration: "15m",
			P99LatencyMs:      "2000",
			LatencyDuration:   "15m",
			Severity:          "info",
		}
	default: // standard
		return alertThresholds{
			ErrorRatePercent:  "0.5",
			ErrorRateDuration: "10m",
			P99LatencyMs:      "500",
			LatencyDuration:   "10m",
			Severity:          "warning",
		}
	}
}

// slaRuleGroup creates SLA-based alerting rules (error rate, latency).
func slaRuleGroup(app, namespace string, t alertThresholds) monitoringv1.RuleGroup {
	return monitoringv1.RuleGroup{
		Name: app + ".sla",
		Rules: []monitoringv1.Rule{
			{
				Alert: "HighErrorRate",
				Expr:  intstr.FromString(fmt.Sprintf(`sum(rate(http_requests_total{namespace="%s",app="%s",code=~"5.."}[5m])) / sum(rate(http_requests_total{namespace="%s",app="%s"}[5m])) * 100 > %s`, namespace, app, namespace, app, t.ErrorRatePercent)),
				For:   toPtr(monitoringv1.Duration(t.ErrorRateDuration)),
				Labels: map[string]string{
					"severity":  t.Severity,
					"app":       app,
					"namespace": namespace,
				},
				Annotations: map[string]string{
					"summary":     fmt.Sprintf("High error rate on %s", app),
					"description": fmt.Sprintf("Error rate for %s in %s is above %s%% for %s.", app, namespace, t.ErrorRatePercent, t.ErrorRateDuration),
				},
			},
			{
				Alert: "HighP99Latency",
				Expr:  intstr.FromString(fmt.Sprintf(`histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{namespace="%s",app="%s"}[5m])) by (le)) * 1000 > %s`, namespace, app, t.P99LatencyMs)),
				For:   toPtr(monitoringv1.Duration(t.LatencyDuration)),
				Labels: map[string]string{
					"severity":  t.Severity,
					"app":       app,
					"namespace": namespace,
				},
				Annotations: map[string]string{
					"summary":     fmt.Sprintf("High P99 latency on %s", app),
					"description": fmt.Sprintf("P99 latency for %s in %s is above %sms for %s.", app, namespace, t.P99LatencyMs, t.LatencyDuration),
				},
			},
		},
	}
}

// healthRuleGroup creates standard health alerts applicable to all tiers.
func healthRuleGroup(app, namespace string, t alertThresholds) monitoringv1.RuleGroup {
	return monitoringv1.RuleGroup{
		Name: app + ".health",
		Rules: []monitoringv1.Rule{
			{
				Alert: "PodCrashLooping",
				Expr:  intstr.FromString(fmt.Sprintf(`increase(kube_pod_container_status_restarts_total{namespace="%s",pod=~"%s-.*"}[15m]) > 3`, namespace, app)),
				For:   toPtr(monitoringv1.Duration("0m")),
				Labels: map[string]string{
					"severity":  "warning",
					"app":       app,
					"namespace": namespace,
				},
				Annotations: map[string]string{
					"summary":     fmt.Sprintf("Pod crash looping for %s", app),
					"description": fmt.Sprintf("A pod for %s in %s has restarted more than 3 times in 15 minutes.", app, namespace),
				},
			},
			{
				Alert: "HighRestartCount",
				Expr:  intstr.FromString(fmt.Sprintf(`increase(kube_pod_container_status_restarts_total{namespace="%s",pod=~"%s-.*"}[1h]) > 5`, namespace, app)),
				For:   toPtr(monitoringv1.Duration("0m")),
				Labels: map[string]string{
					"severity":  "warning",
					"app":       app,
					"namespace": namespace,
				},
				Annotations: map[string]string{
					"summary":     fmt.Sprintf("High restart count for %s", app),
					"description": fmt.Sprintf("A pod for %s in %s has restarted more than 5 times in 1 hour.", app, namespace),
				},
			},
			{
				Alert: "ContainerOOMKilled",
				Expr:  intstr.FromString(fmt.Sprintf(`kube_pod_container_status_last_terminated_reason{namespace="%s",pod=~"%s-.*",reason="OOMKilled"} == 1`, namespace, app)),
				For:   toPtr(monitoringv1.Duration("0m")),
				Labels: map[string]string{
					"severity":  t.Severity,
					"app":       app,
					"namespace": namespace,
				},
				Annotations: map[string]string{
					"summary":     fmt.Sprintf("Container OOM killed for %s", app),
					"description": fmt.Sprintf("A container for %s in %s was OOM killed. Consider increasing memory limits.", app, namespace),
				},
			},
			{
				Alert: "DeploymentStuck",
				Expr:  intstr.FromString(fmt.Sprintf(`kube_deployment_status_condition{namespace="%s",deployment="%s",condition="Progressing",status="false"} == 1`, namespace, app)),
				For:   toPtr(monitoringv1.Duration("15m")),
				Labels: map[string]string{
					"severity":  "warning",
					"app":       app,
					"namespace": namespace,
				},
				Annotations: map[string]string{
					"summary":     fmt.Sprintf("Deployment stuck for %s", app),
					"description": fmt.Sprintf("Deployment %s in %s has not been progressing for 15 minutes.", app, namespace),
				},
			},
		},
	}
}

// =============================================================================
// CLEANUP
// =============================================================================

// cleanupMonitoringResources deletes ServiceMonitor and PrometheusRule if they exist.
// Called when observability is disabled or removed from the Application spec.
func (r *ApplicationReconciler) cleanupMonitoringResources(ctx context.Context, app *platformv1alpha1.Application) error {
	logger := log.FromContext(ctx)

	// Delete ServiceMonitor
	sm := &monitoringv1.ServiceMonitor{}
	if err := r.Get(ctx, client.ObjectKey{Name: app.Name, Namespace: app.Namespace}, sm); err == nil {
		if err := r.Delete(ctx, sm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting ServiceMonitor: %w", err)
		}
		logger.Info("deleted ServiceMonitor", "name", app.Name)
		r.Recorder.Event(app, corev1.EventTypeNormal, "ServiceMonitorDeleted",
			fmt.Sprintf("ServiceMonitor %s deleted (observability disabled)", app.Name))
	}

	// Delete PrometheusRule
	pr := &monitoringv1.PrometheusRule{}
	if err := r.Get(ctx, client.ObjectKey{Name: app.Name + "-alerts", Namespace: app.Namespace}, pr); err == nil {
		if err := r.Delete(ctx, pr); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting PrometheusRule: %w", err)
		}
		logger.Info("deleted PrometheusRule", "name", app.Name+"-alerts")
		r.Recorder.Event(app, corev1.EventTypeNormal, "PrometheusRuleDeleted",
			fmt.Sprintf("PrometheusRule %s-alerts deleted (observability disabled)", app.Name))
	}

	return nil
}

// =============================================================================
// HELPERS
// =============================================================================

// monitoringLabels returns the standard set of labels for monitoring resources.
func monitoringLabels(app *platformv1alpha1.Application) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       app.Name,
		"app.kubernetes.io/managed-by": "goplatform",
		"app.kubernetes.io/part-of":    "goplatform",
		"platform.goplatform.io/team":  sanitizeMonitoringLabel(app.Spec.Team),
		"platform.goplatform.io/tier":  string(app.Spec.Tier),
	}
}

// scrapeIntervalForTier returns the Prometheus scrape interval based on tier.
// Critical services need faster detection of issues.
func scrapeIntervalForTier(tier platformv1alpha1.ServiceTier) string {
	switch tier {
	case platformv1alpha1.TierCritical:
		return "15s"
	case platformv1alpha1.TierDevelopment:
		return "60s"
	default:
		return "30s"
	}
}

// sanitizeMonitoringLabel ensures a label value is safe for Kubernetes labels.
// Kubernetes labels must be 63 chars or less, start/end with alphanumeric.
func sanitizeMonitoringLabel(s string) string {
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// toPtr returns a pointer to the given value.
func toPtr[T any](v T) *T {
	return &v
}
