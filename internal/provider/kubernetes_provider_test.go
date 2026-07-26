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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// =============================================================================
// FAKE DISCOVERY CLIENT
// =============================================================================

type fakeDiscovery struct {
	resources map[string][]metav1.APIResource
}

func (f *fakeDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	if resources, ok := f.resources[groupVersion]; ok {
		return &metav1.APIResourceList{
			GroupVersion: groupVersion,
			APIResources: resources,
		}, nil
	}
	return nil, fmt.Errorf("groupVersion %s not found", groupVersion)
}

// =============================================================================
// TEST HELPERS
// =============================================================================

func buildKubernetesProvider(t *testing.T, discovery *fakeDiscovery) (*KubernetesProvider, client.Client, *runtime.Scheme) {
	t.Helper()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = platformv1alpha1.AddToScheme(scheme)

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := &ProviderConfig{
		Provider: ProviderKubernetes,
		Kubernetes: &KubernetesConfig{
			PostgresOperator: "cnpg",
			RedisOperator:    "spotahome",
			QueueOperator:    "rabbitmq",
			StorageClass:     "standard",
		},
	}

	provider, err := NewKubernetesProvider(config, cl, scheme, discovery)
	if err != nil {
		t.Fatalf("NewKubernetesProvider() error = %v", err)
	}

	return provider, cl, scheme
}

func createInfraApplication(name string) *platformv1alpha1.Application {
	app := createTestApplication(name, "default")
	app.Spec.Database = &platformv1alpha1.DatabaseSpec{
		Type:    platformv1alpha1.DatabasePostgres,
		Size:    platformv1alpha1.SizeSmall,
		Version: "15",
	}
	app.Spec.Cache = &platformv1alpha1.CacheSpec{
		Type: platformv1alpha1.CacheRedis,
		Size: platformv1alpha1.SizeSmall,
	}
	app.Spec.Queue = &platformv1alpha1.QueueSpec{
		Type: platformv1alpha1.QueueRabbitMQ,
		FIFO: false,
	}
	app.Spec.Storage = &platformv1alpha1.StorageSpec{
		Type: platformv1alpha1.StorageS3,
	}
	return app
}

// =============================================================================
// TESTS
// =============================================================================

func TestKubernetesProvider_Provision_CreatesResources(t *testing.T) {
	discovery := &fakeDiscovery{
		resources: map[string][]metav1.APIResource{
			cnpgClusterGVK.GroupVersion().String(): {
				{Name: "clusters", Kind: cnpgClusterGVK.Kind},
			},
			redisFailoverGVK.GroupVersion().String(): {
				{Name: "redisfailovers", Kind: redisFailoverGVK.Kind},
			},
			rabbitmqClusterGVK.GroupVersion().String(): {
				{Name: "rabbitmqclusters", Kind: rabbitmqClusterGVK.Kind},
			},
		},
	}

	provider, cl, _ := buildKubernetesProvider(t, discovery)
	app := createInfraApplication("orders")

	state, err := provider.Provision(context.Background(), app)
	if err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}
	if state.Database == nil || state.Cache == nil || state.Queue == nil || state.Storage == nil {
		t.Fatal("expected all resource states to be populated")
	}

	// Verify Secrets exist
	for _, name := range []string{"orders-db-credentials", "orders-cache-credentials", "orders-queue-credentials"} {
		secret := &corev1.Secret{}
		if err := cl.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, secret); err != nil {
			t.Fatalf("expected secret %s to exist: %v", name, err)
		}
	}

	// Verify CRDs exist
	cnpg := &unstructured.Unstructured{}
	cnpg.SetGroupVersionKind(cnpgClusterGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "orders-db", Namespace: "default"}, cnpg); err != nil {
		t.Fatalf("expected CNPG Cluster to exist: %v", err)
	}

	redis := &unstructured.Unstructured{}
	redis.SetGroupVersionKind(redisFailoverGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "orders-cache", Namespace: "default"}, redis); err != nil {
		t.Fatalf("expected RedisFailover to exist: %v", err)
	}

	rabbit := &unstructured.Unstructured{}
	rabbit.SetGroupVersionKind(rabbitmqClusterGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "orders-queue", Namespace: "default"}, rabbit); err != nil {
		t.Fatalf("expected RabbitmqCluster to exist: %v", err)
	}

	// Verify PVC exists
	pvc := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "orders-storage", Namespace: "default"}, pvc); err != nil {
		t.Fatalf("expected PVC to exist: %v", err)
	}
}

func TestKubernetesProvider_Provision_MissingOperator(t *testing.T) {
	discovery := &fakeDiscovery{
		resources: map[string][]metav1.APIResource{
			cnpgClusterGVK.GroupVersion().String(): {
				{Name: "clusters", Kind: cnpgClusterGVK.Kind},
			},
			// Redis operator intentionally missing
			rabbitmqClusterGVK.GroupVersion().String(): {
				{Name: "rabbitmqclusters", Kind: rabbitmqClusterGVK.Kind},
			},
		},
	}

	provider, _, _ := buildKubernetesProvider(t, discovery)
	app := createInfraApplication("payments")

	_, err := provider.Provision(context.Background(), app)
	if err == nil {
		t.Fatal("expected error when Redis operator CRD is missing")
	}
	if !IsInvalidConfig(err) {
		t.Fatalf("expected InvalidConfigError, got %T", err)
	}
}

func TestKubernetesProvider_GetStatus_ReadyMapping(t *testing.T) {
	discovery := &fakeDiscovery{
		resources: map[string][]metav1.APIResource{
			cnpgClusterGVK.GroupVersion().String(): {
				{Name: "clusters", Kind: cnpgClusterGVK.Kind},
			},
			redisFailoverGVK.GroupVersion().String(): {
				{Name: "redisfailovers", Kind: redisFailoverGVK.Kind},
			},
			rabbitmqClusterGVK.GroupVersion().String(): {
				{Name: "rabbitmqclusters", Kind: rabbitmqClusterGVK.Kind},
			},
		},
	}

	provider, cl, _ := buildKubernetesProvider(t, discovery)
	app := createInfraApplication("billing")

	_, _ = provider.Provision(context.Background(), app)

	// Mark resources as Ready
	setReadyCondition := func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True"},
		}, "status", "conditions")
		_ = cl.Update(context.Background(), obj)
	}

	cnpg := &unstructured.Unstructured{}
	cnpg.SetGroupVersionKind(cnpgClusterGVK)
	_ = cl.Get(context.Background(), client.ObjectKey{Name: "billing-db", Namespace: "default"}, cnpg)
	setReadyCondition(cnpg)

	redis := &unstructured.Unstructured{}
	redis.SetGroupVersionKind(redisFailoverGVK)
	_ = cl.Get(context.Background(), client.ObjectKey{Name: "billing-cache", Namespace: "default"}, redis)
	setReadyCondition(redis)

	rabbit := &unstructured.Unstructured{}
	rabbit.SetGroupVersionKind(rabbitmqClusterGVK)
	_ = cl.Get(context.Background(), client.ObjectKey{Name: "billing-queue", Namespace: "default"}, rabbit)
	setReadyCondition(rabbit)

	pvc := &corev1.PersistentVolumeClaim{}
	_ = cl.Get(context.Background(), client.ObjectKey{Name: "billing-storage", Namespace: "default"}, pvc)
	pvc.Status.Phase = corev1.ClaimBound
	_ = cl.Status().Update(context.Background(), pvc)

	state, err := provider.GetStatus(context.Background(), app)
	if err != nil {
		t.Fatalf("GetStatus() error = %v", err)
	}

	if state.Database == nil || state.Database.Phase != ResourceReady {
		t.Fatalf("expected database ready, got %v", state.Database)
	}
	if state.Cache == nil || state.Cache.Phase != ResourceReady {
		t.Fatalf("expected cache ready, got %v", state.Cache)
	}
	if state.Queue == nil || state.Queue.Phase != ResourceReady {
		t.Fatalf("expected queue ready, got %v", state.Queue)
	}
	if state.Storage == nil || state.Storage.Phase != ResourceReady {
		t.Fatalf("expected storage ready, got %v", state.Storage)
	}
}

func TestKubernetesProvider_Destroy_Idempotent(t *testing.T) {
	discovery := &fakeDiscovery{
		resources: map[string][]metav1.APIResource{
			cnpgClusterGVK.GroupVersion().String(): {
				{Name: "clusters", Kind: cnpgClusterGVK.Kind},
			},
			redisFailoverGVK.GroupVersion().String(): {
				{Name: "redisfailovers", Kind: redisFailoverGVK.Kind},
			},
			rabbitmqClusterGVK.GroupVersion().String(): {
				{Name: "rabbitmqclusters", Kind: rabbitmqClusterGVK.Kind},
			},
		},
	}

	provider, _, _ := buildKubernetesProvider(t, discovery)
	app := createInfraApplication("analytics")

	_, _ = provider.Provision(context.Background(), app)

	if err := provider.Destroy(context.Background(), app); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}

	// Second destroy should be idempotent
	if err := provider.Destroy(context.Background(), app); err != nil {
		t.Fatalf("Destroy() second call error = %v", err)
	}
}
