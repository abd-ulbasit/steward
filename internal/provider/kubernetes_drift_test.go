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

package provider

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestKubernetesProvider_DetectDrift provisions the operator CRDs at desired
// state, then mutates each one out-of-band and asserts DetectDrift reports the
// difference for database, cache, and queue.
func TestKubernetesProvider_DetectDrift(t *testing.T) {
	discovery := &fakeDiscovery{
		resources: map[string][]metav1.APIResource{
			cnpgClusterGVK.GroupVersion().String():     {{Name: "clusters", Kind: cnpgClusterGVK.Kind}},
			redisFailoverGVK.GroupVersion().String():   {{Name: "redisfailovers", Kind: redisFailoverGVK.Kind}},
			rabbitmqClusterGVK.GroupVersion().String(): {{Name: "rabbitmqclusters", Kind: rabbitmqClusterGVK.Kind}},
		},
	}

	p, cl, _ := buildKubernetesProvider(t, discovery)
	ctx := context.Background()
	app := createInfraApplication("payments") // SizeSmall, HA=false → expect 1 instance/replica

	// Provision creates the CRDs at desired state (NotReady is expected with a
	// fake client because the CRDs carry no ready status).
	if _, err := p.Provision(ctx, app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	// Freshly provisioned: actual == desired, so no drift.
	items, err := p.DetectDrift(ctx, app)
	if err != nil {
		t.Fatalf("DetectDrift() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no drift immediately after provision, got %+v", items)
	}

	// Mutate each CRD to simulate external edits.
	mutate := func(gvk schema.GroupVersionKind, name string, value int64, path ...string) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := cl.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, obj); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if err := unstructured.SetNestedField(obj.Object, value, path...); err != nil {
			t.Fatalf("set %v on %s: %v", path, name, err)
		}
		if err := cl.Update(ctx, obj); err != nil {
			t.Fatalf("update %s: %v", name, err)
		}
	}
	mutate(cnpgClusterGVK, "payments-db", 3, "spec", "instances")
	mutate(redisFailoverGVK, "payments-cache", 5, "spec", "redis", "replicas")
	mutate(rabbitmqClusterGVK, "payments-queue", 2, "spec", "replicas")

	items, err = p.DetectDrift(ctx, app)
	if err != nil {
		t.Fatalf("DetectDrift() error = %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 drift items, got %d: %+v", len(items), items)
	}

	byType := map[string]DriftItem{}
	for _, it := range items {
		byType[it.ResourceType] = it
	}
	for _, rt := range []string{"database", "cache", "queue"} {
		if _, ok := byType[rt]; !ok {
			t.Errorf("expected drift reported for %q, got items %+v", rt, items)
		}
	}
	if got := byType["database"]; got.DesiredValue != "1" || got.ActualValue != "3" {
		t.Errorf("database drift: want desired=1 actual=3, got desired=%s actual=%s", got.DesiredValue, got.ActualValue)
	}
}

// TestKubernetesProvider_DetectDrift_NoInfra verifies an Application with no
// infrastructure requests reports no drift and does not error.
func TestKubernetesProvider_DetectDrift_NoInfra(t *testing.T) {
	p, _, _ := buildKubernetesProvider(t, &fakeDiscovery{})
	app := createTestApplication("web-only", "default")

	items, err := p.DetectDrift(context.Background(), app)
	if err != nil {
		t.Fatalf("DetectDrift() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no drift for infra-less app, got %+v", items)
	}
}
