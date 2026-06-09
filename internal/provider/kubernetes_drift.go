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
// INFRASTRUCTURE DRIFT DETECTION (Milestone 9)
// =============================================================================
//
// This implements the optional DriftDetector capability for KubernetesProvider.
//
// SPEC DRIFT vs STATE DRIFT — and why infra drift is DETECT-ONLY:
// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
//
//   Kubernetes child resources (Deployment, Service, ...) are AUTO-CORRECTED by
//   the controller: every reconcile runs controllerutil.CreateOrUpdate, which
//   overwrites any manual edit back to desired state. The controller owns them.
//
//   Operator CRDs (CNPG Cluster, RedisFailover, RabbitmqCluster) are owned by
//   THEIR operators. We provision them via CreateOrUpdate too, so Provision()
//   already re-applies desired values on the next reconcile. DetectDrift is a
//   READ-ONLY comparison layered on top: it answers "did someone edit the CRD
//   since we last applied it?" and reports it for visibility (events + status),
//   even within the window before the next Provision() pass overwrites it.
//
//   ┌──────────────┬───────────────────────┬───────────────────────────────┐
//   │ Resource     │ Owner                 │ Drift handling                │
//   ├──────────────┼───────────────────────┼───────────────────────────────┤
//   │ Deployment…  │ our controller        │auto-corrected (CreateOrUpdate)│
//   │ CNPG/Redis/… │ external operators    │detected + reported here       │
//   └──────────────┴───────────────────────┴───────────────────────────────┘
//
// We compare the SAME fields we set during provisioning, derived from the same
// spec inputs (HA → instances/replicas, size → storage), so "expected" here is
// guaranteed to match what Provision() would write.
// =============================================================================

package provider

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
)

// Compile-time assertion that KubernetesProvider satisfies the optional
// DriftDetector capability interface.
var _ DriftDetector = (*KubernetesProvider)(nil)

// DetectDrift compares the live operator CRDs against the values the Application
// spec implies. It is read-only and best-effort: a missing CRD is not reported
// as field drift (Provision recreates it; that path is the controller's
// "deleted resource recovery"), and only fields we actively manage are checked.
func (p *KubernetesProvider) DetectDrift(ctx context.Context, app *platformv1alpha1.Application) ([]DriftItem, error) {
	ns := p.resolveNamespace(app)
	var items []DriftItem

	if app.Spec.Database != nil {
		dbItems, err := p.detectDatabaseDrift(ctx, app, ns)
		if err != nil {
			return nil, err
		}
		items = append(items, dbItems...)
	}

	if app.Spec.Cache != nil && app.Spec.Cache.Type == platformv1alpha1.CacheRedis {
		cacheItems, err := p.detectCacheDrift(ctx, app, ns)
		if err != nil {
			return nil, err
		}
		items = append(items, cacheItems...)
	}

	if app.Spec.Queue != nil {
		queueItems, err := p.detectQueueDrift(ctx, app, ns)
		if err != nil {
			return nil, err
		}
		items = append(items, queueItems...)
	}

	return items, nil
}

// detectDatabaseDrift compares the CNPG Cluster's instance count and storage
// size against the spec (HA → 3 instances else 1; size → storage request).
func (p *KubernetesProvider) detectDatabaseDrift(ctx context.Context, app *platformv1alpha1.Application, ns string) ([]DriftItem, error) {
	name := fmt.Sprintf("%s-db", app.Name)
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := p.client.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // missing CRD → recreated by Provision, not field drift
		}
		return nil, fmt.Errorf("getting CNPG cluster %q for drift detection: %w", name, err)
	}

	var items []DriftItem

	expectedInstances := int64(1)
	if app.Spec.Database.HighAvailability {
		expectedInstances = 3
	}
	if actual, found, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances"); found && actual != expectedInstances {
		items = append(items, DriftItem{
			ResourceType: "database",
			Field:        "spec.instances",
			DesiredValue: fmt.Sprintf("%d", expectedInstances),
			ActualValue:  fmt.Sprintf("%d", actual),
			Severity:     "warning",
		})
	}

	expectedStorage := sizeToStorage(app.Spec.Database.Size)
	if actual, found, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "size"); found && actual != expectedStorage {
		// Storage shrink/grow on a live database is high-impact (data risk).
		items = append(items, DriftItem{
			ResourceType: "database",
			Field:        "spec.storage.size",
			DesiredValue: expectedStorage,
			ActualValue:  actual,
			Severity:     "critical",
		})
	}

	return items, nil
}

// detectCacheDrift compares the RedisFailover replica count against the spec
// (HA → 3 else 1). The provider sets redis and sentinel replicas together, so
// the redis replica count is a faithful proxy.
func (p *KubernetesProvider) detectCacheDrift(ctx context.Context, app *platformv1alpha1.Application, ns string) ([]DriftItem, error) {
	name := fmt.Sprintf("%s-cache", app.Name)
	redis := &unstructured.Unstructured{}
	redis.SetGroupVersionKind(redisFailoverGVK)
	if err := p.client.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, redis); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting RedisFailover %q for drift detection: %w", name, err)
	}

	expected := int64(1)
	if app.Spec.Cache.HighAvailability {
		expected = 3
	}
	if actual, found, _ := unstructured.NestedInt64(redis.Object, "spec", "redis", "replicas"); found && actual != expected {
		return []DriftItem{{
			ResourceType: "cache",
			Field:        "spec.redis.replicas",
			DesiredValue: fmt.Sprintf("%d", expected),
			ActualValue:  fmt.Sprintf("%d", actual),
			Severity:     "warning",
		}}, nil
	}

	return nil, nil
}

// detectQueueDrift compares the RabbitmqCluster replica count against the spec.
// The provider always provisions a single replica today, so any other value is
// an external edit.
func (p *KubernetesProvider) detectQueueDrift(ctx context.Context, app *platformv1alpha1.Application, ns string) ([]DriftItem, error) {
	name := fmt.Sprintf("%s-queue", app.Name)
	rabbit := &unstructured.Unstructured{}
	rabbit.SetGroupVersionKind(rabbitmqClusterGVK)
	if err := p.client.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, rabbit); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting RabbitmqCluster %q for drift detection: %w", name, err)
	}

	expected := int64(1)
	if actual, found, _ := unstructured.NestedInt64(rabbit.Object, "spec", "replicas"); found && actual != expected {
		return []DriftItem{{
			ResourceType: "queue",
			Field:        "spec.replicas",
			DesiredValue: fmt.Sprintf("%d", expected),
			ActualValue:  fmt.Sprintf("%d", actual),
			Severity:     "warning",
		}}, nil
	}

	return nil, nil
}
