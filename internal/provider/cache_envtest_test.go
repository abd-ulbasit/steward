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
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// =============================================================================
// ENVTEST: label-scoped Secret cache
// =============================================================================
//
// These tests run the real informer cache built from CacheOptions/ClientOptions
// (the configuration cmd/main.go hands to the manager) against an API server,
// because a fake client cannot show what an informer with a label selector
// does and does not see.
// =============================================================================

func TestEnvtest_SecretCacheHoldsOnlyStewardSecrets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg, stop := startCacheEnvtest(t)
	// Stop the informers before the API server, or Stop waits for their
	// watches to time out.
	defer func() { cancel(); stop() }()
	direct, _ := newEnvtestClient(t, cfg)
	cached, _ := newManagerStyleClient(ctx, t, cfg, direct)

	managed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "managed", Namespace: "default",
		Labels: map[string]string{ManagedByLabel: ManagedByValue},
	}}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-tls", Namespace: "default"}}
	for _, s := range []*corev1.Secret{managed, foreign} {
		if err := direct.Create(ctx, s); err != nil {
			t.Fatalf("create %s: %v", s.Name, err)
		}
	}

	waitForCached(ctx, t, cached, "managed")

	err := cached.Get(ctx, client.ObjectKey{Name: "someone-elses-tls", Namespace: "default"}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("unlabelled Secret is visible through the cache (err = %v)", err)
	}
}

func TestEnvtest_LegacyCredentialSecretIsAdoptedDespiteCacheScope(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg, stop := startCacheEnvtest(t)
	// Stop the informers before the API server, or Stop waits for their
	// watches to time out.
	defer func() { cancel(); stop() }()
	direct, scheme := newEnvtestClient(t, cfg)
	cached, _ := newManagerStyleClient(ctx, t, cfg, direct)
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatalf("discovery client: %v", err)
	}

	app := databaseApplication("legacy", "")
	if err := direct.Create(ctx, app); err != nil {
		t.Fatalf("create Application: %v", err)
	}

	// An upgrade from a release that did not label credential Secrets: the
	// CNPG Cluster is controlled by the Application, the Secret is bare and
	// so outside the cache's label selector.
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName("legacy-db")
	cluster.SetNamespace("default")
	cluster.SetOwnerReferences([]metav1.OwnerReference{controllerRefTo(app)})
	if err := direct.Create(ctx, cluster); err != nil {
		t.Fatalf("create CNPG Cluster: %v", err)
	}
	if err := direct.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-db-credentials", Namespace: "default"},
		Data: map[string][]byte{
			"username": []byte("legacy"),
			"password": []byte("password-the-database-uses"),
			"database": []byte("legacy"),
		},
	}); err != nil {
		t.Fatalf("create Secret: %v", err)
	}

	newProvider := func() *KubernetesProvider {
		p, err := NewKubernetesProvider(&ProviderConfig{
			Provider:   ProviderKubernetes,
			Kubernetes: &KubernetesConfig{},
		}, cached, scheme, disco)
		if err != nil {
			t.Fatalf("NewKubernetesProvider: %v", err)
		}
		return p
	}

	// Without the uncached reader the Secret looks absent, Create collides
	// with it, and the database never progresses. This is the failure the
	// fallback exists for.
	if _, err := newProvider().Provision(ctx, app); !IsNotReady(err) {
		t.Fatalf("Provision() without uncached reader: err = %v, want NotReady", err)
	}

	if _, err := newProvider().WithUncachedReader(direct).Provision(ctx, app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	secret := &corev1.Secret{}
	if err := direct.Get(ctx, client.ObjectKey{Name: "legacy-db-credentials", Namespace: "default"}, secret); err != nil {
		t.Fatalf("get Secret: %v", err)
	}
	if string(secret.Data["password"]) != "password-the-database-uses" {
		t.Fatalf("password was replaced during adoption")
	}
	if !metav1.IsControlledBy(secret, app) || secret.Labels[ManagedByLabel] != ManagedByValue {
		t.Fatalf("Secret was not labelled and owned: labels=%v owners=%v", secret.Labels, secret.OwnerReferences)
	}

	// Labelled now, so the next reconcile is served from the cache.
	waitForCached(ctx, t, cached, "legacy-db-credentials")
}

// startCacheEnvtest starts an API server with the Application CRD and a
// minimal CNPG Cluster CRD.
func startCacheEnvtest(t *testing.T) (*rest.Config, func()) {
	t.Helper()
	testEnv := &envtest.Environment{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			buildTestCRD(cnpgClusterGVK.Group, cnpgClusterGVK.Version, "Cluster", "clusters"),
		},
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("envtest start error: %v", err)
	}
	return cfg, func() { _ = testEnv.Stop() }
}

// newManagerStyleClient builds what ctrl.NewManager builds from CacheOptions
// and ClientOptions: an informer cache plus a client that reads through it.
func newManagerStyleClient(ctx context.Context, t *testing.T, cfg *rest.Config, direct client.Client) (client.Client, cache.Cache) {
	t.Helper()

	cacheOpts := CacheOptions()
	cacheOpts.Scheme = direct.Scheme()
	informers, err := cache.New(cfg, cacheOpts)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	go func() { _ = informers.Start(ctx) }()
	if !informers.WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	clientOpts := ClientOptions()
	clientOpts.Scheme = direct.Scheme()
	clientOpts.Cache.Reader = informers
	cached, err := client.New(cfg, clientOpts)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return cached, informers
}

func waitForCached(ctx context.Context, t *testing.T, cached client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := cached.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, &corev1.Secret{})
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Secret %s never appeared in the cache: %v", name, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
