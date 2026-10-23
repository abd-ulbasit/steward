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

func TestCredentialSecrets_AreLabeledAndControlledByApplication(t *testing.T) {
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

// =============================================================================
// RABBITMQ DEFAULT USER
// =============================================================================

func rabbitOnlyDiscovery() *fakeDiscovery {
	return &fakeDiscovery{
		resources: map[string][]metav1.APIResource{
			rabbitmqClusterGVK.GroupVersion().String(): {
				{Name: "rabbitmqclusters", Kind: rabbitmqClusterGVK.Kind},
			},
		},
	}
}

func queueApplication(name string, uid types.UID) *platformv1alpha1.Application {
	app := createTestApplication(name, "default")
	app.UID = uid
	app.Spec.Queue = &platformv1alpha1.QueueSpec{Type: platformv1alpha1.QueueRabbitMQ}
	return app
}

func TestQueue_PasswordStaysOutOfRabbitmqClusterAndStatus(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, rabbitOnlyDiscovery())
	app := queueApplication("orders", "uid-orders")

	state, err := provider.Provision(ctx, app)
	if err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	secret := getSecretData(t, cl, "orders-queue-credentials")
	password := string(secret.Data["password"])
	if password == "" {
		t.Fatal("no password generated")
	}

	rabbit := &unstructured.Unstructured{}
	rabbit.SetGroupVersionKind(rabbitmqClusterGVK)
	if err := cl.Get(ctx, client.ObjectKey{Name: "orders-queue", Namespace: "default"}, rabbit); err != nil {
		t.Fatalf("get RabbitmqCluster: %v", err)
	}
	raw, err := rabbit.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal RabbitmqCluster: %v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Errorf("RabbitmqCluster contains the password: %s", raw)
	}
	ref, _, _ := unstructured.NestedString(rabbit.Object, "spec", "secretBackend", "externalSecret", "name")
	if ref != "orders-queue-credentials" {
		t.Errorf("spec.secretBackend.externalSecret.name = %q, want orders-queue-credentials", ref)
	}

	wantConf := "default_user = orders\ndefault_pass = " + password + "\n"
	if got := string(secret.Data["default_user.conf"]); got != wantConf {
		t.Errorf("default_user.conf does not match the stored credentials")
	}

	if state == nil || state.Queue == nil {
		t.Fatal("queue state missing")
	}
	if strings.Contains(state.Queue.URL, password) || strings.Contains(state.Queue.URL, "@") {
		t.Errorf("queue URL, which is copied into Application status, carries credentials: %q", state.Queue.URL)
	}
}

func TestQueue_KeepsLegacyPasswordWhenMovingToExternalSecret(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, rabbitOnlyDiscovery())
	app := queueApplication("billing", "uid-billing")

	// What an older release left behind: the password inlined into
	// additionalConfig and an unowned credentials Secret holding the same value.
	rabbit := &unstructured.Unstructured{}
	rabbit.SetGroupVersionKind(rabbitmqClusterGVK)
	rabbit.SetName("billing-queue")
	rabbit.SetNamespace("default")
	rabbit.SetOwnerReferences([]metav1.OwnerReference{controllerRefTo(app)})
	_ = unstructured.SetNestedField(rabbit.Object,
		"default_user = billing\ndefault_pass = legacy-amqp-password", "spec", "rabbitmq", "additionalConfig")
	if err := cl.Create(ctx, rabbit); err != nil {
		t.Fatalf("create RabbitmqCluster: %v", err)
	}
	if err := cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "billing-queue-credentials", Namespace: "default"},
		Data: map[string][]byte{
			"username": []byte("billing"),
			"password": []byte("legacy-amqp-password"),
		},
	}); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	if _, err := provider.Provision(ctx, app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	if err := cl.Get(ctx, client.ObjectKey{Name: "billing-queue", Namespace: "default"}, rabbit); err != nil {
		t.Fatalf("get RabbitmqCluster: %v", err)
	}
	if _, found, _ := unstructured.NestedString(rabbit.Object, "spec", "rabbitmq", "additionalConfig"); found {
		t.Errorf("additionalConfig with the password is still in the spec")
	}
	secret := getSecretData(t, cl, "billing-queue-credentials")
	if got := string(secret.Data["default_user.conf"]); got != "default_user = billing\ndefault_pass = legacy-amqp-password\n" {
		t.Errorf("default_user.conf does not carry the password the broker already uses")
	}
}

func TestQueue_RejectsLineBreaksInCredentials(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, rabbitOnlyDiscovery())
	app := queueApplication("inject", "uid-inject")

	if err := cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "inject-queue-credentials",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{controllerRefTo(app)},
		},
		Data: map[string][]byte{
			"username": []byte("inject"),
			"password": []byte("x\nloopback_users = none"),
		},
	}); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	_, err := provider.Provision(ctx, app)
	if !IsInvalidConfig(err) {
		t.Fatalf("Provision() error = %v, want InvalidConfigError", err)
	}
	if secret := getSecretData(t, cl, "inject-queue-credentials"); len(secret.Data["default_user.conf"]) != 0 {
		t.Errorf("default_user.conf was written from a value containing a line break")
	}
}

// =============================================================================
// POSTGRES DSN
// =============================================================================

func TestDatabase_ConnectionStringRequiresTLS(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, cnpgOnlyDiscovery())
	app := databaseApplication("tls", "uid-tls")

	if _, err := provider.Provision(ctx, app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	conn := string(getSecretData(t, cl, "tls-db-credentials").Data["connectionString"])
	if !strings.HasSuffix(conn, "?sslmode=require") {
		t.Errorf("connectionString = %q, want sslmode=require", conn)
	}
}

func TestDatabase_ExistingConnectionStringIsUpgradedToTLS(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, cnpgOnlyDiscovery())
	app := databaseApplication("old", "uid-old")

	if err := cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "old-db-credentials",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{controllerRefTo(app)},
		},
		Data: map[string][]byte{
			"username":         []byte("old"),
			"password":         []byte("kept-password"),
			"database":         []byte("old"),
			"connectionString": []byte("postgresql://old:kept-password@old-db-rw.default.svc:5432/old?sslmode=disable"),
		},
	}); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	if _, err := provider.Provision(ctx, app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	want := "postgresql://old:kept-password@old-db-rw.default.svc:5432/old?sslmode=require"
	if got := string(getSecretData(t, cl, "old-db-credentials").Data["connectionString"]); got != want {
		t.Errorf("connectionString = %q, want %q", got, want)
	}
}

func TestChangedKeys_NamesOnlyTheKeysThatDiffer(t *testing.T) {
	before := map[string][]byte{
		"password":         []byte("same"),
		"connectionString": []byte("postgresql://u:p@h:5432/d?sslmode=disable"),
		"stale":            []byte("x"),
	}
	after := map[string][]byte{
		"password":         []byte("same"),
		"connectionString": []byte("postgresql://u:p@h:5432/d?sslmode=require"),
		"host":             []byte("h"),
	}
	got := changedKeys(before, after)
	want := []string{"connectionString", "host", "stale"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("changedKeys() = %v, want %v", got, want)
	}
	if got := changedKeys(after, after); len(got) != 0 {
		t.Errorf("changedKeys() on identical data = %v, want none", got)
	}
}

func TestDatabase_SSLModeDisableIsAnExplicitOptOut(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, cnpgOnlyDiscovery())
	app := databaseApplication("node", "uid-node")
	app.Spec.Database.SSLMode = platformv1alpha1.PostgresSSLModeDisable

	if _, err := provider.Provision(ctx, app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	conn := string(getSecretData(t, cl, "node-db-credentials").Data["connectionString"])
	if !strings.HasSuffix(conn, "?sslmode=disable") {
		t.Errorf("connectionString = %q, want the opted-out sslmode=disable", conn)
	}
}

func TestDatabase_ApplicationSecretIsNotTheSuperuserSecret(t *testing.T) {
	ctx := context.Background()
	provider, cl, _ := buildKubernetesProvider(t, cnpgOnlyDiscovery())
	app := databaseApplication("roles", "uid-roles")

	if _, err := provider.Provision(ctx, app); err != nil && !IsNotReady(err) {
		t.Fatalf("Provision() error = %v", err)
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := cl.Get(ctx, client.ObjectKey{Name: "roles-db", Namespace: "default"}, cluster); err != nil {
		t.Fatalf("get CNPG Cluster: %v", err)
	}
	if _, found, _ := unstructured.NestedMap(cluster.Object, "spec", "superuserSecret"); found {
		t.Errorf("spec.superuserSecret is set; the application credentials must not double as superuser credentials")
	}
	enabled, found, _ := unstructured.NestedBool(cluster.Object, "spec", "enableSuperuserAccess")
	if !found || enabled {
		t.Errorf("spec.enableSuperuserAccess = %v (found=%v), want explicit false", enabled, found)
	}
	initdbSecret, _, _ := unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "secret", "name")
	if initdbSecret != "roles-db-credentials" {
		t.Errorf("bootstrap.initdb.secret.name = %q, want roles-db-credentials", initdbSecret)
	}
}
