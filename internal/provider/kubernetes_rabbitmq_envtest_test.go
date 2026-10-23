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
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// =============================================================================
// ENVTEST: RabbitMQ Cluster Operator too old for spec.secretBackend
// =============================================================================
//
// A fake client keeps every field it is given. Only a real API server with a
// structural CRD shows the pruning an operator older than v2.1.0 causes, so
// this test installs a RabbitmqCluster CRD whose spec schema has no
// secretBackend, the way those releases shipped it.
// =============================================================================

func TestEnvtest_RabbitmqOperatorWithoutSecretBackendIsRejected(t *testing.T) {
	ctx := context.Background()

	legacyRabbitCRD := buildTestCRD(rabbitmqClusterGVK.Group, rabbitmqClusterGVK.Version, "RabbitmqCluster", "rabbitmqclusters")
	legacyRabbitCRD.Spec.Versions[0].Schema.OpenAPIV3Schema = &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"spec": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"replicas":    {Type: "integer"},
					"persistence": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
					"rabbitmq":    {Type: "object", XPreserveUnknownFields: ptr.To(true)},
				},
			},
			"status": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
		},
	}

	testEnv := &envtest.Environment{
		CRDs:              []*apiextensionsv1.CustomResourceDefinition{legacyRabbitCRD},
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("envtest start error: %v", err)
	}
	defer func() { _ = testEnv.Stop() }()

	cl, scheme := newEnvtestClient(t, cfg)
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatalf("discovery client: %v", err)
	}
	p, err := NewKubernetesProvider(&ProviderConfig{
		Provider:   ProviderKubernetes,
		Kubernetes: &KubernetesConfig{QueueOperator: "rabbitmq"},
	}, cl, scheme, disco)
	if err != nil {
		t.Fatalf("NewKubernetesProvider: %v", err)
	}

	app := queueApplication("oldrabbit", "")
	if err := cl.Create(ctx, app); err != nil {
		t.Fatalf("create Application: %v", err)
	}

	rabbitKey := client.ObjectKey{Name: "oldrabbit-queue", Namespace: "default"}
	getRabbit := func() (*unstructured.Unstructured, error) {
		rabbit := &unstructured.Unstructured{}
		rabbit.SetGroupVersionKind(rabbitmqClusterGVK)
		return rabbit, cl.Get(ctx, rabbitKey, rabbit)
	}

	// New queue: the dry run shows the field would be pruned, so nothing is
	// created and no broker boots with credentials steward does not know.
	_, err = p.Provision(ctx, app)
	if !IsInvalidConfig(err) || !strings.Contains(err.Error(), "v2.1.0") {
		t.Fatalf("Provision() error = %v, want InvalidConfigError naming v2.1.0", err)
	}
	if _, err := getRabbit(); !apierrors.IsNotFound(err) {
		t.Fatalf("RabbitmqCluster was created despite the pruned secretBackend (get err = %v)", err)
	}

	// Existing queue (left by an older steward release): the read-back after
	// the update catches it.
	legacy := &unstructured.Unstructured{}
	legacy.SetGroupVersionKind(rabbitmqClusterGVK)
	legacy.SetName(rabbitKey.Name)
	legacy.SetNamespace(rabbitKey.Namespace)
	legacy.SetOwnerReferences([]metav1.OwnerReference{controllerRefTo(app)})
	_ = unstructured.SetNestedField(legacy.Object, int64(1), "spec", "replicas")
	if err := cl.Create(ctx, legacy); err != nil {
		t.Fatalf("create legacy RabbitmqCluster: %v", err)
	}

	_, err = p.Provision(ctx, app)
	if !IsInvalidConfig(err) {
		t.Fatalf("Provision() on an existing cluster: error = %v, want InvalidConfigError", err)
	}
	rabbit, err := getRabbit()
	if err != nil {
		t.Fatalf("get RabbitmqCluster: %v", err)
	}
	if _, found, _ := unstructured.NestedMap(rabbit.Object, "spec", "secretBackend"); found {
		t.Fatalf("test CRD did not prune spec.secretBackend; the test is not exercising the old operator")
	}
}
