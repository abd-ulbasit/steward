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
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
)

// =============================================================================
// MOCK PROVIDER
// =============================================================================
//
// MockProvider implements InfrastructureProvider for testing purposes.
// It doesn't provision any real infrastructure - instead, it simulates
// the provisioning lifecycle with configurable behavior.
//
// WHY MOCK PROVIDER:
// ━━━━━━━━━━━━━━━━━━
//
//   1. UNIT TESTING
//      Test controller logic without real cloud infrastructure.
//      Each test can configure exactly what the provider should return.
//
//   2. LOCAL DEVELOPMENT
//      Developers can run the controller locally without AWS credentials.
//      The mock simulates realistic provisioning behavior.
//
//   3. CI/CD
//      Automated tests in GitHub Actions don't need cloud access.
//      Tests run fast (no 10-minute RDS provisioning waits).
//
//   4. EDGE CASE TESTING
//      Easy to simulate failures, timeouts, quota limits.
//      Hard to trigger these in real cloud providers.
//
// HOW IT WORKS:
// ━━━━━━━━━━━━━
//
//   The mock maintains an in-memory state map per Application.
//   State progresses through phases:
//
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │  First Provision()                                                      │
//   │    └──► Creates state with Phase=Provisioning                           │
//   │         Sets ProvisioningStartTime                                      │
//   │                                                                         │
//   │  Second Provision() (after MockDelay)                                   │
//   │    └──► Updates state to Phase=Ready                                    │
//   │         Fills in endpoints, secrets                                     │
//   │                                                                         │
//   │  Destroy()                                                              │
//   │    └──► Removes state from memory                                       │
//   └─────────────────────────────────────────────────────────────────────────┘
//
// CONFIGURABLE BEHAVIOR:
// ━━━━━━━━━━━━━━━━━━━━━
//
//   MockProvider can be configured to:
//   - Return specific errors (for testing error handling)
//   - Simulate provisioning delay (for testing async behavior)
//   - Report as healthy or unhealthy (for testing health checks)
//   - Track method invocations (for verification in tests)
//
// =============================================================================

// MockProvider implements InfrastructureProvider for testing.
type MockProvider struct {
	// config is the provider configuration
	config *ProviderConfig

	// mu protects mutable state
	mu sync.RWMutex

	// state stores ResourceState per Application (namespace/name key)
	state map[string]*ResourceState

	// provisioningStart tracks when provisioning started per Application
	provisioningStart map[string]time.Time

	// =========================================================================
	// CONFIGURABLE BEHAVIOR FOR TESTING
	// =========================================================================

	// ProvisionDelay is how long provisioning takes to complete.
	// Default: 0 (instant). Set to simulate real provisioning time.
	ProvisionDelay time.Duration

	// DestroyDelay is how long destruction takes to complete.
	// Default: 0 (instant).
	DestroyDelay time.Duration

	// ProvisionError if set, Provision() returns this error.
	// Set to nil for normal behavior.
	ProvisionError error

	// DestroyError if set, Destroy() returns this error.
	DestroyError error

	// GetStatusError if set, GetStatus() returns this error.
	GetStatusError error

	// IsHealthy controls what Healthy() returns.
	// Default: true
	IsHealthy bool

	// =========================================================================
	// INVOCATION TRACKING FOR TEST ASSERTIONS
	// =========================================================================

	// ProvisionCalls records all Provision() invocations
	ProvisionCalls []ProvisionCall

	// DestroyCalls records all Destroy() invocations
	DestroyCalls []DestroyCall

	// GetStatusCalls records all GetStatus() invocations
	GetStatusCalls []GetStatusCall
}

// ProvisionCall records a Provision() invocation.
type ProvisionCall struct {
	Time      time.Time
	Namespace string
	Name      string
	App       *platformv1alpha1.Application
}

// DestroyCall records a Destroy() invocation.
type DestroyCall struct {
	Time      time.Time
	Namespace string
	Name      string
	App       *platformv1alpha1.Application
}

// GetStatusCall records a GetStatus() invocation.
type GetStatusCall struct {
	Time      time.Time
	Namespace string
	Name      string
	App       *platformv1alpha1.Application
}

// NewMockProvider creates a new mock provider with default configuration.
func NewMockProvider(config *ProviderConfig) *MockProvider {
	delay := time.Duration(0)
	if config != nil && config.Local != nil {
		delay = config.Local.MockDelay
	}

	return &MockProvider{
		config:            config,
		state:             make(map[string]*ResourceState),
		provisioningStart: make(map[string]time.Time),
		ProvisionDelay:    delay,
		IsHealthy:         true,
		ProvisionCalls:    make([]ProvisionCall, 0),
		DestroyCalls:      make([]DestroyCall, 0),
		GetStatusCalls:    make([]GetStatusCall, 0),
	}
}

// appKey returns the key for an Application in the state map.
func appKey(app *platformv1alpha1.Application) string {
	return fmt.Sprintf("%s/%s", app.Namespace, app.Name)
}

// Name returns the provider name.
func (m *MockProvider) Name() string {
	return "mock"
}

// Type returns the provider type.
func (m *MockProvider) Type() ProviderType {
	return ProviderMock
}

// Healthy returns whether the provider is healthy.
func (m *MockProvider) Healthy(ctx context.Context) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.IsHealthy
}

// Provision creates or updates infrastructure for an Application.
// =============================================================================
// BEHAVIOR:
//  1. First call: Creates state with Phase=Provisioning
//  2. Subsequent calls before ProvisionDelay: Returns Provisioning state
//  3. Calls after ProvisionDelay: Returns Ready state with mock endpoints
//
// MOCK DATA GENERATION:
//   - Endpoints: {app-name}.mock.local
//   - Ports: Standard ports (5432 for Postgres, 6379 for Redis)
//   - Secrets: Reference to mock secret (not actually created)
//   - Resource IDs: mock-{resource-type}-{app-name}
//
// =============================================================================
func (m *MockProvider) Provision(ctx context.Context, app *platformv1alpha1.Application) (*ResourceState, error) {
	logger := log.FromContext(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Record invocation
	m.ProvisionCalls = append(m.ProvisionCalls, ProvisionCall{
		Time:      time.Now(),
		Namespace: app.Namespace,
		Name:      app.Name,
		App:       app,
	})

	logger.Info("mock provider: Provision called",
		"app", app.Name,
		"namespace", app.Namespace,
		"hasDatabase", app.Spec.Database != nil,
		"hasCache", app.Spec.Cache != nil,
		"hasQueue", app.Spec.Queue != nil,
		"hasStorage", app.Spec.Storage != nil,
	)

	// Return configured error if set
	if m.ProvisionError != nil {
		return nil, m.ProvisionError
	}

	key := appKey(app)
	now := time.Now()

	// Check if this is first provision call
	startTime, exists := m.provisioningStart[key]
	if !exists {
		// First call - start provisioning
		m.provisioningStart[key] = now

		// Create initial state
		state := m.createInitialState(app, now)
		m.state[key] = state

		logger.Info("mock provider: started provisioning",
			"app", app.Name,
			"delay", m.ProvisionDelay,
		)

		// If no delay configured, skip to ready immediately
		if m.ProvisionDelay == 0 {
			m.updateStateToReady(app, state)
		}

		return state, nil
	}

	// Check if provisioning delay has passed
	elapsed := now.Sub(startTime)
	state := m.state[key]
	if state == nil {
		// Shouldn't happen, but handle gracefully
		state = m.createInitialState(app, now)
		m.state[key] = state
	}

	if elapsed < m.ProvisionDelay {
		// Still provisioning
		logger.Info("mock provider: still provisioning",
			"app", app.Name,
			"elapsed", elapsed,
			"remaining", m.ProvisionDelay-elapsed,
		)
		return state, nil
	}

	// Provisioning complete - update to ready
	m.updateStateToReady(app, state)

	logger.Info("mock provider: provisioning complete",
		"app", app.Name,
	)

	return state, nil
}

// createInitialState creates the initial Provisioning state.
func (m *MockProvider) createInitialState(app *platformv1alpha1.Application, startTime time.Time) *ResourceState {
	state := &ResourceState{
		ProvisioningStartTime: &startTime,
		LastUpdateTime:        startTime,
		ProviderMetadata: map[string]string{
			"provider":    "mock",
			"provisionId": fmt.Sprintf("mock-%s-%d", app.Name, startTime.Unix()),
			"startedAt":   startTime.Format(time.RFC3339),
		},
	}

	// Add resources based on spec
	if app.Spec.Database != nil {
		state.Database = &DatabaseState{
			Phase:   ResourceProvisioning,
			Message: "Database is being provisioned...",
			Engine:  string(app.Spec.Database.Type),
			Version: app.Spec.Database.Version,
		}
	}

	if app.Spec.Cache != nil {
		state.Cache = &CacheState{
			Phase:   ResourceProvisioning,
			Message: "Cache is being provisioned...",
			Engine:  string(app.Spec.Cache.Type),
		}
	}

	if app.Spec.Queue != nil {
		state.Queue = &QueueState{
			Phase:   ResourceProvisioning,
			Message: "Queue is being provisioned...",
			Type:    string(app.Spec.Queue.Type),
		}
	}

	if app.Spec.Storage != nil {
		state.Storage = &StorageState{
			Phase:   ResourceProvisioning,
			Message: "Storage bucket is being provisioned...",
			Type:    string(app.Spec.Storage.Type),
		}
	}

	return state
}

// updateStateToReady updates all resources to Ready phase with mock data.
func (m *MockProvider) updateStateToReady(app *platformv1alpha1.Application, state *ResourceState) {
	state.LastUpdateTime = time.Now()

	if state.Database != nil {
		state.Database.Phase = ResourceReady
		state.Database.Endpoint = fmt.Sprintf("%s-db.mock.local", app.Name)
		state.Database.Port = 5432
		if app.Spec.Database != nil && app.Spec.Database.Type == platformv1alpha1.DatabaseMySQL {
			state.Database.Port = 3306
		}
		state.Database.SecretRef = &corev1.LocalObjectReference{
			Name: fmt.Sprintf("%s-database-credentials", app.Name),
		}
		state.Database.ResourceID = fmt.Sprintf("mock-database-%s", app.Name)
		state.Database.Message = "Database is ready"
	}

	if state.Cache != nil {
		state.Cache.Phase = ResourceReady
		state.Cache.Endpoint = fmt.Sprintf("%s-cache.mock.local", app.Name)
		state.Cache.Port = 6379
		if app.Spec.Cache != nil && app.Spec.Cache.Type == platformv1alpha1.CacheMemcached {
			state.Cache.Port = 11211
		}
		state.Cache.ResourceID = fmt.Sprintf("mock-cache-%s", app.Name)
		state.Cache.Message = "Cache is ready"
	}

	if state.Queue != nil {
		state.Queue.Phase = ResourceReady
		state.Queue.URL = fmt.Sprintf("https://mock-queue.local/%s/%s-queue",
			app.Namespace, app.Name)
		state.Queue.ARN = fmt.Sprintf("arn:mock:sqs:local:000000000000:%s-queue", app.Name)
		if app.Spec.Queue != nil && app.Spec.Queue.DeadLetterQueue != nil &&
			app.Spec.Queue.DeadLetterQueue.Enabled != nil && *app.Spec.Queue.DeadLetterQueue.Enabled {
			state.Queue.DeadLetterQueueURL = fmt.Sprintf("https://mock-queue.local/%s/%s-dlq",
				app.Namespace, app.Name)
			state.Queue.DeadLetterQueueARN = fmt.Sprintf("arn:mock:sqs:local:000000000000:%s-dlq", app.Name)
		}
		state.Queue.ResourceID = fmt.Sprintf("mock-queue-%s", app.Name)
		state.Queue.Message = "Queue is ready"
	}

	if state.Storage != nil {
		state.Storage.Phase = ResourceReady
		state.Storage.BucketName = fmt.Sprintf("%s-%s-storage", app.Namespace, app.Name)
		state.Storage.BucketARN = fmt.Sprintf("arn:mock:s3:::%s-%s-storage",
			app.Namespace, app.Name)
		state.Storage.Region = "mock-region-1"
		state.Storage.ResourceID = fmt.Sprintf("mock-storage-%s", app.Name)
		state.Storage.Message = "Storage bucket is ready"
	}
}

// GetStatus returns the current state of infrastructure.
func (m *MockProvider) GetStatus(ctx context.Context, app *platformv1alpha1.Application) (*ResourceState, error) {
	logger := log.FromContext(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Record invocation
	m.GetStatusCalls = append(m.GetStatusCalls, GetStatusCall{
		Time:      time.Now(),
		Namespace: app.Namespace,
		Name:      app.Name,
		App:       app,
	})

	logger.Info("mock provider: GetStatus called",
		"app", app.Name,
		"namespace", app.Namespace,
	)

	// Return configured error if set
	if m.GetStatusError != nil {
		return nil, m.GetStatusError
	}

	key := appKey(app)
	state, exists := m.state[key]
	if !exists {
		return nil, &NotFoundError{
			ResourceType: "Application",
			ResourceID:   key,
			Message:      "no infrastructure provisioned for this application",
		}
	}

	return state, nil
}

// Destroy removes all infrastructure for an Application.
func (m *MockProvider) Destroy(ctx context.Context, app *platformv1alpha1.Application) error {
	logger := log.FromContext(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Record invocation
	m.DestroyCalls = append(m.DestroyCalls, DestroyCall{
		Time:      time.Now(),
		Namespace: app.Namespace,
		Name:      app.Name,
		App:       app,
	})

	logger.Info("mock provider: Destroy called",
		"app", app.Name,
		"namespace", app.Namespace,
	)

	// Return configured error if set
	if m.DestroyError != nil {
		return m.DestroyError
	}

	key := appKey(app)

	// Check if destruction delay applies
	if m.DestroyDelay > 0 {
		state, exists := m.state[key]
		if exists && state != nil {
			// Check if we're already deleting
			if state.Database != nil && state.Database.Phase == ResourceDeleting {
				// Check if delay has passed
				startTime := m.provisioningStart[key+"-destroy"]
				if time.Since(startTime) < m.DestroyDelay {
					return &NotReadyError{
						ResourceType: "Application",
						ResourceID:   key,
						CurrentPhase: ResourceDeleting,
						Message:      "destruction in progress",
					}
				}
			} else {
				// Start deletion
				m.provisioningStart[key+"-destroy"] = time.Now()
				m.setDeletingPhase(state)
				return &NotReadyError{
					ResourceType: "Application",
					ResourceID:   key,
					CurrentPhase: ResourceDeleting,
					Message:      "destruction started",
				}
			}
		}
	}

	// Remove state
	delete(m.state, key)
	delete(m.provisioningStart, key)
	delete(m.provisioningStart, key+"-destroy")

	logger.Info("mock provider: destroyed infrastructure",
		"app", app.Name,
	)

	return nil
}

// setDeletingPhase updates all resources to Deleting phase.
func (m *MockProvider) setDeletingPhase(state *ResourceState) {
	if state.Database != nil {
		state.Database.Phase = ResourceDeleting
		state.Database.Message = "Destroying database..."
	}
	if state.Cache != nil {
		state.Cache.Phase = ResourceDeleting
		state.Cache.Message = "Destroying cache..."
	}
	if state.Queue != nil {
		state.Queue.Phase = ResourceDeleting
		state.Queue.Message = "Destroying queue..."
	}
	if state.Storage != nil {
		state.Storage.Phase = ResourceDeleting
		state.Storage.Message = "Destroying storage bucket..."
	}
}

// =============================================================================
// TEST HELPER METHODS
// =============================================================================

// Reset clears all state and invocation records.
func (m *MockProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.state = make(map[string]*ResourceState)
	m.provisioningStart = make(map[string]time.Time)
	m.ProvisionCalls = make([]ProvisionCall, 0)
	m.DestroyCalls = make([]DestroyCall, 0)
	m.GetStatusCalls = make([]GetStatusCall, 0)
	m.ProvisionError = nil
	m.DestroyError = nil
	m.GetStatusError = nil
}

// SetState directly sets the state for an Application (for test setup).
func (m *MockProvider) SetState(app *platformv1alpha1.Application, state *ResourceState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[appKey(app)] = state
}

// GetStateForApp returns the current state for an Application.
func (m *MockProvider) GetStateForApp(app *platformv1alpha1.Application) *ResourceState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state[appKey(app)]
}

// HasBeenProvisioned returns true if Provision was called for the Application.
func (m *MockProvider) HasBeenProvisioned(app *platformv1alpha1.Application) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.state[appKey(app)]
	return exists
}

// ProvisionCallCount returns the number of Provision() calls.
func (m *MockProvider) ProvisionCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.ProvisionCalls)
}

// DestroyCallCount returns the number of Destroy() calls.
func (m *MockProvider) DestroyCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.DestroyCalls)
}

// GetStatusCallCount returns the number of GetStatus() calls.
func (m *MockProvider) GetStatusCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.GetStatusCalls)
}

// =============================================================================
// VERIFY INTERFACE IMPLEMENTATION
// =============================================================================

// Compile-time check that MockProvider implements InfrastructureProvider
var _ InfrastructureProvider = (*MockProvider)(nil)
