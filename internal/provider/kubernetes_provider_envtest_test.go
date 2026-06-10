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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
)

// =============================================================================
// ENTEST: Kubernetes Provider CRD Creation
// =============================================================================
//
// WHY ENVTEST HERE:
//   Unit tests with fake client verify logic, but envtest validates that our
//   CRD objects can actually be created against a real API server.
//
// HOW IT WORKS:
//   - Start an in-process API server + etcd (envtest)
//   - Register minimal CRDs for CNPG/RedisFailover/RabbitmqCluster
//   - Run provider.Provision() and ensure it can create those CRs
//
// NOTE:
//   These CRDs are intentionally minimal (schema preserved) because the goal
//   is to validate API plumbing, not operator behavior.
// =============================================================================

func TestKubernetesProvider_Envtest_CRDProvisioning(t *testing.T) {
	ctx := context.Background()

	crds := []*apiextensionsv1.CustomResourceDefinition{
		buildTestCRD(cnpgClusterGVK.Group, cnpgClusterGVK.Version, "Cluster", "clusters"),
		buildTestCRD(redisFailoverGVK.Group, redisFailoverGVK.Version, "RedisFailover", "redisfailovers"),
		buildTestCRD(rabbitmqClusterGVK.Group, rabbitmqClusterGVK.Version, "RabbitmqCluster", "rabbitmqclusters"),
	}

	testEnv := &envtest.Environment{
		CRDs: crds,
		// Install the Application CRD so we can create Application objects
		// in the API server (needed for UID assignment → ownerReferences).
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}

	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("envtest start error: %v", err)
	}
	defer func() {
		_ = testEnv.Stop()
	}()

	cl, scheme := newEnvtestClient(t, cfg)
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatalf("discovery client error: %v", err)
	}

	provider, err := NewKubernetesProvider(&ProviderConfig{
		Provider: ProviderKubernetes,
		Kubernetes: &KubernetesConfig{
			PostgresOperator: "cnpg",
			RedisOperator:    "spotahome",
			QueueOperator:    "rabbitmq",
			StorageClass:     "standard",
		},
	}, cl, scheme, discoveryClient)
	if err != nil {
		t.Fatalf("NewKubernetesProvider error: %v", err)
	}

	app := createInfraApplication("envtest")

	// Create the Application in the API server so it gets a UID.
	// SetControllerReference (used by Provision) requires the owner to have a
	// non-empty UID, which is only assigned when an object is persisted.
	if err := cl.Create(ctx, app); err != nil {
		t.Fatalf("failed to create Application in envtest: %v", err)
	}
	// Re-fetch to get the server-assigned UID.
	if err := cl.Get(ctx, client.ObjectKey{Name: app.Name, Namespace: app.Namespace}, app); err != nil {
		t.Fatalf("failed to re-fetch Application: %v", err)
	}

	state, err := provider.Provision(ctx, app)
	if err != nil && !IsNotReady(err) {
		t.Fatalf("Provision error: %v", err)
	}
	if state.Database == nil || state.Cache == nil || state.Queue == nil || state.Storage == nil {
		t.Fatalf("expected all resource states to be populated")
	}

	// Sanity check: ensure Secrets were created in envtest cluster
	secret := &corev1.Secret{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "envtest-db-credentials", Namespace: "default"}, secret); err != nil {
		t.Fatalf("expected db secret to exist: %v", err)
	}

	// Give the API server a moment to process writes before teardown
	<-time.After(50 * time.Millisecond)
}

func newEnvtestClient(t *testing.T, cfg *rest.Config) (client.Client, *runtime.Scheme) {
	t.Helper()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = platformv1alpha1.AddToScheme(scheme)

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New error: %v", err)
	}
	return cl, scheme
}

func buildTestCRD(group, version, kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: plural + "." + group,
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   plural,
				Singular: strings.ToLower(kind),
				Kind:     kind,
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    version,
					Served:  true,
					Storage: true,
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:                   "object",
							XPreserveUnknownFields: ptr.To(true),
						},
					},
				},
			},
		},
	}
}

// getFirstFoundEnvTestBinaryDir locates the first envtest binary directory.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
