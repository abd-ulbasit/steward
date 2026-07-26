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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// =============================================================================
// TEST HELPERS
// =============================================================================

// createTestApplication creates a minimal Application for testing.
func createTestApplication(name, namespace string) *platformv1alpha1.Application {
	return &platformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: platformv1alpha1.ApplicationSpec{
			Team:  "test-team",
			Owner: "test@example.com",
		},
	}
}

// createTestApplicationWithInfra creates an Application with infrastructure.
func createTestApplicationWithInfra(name, namespace string) *platformv1alpha1.Application {
	app := createTestApplication(name, namespace)
	size := platformv1alpha1.SizeMedium
	app.Spec.Database = &platformv1alpha1.DatabaseSpec{
		Type:    platformv1alpha1.DatabasePostgres,
		Size:    size,
		Version: "15",
	}
	app.Spec.Cache = &platformv1alpha1.CacheSpec{
		Type: platformv1alpha1.CacheRedis,
		Size: size,
	}
	return app
}

// =============================================================================
// MOCK PROVIDER TESTS
// =============================================================================

func TestMockProvider_Name(t *testing.T) {
	provider := NewMockProvider(nil)
	if got := provider.Name(); got != "mock" {
		t.Errorf("Name() = %v, want %v", got, "mock")
	}
}

func TestMockProvider_Type(t *testing.T) {
	provider := NewMockProvider(nil)
	if got := provider.Type(); got != ProviderMock {
		t.Errorf("Type() = %v, want %v", got, ProviderMock)
	}
}

func TestMockProvider_Healthy(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider(nil)

	// Should be healthy by default
	if !provider.Healthy(ctx) {
		t.Error("Healthy() = false, want true by default")
	}

	// Can be set to unhealthy
	provider.IsHealthy = false
	if provider.Healthy(ctx) {
		t.Error("Healthy() = true after setting IsHealthy=false")
	}
}

func TestMockProvider_Provision_NoInfra(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider(nil)
	// Application with no infrastructure requested
	app := createTestApplication("test-app", "default")

	state, err := provider.Provision(ctx, app)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if state == nil {
		t.Fatal("Provision() returned nil state")
	}

	// Should have no resources
	if state.Database != nil {
		t.Error("state.Database should be nil when not requested")
	}
	if state.Cache != nil {
		t.Error("state.Cache should be nil when not requested")
	}
}

func TestMockProvider_Provision_WithDatabase(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider(nil)
	app := createTestApplicationWithInfra("test-app", "default")

	// Provision
	state, err := provider.Provision(ctx, app)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if state == nil {
		t.Fatal("Provision() returned nil state")
	}

	// With no delay, should be Ready immediately
	if state.Database == nil {
		t.Fatal("state.Database should not be nil")
	}
	if state.Database.Phase != ResourceReady {
		t.Errorf("Database.Phase = %v, want %v", state.Database.Phase, ResourceReady)
	}
	if state.Database.Endpoint == "" {
		t.Error("Database.Endpoint should not be empty")
	}
	if state.Database.Port != 5432 {
		t.Errorf("Database.Port = %v, want 5432", state.Database.Port)
	}
	if state.Database.SecretRef == nil {
		t.Error("Database.SecretRef should not be nil")
	}
}

func TestMockProvider_Provision_WithDelay(t *testing.T) {
	ctx := context.Background()
	config := &ProviderConfig{
		Provider: ProviderMock,
		Local: &LocalConfig{
			MockDelay: 100 * time.Millisecond,
		},
	}
	provider := NewMockProvider(config)
	app := createTestApplicationWithInfra("test-app", "default")

	// First provision - should be Provisioning
	state, err := provider.Provision(ctx, app)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if state.Database.Phase != ResourceProvisioning {
		t.Errorf("Database.Phase = %v, want %v", state.Database.Phase, ResourceProvisioning)
	}

	// Immediately after - should still be Provisioning
	state, err = provider.Provision(ctx, app)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if state.Database.Phase != ResourceProvisioning {
		t.Errorf("Database.Phase = %v, want %v", state.Database.Phase, ResourceProvisioning)
	}

	// Wait for delay
	time.Sleep(150 * time.Millisecond)

	// After delay - should be Ready
	state, err = provider.Provision(ctx, app)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if state.Database.Phase != ResourceReady {
		t.Errorf("Database.Phase = %v, want %v", state.Database.Phase, ResourceReady)
	}
}

func TestMockProvider_Provision_Error(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider(nil)
	app := createTestApplicationWithInfra("test-app", "default")

	// Configure error
	expectedErr := &QuotaExceededError{
		ResourceType: "database",
		QuotaName:    "max-instances",
		CurrentUsage: 20,
		Limit:        20,
		Message:      "maximum number of database instances exceeded",
	}
	provider.ProvisionError = expectedErr

	// Should return configured error
	_, err := provider.Provision(ctx, app)
	if err == nil {
		t.Fatal("Provision() should return error")
	}
	if !IsQuotaExceeded(err) {
		t.Errorf("Provision() error should be QuotaExceededError, got %T", err)
	}
}

func TestMockProvider_GetStatus(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider(nil)
	app := createTestApplicationWithInfra("test-app", "default")

	// GetStatus before provisioning - should return NotFoundError
	_, err := provider.GetStatus(ctx, app)
	if err == nil {
		t.Fatal("GetStatus() should return error before provisioning")
	}
	if !IsNotFound(err) {
		t.Errorf("GetStatus() error should be NotFoundError, got %T", err)
	}

	// Provision first
	_, err = provider.Provision(ctx, app)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	// Now GetStatus should work
	state, err := provider.GetStatus(ctx, app)
	if err != nil {
		t.Fatalf("GetStatus() error = %v", err)
	}
	if state == nil {
		t.Fatal("GetStatus() returned nil state")
	}
	if state.Database == nil {
		t.Error("state.Database should not be nil")
	}
}

func TestMockProvider_Destroy(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider(nil)
	app := createTestApplicationWithInfra("test-app", "default")

	// Provision first
	_, err := provider.Provision(ctx, app)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	// Verify provisioned
	if !provider.HasBeenProvisioned(app) {
		t.Error("HasBeenProvisioned() = false after Provision()")
	}

	// Destroy
	err = provider.Destroy(ctx, app)
	if err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}

	// Verify destroyed
	if provider.HasBeenProvisioned(app) {
		t.Error("HasBeenProvisioned() = true after Destroy()")
	}

	// GetStatus should fail now
	_, err = provider.GetStatus(ctx, app)
	if !IsNotFound(err) {
		t.Errorf("GetStatus() after Destroy() should be NotFoundError, got %T", err)
	}
}

func TestMockProvider_Destroy_Idempotent(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider(nil)
	app := createTestApplicationWithInfra("test-app", "default")

	// Destroy without provisioning - should succeed (idempotent)
	err := provider.Destroy(ctx, app)
	if err != nil {
		t.Errorf("Destroy() on non-existent app should succeed, got error = %v", err)
	}
}

func TestMockProvider_InvocationTracking(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider(nil)
	app := createTestApplicationWithInfra("test-app", "default")

	// Initial state
	if provider.ProvisionCallCount() != 0 {
		t.Errorf("ProvisionCallCount() = %v, want 0", provider.ProvisionCallCount())
	}

	// Provision twice
	_, _ = provider.Provision(ctx, app)
	_, _ = provider.Provision(ctx, app)
	if provider.ProvisionCallCount() != 2 {
		t.Errorf("ProvisionCallCount() = %v, want 2", provider.ProvisionCallCount())
	}

	// GetStatus
	_, _ = provider.GetStatus(ctx, app)
	if provider.GetStatusCallCount() != 1 {
		t.Errorf("GetStatusCallCount() = %v, want 1", provider.GetStatusCallCount())
	}

	// Destroy
	_ = provider.Destroy(ctx, app)
	if provider.DestroyCallCount() != 1 {
		t.Errorf("DestroyCallCount() = %v, want 1", provider.DestroyCallCount())
	}

	// Reset clears all
	provider.Reset()
	if provider.ProvisionCallCount() != 0 {
		t.Errorf("ProvisionCallCount() after Reset() = %v, want 0", provider.ProvisionCallCount())
	}
}

func TestMockProvider_ResourceIsReady(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider(nil)
	app := createTestApplicationWithInfra("test-app", "default")

	state, err := provider.Provision(ctx, app)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if !state.IsReady() {
		t.Error("IsReady() = false, want true")
	}
	if state.HasFailures() {
		t.Error("HasFailures() = true, want false")
	}
	if state.IsProvisioning() {
		t.Error("IsProvisioning() = true, want false")
	}
}

// =============================================================================
// ERROR TYPE TESTS
// =============================================================================

func TestNotReadyError(t *testing.T) {
	err := &NotReadyError{
		ResourceType: "database",
		ResourceID:   "test-db",
		CurrentPhase: ResourceProvisioning,
		Message:      "still creating",
	}

	if err.ErrorCode() != "NotReady" {
		t.Errorf("ErrorCode() = %v, want NotReady", err.ErrorCode())
	}
	if !err.IsRetryable() {
		t.Error("IsRetryable() = false, want true")
	}
	if err.Resource() != "database" {
		t.Errorf("Resource() = %v, want database", err.Resource())
	}
	if !IsNotReady(err) {
		t.Error("IsNotReady() = false, want true")
	}
	if IsQuotaExceeded(err) {
		t.Error("IsQuotaExceeded() = true for NotReadyError")
	}
}

func TestNotFoundError(t *testing.T) {
	err := &NotFoundError{
		ResourceType: "database",
		ResourceID:   "test-db",
		Message:      "does not exist",
	}

	if err.ErrorCode() != "NotFound" {
		t.Errorf("ErrorCode() = %v, want NotFound", err.ErrorCode())
	}
	if !IsNotFound(err) {
		t.Error("IsNotFound() = false, want true")
	}
}

func TestQuotaExceededError(t *testing.T) {
	err := &QuotaExceededError{
		ResourceType: "database",
		QuotaName:    "max-instances",
		CurrentUsage: 20,
		Limit:        20,
		Message:      "limit reached",
	}

	if err.ErrorCode() != "QuotaExceeded" {
		t.Errorf("ErrorCode() = %v, want QuotaExceeded", err.ErrorCode())
	}
	if err.IsRetryable() {
		t.Error("IsRetryable() = true, want false")
	}
	if !IsQuotaExceeded(err) {
		t.Error("IsQuotaExceeded() = false, want true")
	}
}

func TestInvalidConfigError(t *testing.T) {
	err := &InvalidConfigError{
		ResourceType: "database",
		Field:        "version",
		Value:        "999",
		Message:      "unsupported PostgreSQL version",
	}

	if err.ErrorCode() != "InvalidConfig" {
		t.Errorf("ErrorCode() = %v, want InvalidConfig", err.ErrorCode())
	}
	if err.IsRetryable() {
		t.Error("IsRetryable() = true, want false for config errors")
	}
	if !IsInvalidConfig(err) {
		t.Error("IsInvalidConfig() = false, want true")
	}
}

func TestProvisioningError_WithCause(t *testing.T) {
	cause := fmt.Errorf("terraform apply failed")
	err := &ProvisioningError{
		ResourceType: "database",
		ResourceID:   "test-db",
		Operation:    "create",
		Cause:        cause,
		Message:      "failed to create database",
	}

	if err.ErrorCode() != "ProvisioningFailed" {
		t.Errorf("ErrorCode() = %v, want ProvisioningFailed", err.ErrorCode())
	}
	if !err.IsRetryable() {
		t.Error("IsRetryable() = false, want true")
	}
	if err.Unwrap() != cause {
		t.Error("Unwrap() should return cause")
	}
	if !IsProvisioningError(err) {
		t.Error("IsProvisioningError() = false, want true")
	}
}

func TestTimeoutError(t *testing.T) {
	err := &TimeoutError{
		ResourceType: "database",
		ResourceID:   "test-db",
		Operation:    "create",
		Duration:     "30m",
		Message:      "operation timed out",
	}

	if err.ErrorCode() != "Timeout" {
		t.Errorf("ErrorCode() = %v, want Timeout", err.ErrorCode())
	}
	if !err.IsRetryable() {
		t.Error("IsRetryable() = false, want true")
	}
	if !IsTimeout(err) {
		t.Error("IsTimeout() = false, want true")
	}
}

func TestAuthenticationError(t *testing.T) {
	err := &AuthenticationError{
		Provider: "aws",
		Message:  "invalid credentials",
		Cause:    fmt.Errorf("access denied"),
	}

	if err.ErrorCode() != "AuthenticationFailed" {
		t.Errorf("ErrorCode() = %v, want AuthenticationFailed", err.ErrorCode())
	}
	if err.IsRetryable() {
		t.Error("IsRetryable() = true, want false")
	}
	if !IsAuthenticationError(err) {
		t.Error("IsAuthenticationError() = false, want true")
	}
}

func TestProviderNotConfiguredError(t *testing.T) {
	err := &ProviderNotConfiguredError{
		Provider: ProviderAWS,
		Message:  "AWS provider not configured",
	}

	if err.ErrorCode() != "ProviderNotConfigured" {
		t.Errorf("ErrorCode() = %v, want ProviderNotConfigured", err.ErrorCode())
	}
	if !IsProviderNotConfigured(err) {
		t.Error("IsProviderNotConfigured() = false, want true")
	}
}

func TestGetErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"NotReadyError", &NotReadyError{}, "NotReady"},
		{"NotFoundError", &NotFoundError{}, "NotFound"},
		{"QuotaExceededError", &QuotaExceededError{}, "QuotaExceeded"},
		{"Regular error", fmt.Errorf("some error"), ""},
		{"Nil error", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetErrorCode(tt.err); got != tt.want {
				t.Errorf("GetErrorCode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"NotReadyError", &NotReadyError{}, true},
		{"NotFoundError", &NotFoundError{}, true},
		{"QuotaExceededError", &QuotaExceededError{}, false},
		{"InvalidConfigError", &InvalidConfigError{}, false},
		{"ProvisioningError", &ProvisioningError{}, true},
		{"TimeoutError", &TimeoutError{}, true},
		{"AuthenticationError", &AuthenticationError{}, false},
		{"Regular error", fmt.Errorf("some error"), true}, // Unknown = retryable
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryable(tt.err); got != tt.want {
				t.Errorf("IsRetryable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// =============================================================================
// RESOURCE STATE TESTS
// =============================================================================

func TestResourceState_IsReady(t *testing.T) {
	tests := []struct {
		name  string
		state ResourceState
		want  bool
	}{
		{
			name:  "empty state is ready",
			state: ResourceState{},
			want:  true,
		},
		{
			name: "all ready",
			state: ResourceState{
				Database: &DatabaseState{Phase: ResourceReady},
				Cache:    &CacheState{Phase: ResourceReady},
			},
			want: true,
		},
		{
			name: "database provisioning",
			state: ResourceState{
				Database: &DatabaseState{Phase: ResourceProvisioning},
				Cache:    &CacheState{Phase: ResourceReady},
			},
			want: false,
		},
		{
			name: "cache failed",
			state: ResourceState{
				Database: &DatabaseState{Phase: ResourceReady},
				Cache:    &CacheState{Phase: ResourceFailed},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.IsReady(); got != tt.want {
				t.Errorf("IsReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResourceState_HasFailures(t *testing.T) {
	tests := []struct {
		name  string
		state ResourceState
		want  bool
	}{
		{
			name:  "empty state no failures",
			state: ResourceState{},
			want:  false,
		},
		{
			name: "database failed",
			state: ResourceState{
				Database: &DatabaseState{Phase: ResourceFailed},
			},
			want: true,
		},
		{
			name: "queue failed",
			state: ResourceState{
				Queue: &QueueState{Phase: ResourceFailed},
			},
			want: true,
		},
		{
			name: "all ready no failures",
			state: ResourceState{
				Database: &DatabaseState{Phase: ResourceReady},
				Cache:    &CacheState{Phase: ResourceReady},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.HasFailures(); got != tt.want {
				t.Errorf("HasFailures() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResourceState_IsProvisioning(t *testing.T) {
	tests := []struct {
		name  string
		state ResourceState
		want  bool
	}{
		{
			name:  "empty state not provisioning",
			state: ResourceState{},
			want:  false,
		},
		{
			name: "database provisioning",
			state: ResourceState{
				Database: &DatabaseState{Phase: ResourceProvisioning},
			},
			want: true,
		},
		{
			name: "all ready not provisioning",
			state: ResourceState{
				Database: &DatabaseState{Phase: ResourceReady},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.IsProvisioning(); got != tt.want {
				t.Errorf("IsProvisioning() = %v, want %v", got, tt.want)
			}
		})
	}
}

// =============================================================================
// FACTORY TESTS
// =============================================================================

func TestFactory_GetProvider_Mock(t *testing.T) {
	factory := NewFactory()
	factory.SetConfig(&ProviderConfig{
		Provider: ProviderMock,
		Local:    &LocalConfig{UseMocks: true},
	})

	provider, err := factory.GetProvider()
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}
	if provider == nil {
		t.Fatal("GetProvider() returned nil")
	}
	if provider.Name() != "mock" {
		t.Errorf("provider.Name() = %v, want mock", provider.Name())
	}
}

func TestFactory_GetProvider_Cached(t *testing.T) {
	factory := NewFactory()
	factory.SetConfig(&ProviderConfig{
		Provider: ProviderMock,
	})

	// First call
	p1, err := factory.GetProvider()
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}

	// Second call - should return same instance
	p2, err := factory.GetProvider()
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}

	if p1 != p2 {
		t.Error("GetProvider() should return cached instance")
	}
}

func TestFactory_SetProvider(t *testing.T) {
	factory := NewFactory()
	mockProvider := NewMockProvider(nil)

	factory.SetProvider(mockProvider)

	provider, err := factory.GetProvider()
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}
	if provider != mockProvider {
		t.Error("GetProvider() should return the set provider")
	}
}

func TestFactory_Reset(t *testing.T) {
	factory := NewFactory()
	factory.SetConfig(&ProviderConfig{
		Provider: ProviderMock,
	})

	// Get provider to cache it
	_, _ = factory.GetProvider()

	// Reset
	factory.Reset()

	// Should create new instance after reset
	factory.SetConfig(&ProviderConfig{
		Provider: ProviderMock,
	})
	p, err := factory.GetProvider()
	if err != nil {
		t.Fatalf("GetProvider() after Reset() error = %v", err)
	}
	if p == nil {
		t.Error("GetProvider() after Reset() should work")
	}
}

func TestFactory_GetProvider_AWS_NotImplemented(t *testing.T) {
	factory := NewFactory()
	factory.SetConfig(&ProviderConfig{
		Provider: ProviderAWS,
		AWS: &AWSConfig{
			Region: "us-east-1",
		},
	})

	_, err := factory.GetProvider()
	if err == nil {
		t.Fatal("GetProvider() for AWS should return error (not implemented)")
	}
	if !IsProviderNotConfigured(err) {
		t.Errorf("error should be ProviderNotConfiguredError, got %T", err)
	}
}

func TestFactory_GetProvider_Kubernetes_Registered(t *testing.T) {
	// ==========================================================================
	// KUBERNETES-NATIVE PROVIDER TEST
	//
	// The Kubernetes provider requires a live kubeconfig when using the
	// built-in constructor. For unit tests, we register a custom constructor
	// so the factory path is still verified without external dependencies.
	// ==========================================================================
	factory := NewFactory()
	customProvider := &testProvider{name: "kubernetes-test"}
	factory.RegisterProvider(ProviderKubernetes, func(config *ProviderConfig) (InfrastructureProvider, error) {
		return customProvider, nil
	})
	factory.SetConfig(&ProviderConfig{
		Provider: ProviderKubernetes,
		Kubernetes: &KubernetesConfig{
			PostgresOperator: "cnpg",
			RedisOperator:    "spotahome",
			StorageClass:     "standard",
		},
	})

	provider, err := factory.GetProvider()
	if err != nil {
		t.Fatalf("GetProvider() for Kubernetes error = %v", err)
	}
	if provider != customProvider {
		t.Errorf("GetProvider() should return registered provider")
	}
}

func TestFactory_RegisterProvider(t *testing.T) {
	factory := NewFactory()

	// Register custom provider constructor
	customProvider := &testProvider{name: "custom"}
	factory.RegisterProvider("custom", func(config *ProviderConfig) (InfrastructureProvider, error) {
		return customProvider, nil
	})

	factory.SetConfig(&ProviderConfig{
		Provider: "custom",
	})

	provider, err := factory.GetProvider()
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}
	if provider != customProvider {
		t.Error("GetProvider() should return registered custom provider")
	}
}

// testProvider is a minimal provider for testing custom registration.
type testProvider struct {
	name string
}

func (p *testProvider) Provision(ctx context.Context, app *platformv1alpha1.Application) (*ResourceState, error) {
	return &ResourceState{}, nil
}

func (p *testProvider) GetStatus(ctx context.Context, app *platformv1alpha1.Application) (*ResourceState, error) {
	return &ResourceState{}, nil
}

func (p *testProvider) Destroy(ctx context.Context, app *platformv1alpha1.Application) error {
	return nil
}

func (p *testProvider) Name() string {
	return p.name
}

func (p *testProvider) Type() ProviderType {
	return "custom"
}

func (p *testProvider) Healthy(ctx context.Context) bool {
	return true
}

var _ InfrastructureProvider = (*testProvider)(nil)
