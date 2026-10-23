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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// =============================================================================
// CREDENTIAL SECRET OWNERSHIP
// =============================================================================

func cnpgOnlyDiscovery() *fakeDiscovery {
	return &fakeDiscovery{
		resources: map[string][]metav1.APIResource{
			cnpgClusterGVK.GroupVersion().String(): {
				{Name: "clusters", Kind: cnpgClusterGVK.Kind},
			},
		},
	}
}

func databaseApplication(name string, uid types.UID) *platformv1alpha1.Application {
	app := createTestApplication(name, "default")
	app.UID = uid
	app.Spec.Database = &platformv1alpha1.DatabaseSpec{
		Type:    platformv1alpha1.DatabasePostgres,
		Size:    platformv1alpha1.SizeSmall,
		Version: "16",
	}
	return app
}

func controllerRefTo(app *platformv1alpha1.Application) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         platformv1alpha1.GroupVersion.String(),
		Kind:               "Application",
		Name:               app.Name,
		UID:                app.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

func getSecretData(t *testing.T, cl client.Client, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, secret); err != nil {
		t.Fatalf("get secret %s: %v", name, err)
	}
	return secret
}

func TestCredentialSecrets_AreLabelledAndControlledByApplication(t *testing.T) {
	provider, cl, _ := buildKubernetesProvider(t, &fakeDiscovery{
		resources: map[string][]metav1.APIResource{
			cnpgClusterGVK.GroupVersion().String():     {{Name: "clusters", Kind: cnpgClusterGVK.Kind}},
			redisFailoverGVK.GroupVersion().String():   {{Name: "redisfailovers", Kind: redisFailoverGVK.Kind}},
			rabbitmqClusterGVK.GroupVersion().String(): {{Name: "rabbitmqclusters", Kind: rabbitmqClusterGVK.Kind}},
		},
	})
	app := createInfraApplication("labels")
	app.UID = "uid-labels"

	if _, err := provider.Provision(context.Background(), app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	for _, name := range []string{"labels-db-credentials", "labels-cache-credentials", "labels-queue-credentials"} {
		secret := getSecretData(t, cl, name)
		if got := secret.Labels["app.kubernetes.io/managed-by"]; got != "steward" {
			t.Errorf("%s: managed-by label = %q, want steward", name, got)
		}
		if got := secret.Labels["app.kubernetes.io/instance"]; got != "labels" {
			t.Errorf("%s: instance label = %q, want labels", name, got)
		}
		if !metav1.IsControlledBy(secret, app) {
			t.Errorf("%s: not controlled by the Application; ownerReferences = %v", name, secret.OwnerReferences)
		}
		if len(secret.Data["password"]) == 0 {
			t.Errorf("%s: empty password", name)
		}
	}
}

func TestCredentialSecrets_AdoptsOrphanFromOlderRelease(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, cnpgOnlyDiscovery())
	app := databaseApplication("legacy", "uid-legacy")

	// State left by a release that did not label or own credential Secrets:
	// the CNPG Cluster is controlled by the Application, the Secret is not.
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName("legacy-db")
	cluster.SetNamespace("default")
	cluster.SetOwnerReferences([]metav1.OwnerReference{controllerRefTo(app)})
	if err := cl.Create(ctx, cluster); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-db-credentials", Namespace: "default"},
		Data: map[string][]byte{
			"username":         []byte("legacy"),
			"password":         []byte("legacy-password-in-use"),
			"database":         []byte("legacy"),
			"connectionString": []byte("postgresql://stale"),
		},
	}
	if err := cl.Create(ctx, legacy); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	if _, err := provider.Provision(ctx, app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	secret := getSecretData(t, cl, "legacy-db-credentials")
	if got := string(secret.Data["password"]); got != "legacy-password-in-use" {
		t.Fatalf("password was replaced; the running database still uses the old one")
	}
	if !metav1.IsControlledBy(secret, app) {
		t.Errorf("adopted Secret is not controlled by the Application")
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "steward" {
		t.Errorf("adopted Secret is missing the managed-by label")
	}
	if conn := string(secret.Data["connectionString"]); !strings.Contains(conn, "legacy-password-in-use@legacy-db-rw.default.svc:5432/legacy") {
		t.Errorf("connectionString was not re-derived from the stored password: %q", conn)
	}
}

func TestCredentialSecrets_RefusesUnknownPreexistingSecret(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, cnpgOnlyDiscovery())
	app := databaseApplication("planted", "uid-planted")

	// A Secret that exists before steward ever provisioned anything for this
	// Application. Taking it over would let its author choose the password.
	planted := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "planted-db-credentials", Namespace: "default"},
		Data: map[string][]byte{
			"username": []byte("planted"),
			"password": []byte("chosen-by-someone-else"),
		},
	}
	if err := cl.Create(ctx, planted); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	_, err := provider.Provision(ctx, app)
	if err == nil || IsNotReady(err) {
		t.Fatalf("Provision() error = %v, want a refusal", err)
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if getErr := cl.Get(ctx, client.ObjectKey{Name: "planted-db", Namespace: "default"}, cluster); getErr == nil {
		t.Errorf("CNPG Cluster was created with the planted credentials")
	}
	if secret := getSecretData(t, cl, "planted-db-credentials"); len(secret.OwnerReferences) != 0 {
		t.Errorf("planted Secret was adopted: %v", secret.OwnerReferences)
	}
}

func TestCredentialSecrets_RefusesSecretControlledByAnotherObject(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, cnpgOnlyDiscovery())
	app := databaseApplication("taken", "uid-taken")

	other := databaseApplication("someone-else", "uid-other")
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "taken-db-credentials",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{controllerRefTo(other)},
		},
		Data: map[string][]byte{"password": []byte("not-ours")},
	}
	if err := cl.Create(ctx, foreign); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	if _, err := provider.Provision(ctx, app); err == nil || IsNotReady(err) {
		t.Fatalf("Provision() error = %v, want a refusal", err)
	}
}

func TestCredentialSecrets_FillsMissingPassword(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, cnpgOnlyDiscovery())
	app := databaseApplication("nopass", "uid-nopass")

	// Owned by the Application but the password key was removed by hand.
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "nopass-db-credentials",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{controllerRefTo(app)},
		},
		Data: map[string][]byte{"username": []byte("nopass"), "database": []byte("nopass")},
	}
	if err := cl.Create(ctx, broken); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	if _, err := provider.Provision(ctx, app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	secret := getSecretData(t, cl, "nopass-db-credentials")
	password := string(secret.Data["password"])
	if password == "" {
		t.Fatal("password is still empty")
	}
	if conn := string(secret.Data["connectionString"]); !strings.Contains(conn, ":"+password+"@") {
		t.Errorf("connectionString does not carry the generated password: %q", conn)
	}
}
