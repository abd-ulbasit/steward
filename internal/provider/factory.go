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
	"os"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// =============================================================================
// PROVIDER FACTORY
// =============================================================================
//
// The Factory creates and caches InfrastructureProvider instances.
//
// WHY A FACTORY:
// ━━━━━━━━━━━━━━
//
//   1. DECOUPLING
//      Controller doesn't know about AWS/GCP/Local specifics.
//      It just asks the factory for "the provider" and uses the interface.
//
//   2. CONFIGURATION
//      Factory reads config (env vars, ConfigMap, CRD) and creates
//      the appropriate provider with correct settings.
//
//   3. LIFECYCLE
//      Factory manages provider lifecycle (creation, caching, cleanup).
//      Providers are expensive to create (initialize Terraform, validate creds).
//
//   4. TESTING
//      Easy to inject mock provider for tests without changing controller code.
//
// FACTORY PATTERN:
// ━━━━━━━━━━━━━━━━
//
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │                                                                         │
//   │  Factory.GetProvider(config)                                            │
//   │      │                                                                  │
//   │      ├── config.Provider == "aws"                                       │
//   │      │       └──► Create AWSProvider (cached)                           │
//   │      │            - Initialize Terraform                                │
//   │      │            - Validate AWS credentials                            │
//   │      │            - Configure state backend                             │
//   │      │                                                                  │
//   │      ├── config.Provider == "gcp"                                       │
//   │      │       └──► Create GCPProvider (cached)                           │
//   │      │                                                                  │
//   │      ├── config.Provider == "local"                                     │
//   │      │       └──► Create LocalProvider (cached)                         │
//   │      │                                                                  │
//   │      └── config.Provider == "mock"                                      │
//   │              └──► Create MockProvider (cached)                          │
//   │                                                                         │
//   └─────────────────────────────────────────────────────────────────────────┘
//
// HOW CROSSPLANE DOES IT:
//   - Each provider is a separate controller deployment
//   - ProviderConfig CRD tells provider which credentials to use
//   - No factory needed - each ManagedResource references ProviderConfig
//
// WHY WE USE FACTORY INSTEAD:
//   - Simpler deployment (one controller, not N per provider)
//   - Consistent behavior across providers
//   - Easy to add new providers
//
// =============================================================================

// Factory creates and manages InfrastructureProvider instances.
type Factory struct {
	// mu protects provider and config
	mu sync.RWMutex

	// config is the current provider configuration
	config *ProviderConfig

	// provider is the cached provider instance
	// Created on first GetProvider() call
	provider InfrastructureProvider

	// providerConstructors maps provider types to constructor functions
	// This allows injecting custom providers for testing
	providerConstructors map[ProviderType]ProviderConstructor
}

// ProviderConstructor is a function that creates an InfrastructureProvider.
// Used for dependency injection and testing.
type ProviderConstructor func(config *ProviderConfig) (InfrastructureProvider, error)

// NewFactory creates a new provider factory.
func NewFactory() *Factory {
	return &Factory{
		providerConstructors: make(map[ProviderType]ProviderConstructor),
	}
}

// RegisterProvider registers a constructor for a provider type.
// This allows cloud-specific packages to register themselves.
//
// USAGE:
//
//	factory := provider.NewFactory()
//	factory.RegisterProvider(provider.ProviderAWS, aws.NewProvider)
//	factory.RegisterProvider(provider.ProviderMock, provider.NewMockProvider)
func (f *Factory) RegisterProvider(providerType ProviderType, constructor ProviderConstructor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.providerConstructors[providerType] = constructor
}

// GetProvider returns the configured InfrastructureProvider.
// Creates the provider on first call, then returns cached instance.
//
// Thread-safe: multiple goroutines can call this safely.
func (f *Factory) GetProvider() (InfrastructureProvider, error) {
	f.mu.RLock()
	if f.provider != nil {
		defer f.mu.RUnlock()
		return f.provider, nil
	}
	f.mu.RUnlock()

	// Need to create provider - upgrade to write lock
	f.mu.Lock()
	defer f.mu.Unlock()

	// Double-check after acquiring write lock
	if f.provider != nil {
		return f.provider, nil
	}

	// Load config if not already loaded
	if f.config == nil {
		config, err := f.loadConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to load provider config: %w", err)
		}
		f.config = config
	}

	// Create provider
	provider, err := f.createProvider(f.config)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider: %w", err)
	}

	f.provider = provider
	return f.provider, nil
}

// SetConfig explicitly sets the provider configuration.
// Invalidates any cached provider (will be recreated on next GetProvider).
func (f *Factory) SetConfig(config *ProviderConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.config = config
	f.provider = nil // Invalidate cached provider
}

// SetProvider explicitly sets the provider (useful for testing).
func (f *Factory) SetProvider(provider InfrastructureProvider) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provider = provider
}

// Reset clears the cached provider and config.
// Next GetProvider() call will reload config and create new provider.
func (f *Factory) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provider = nil
	f.config = nil
}

// createProvider creates a new provider based on configuration.
func (f *Factory) createProvider(config *ProviderConfig) (InfrastructureProvider, error) {
	// Check if custom constructor is registered
	if constructor, ok := f.providerConstructors[config.Provider]; ok {
		return constructor(config)
	}

	// Built-in provider creation
	switch config.Provider {
	case ProviderMock:
		return NewMockProvider(config), nil

	case ProviderLocal:
		// LocalProvider is mock-backed; use ProviderKubernetes for real
		// in-cluster operators.
		return NewMockProvider(config), nil

	case ProviderAWS:
		// AWSProvider is not implemented.
		return nil, &ProviderNotConfiguredError{
			Provider: ProviderAWS,
			Message:  "AWS provider is not implemented; use the kubernetes provider",
		}

	case ProviderGCP:
		// GCPProvider not implemented yet (future)
		return nil, &ProviderNotConfiguredError{
			Provider: ProviderGCP,
			Message:  "GCP provider is not yet implemented",
		}

	case ProviderKubernetes:
		// ==========================================================================
		// KUBERNETES-NATIVE PROVIDER
		//
		// WHY THIS EXISTS:
		//   Users want to run everything in-cluster without cloud dependencies.
		//   Perfect for dev/staging, air-gapped environments, or cost savings.
		//
		// WHAT IT DOES:
		//   - PostgreSQL: CloudNativePG Cluster CRD or StatefulSet
		//   - Redis: Bitnami Redis or Redis Operator CRD
		//   - Queue: RabbitMQ Cluster Operator or NATS
		//   - Storage: PersistentVolumeClaims
		//
		// IMPLEMENTATION NOTE:
		//   The KubernetesProvider creates operator CRDs and Secrets in-cluster.
		//   It requires access to kubeconfig; if none is available, it returns
		//   ProviderNotConfigured with remediation guidance.
		// ==========================================================================
		return NewKubernetesProvider(config, nil, nil, nil)

	default:
		return nil, &ProviderNotConfiguredError{
			Provider: config.Provider,
			Message:  fmt.Sprintf("unknown provider type: %s", config.Provider),
		}
	}
}

// loadConfig loads provider configuration from environment or defaults.
// =============================================================================
// CONFIGURATION SOURCES (in priority order):
//
//  1. STEWARD_PROVIDER env var → Provider type
//  2. STEWARD_AWS_REGION → AWS region
//  3. STEWARD_STATE_BUCKET → S3 bucket for Terraform state
//  4. (Future) ConfigMap mounted at /etc/steward/config.yaml
//  5. (Future) ProviderConfig CRD
//
// WHY ENV VARS FIRST:
//   - Simple for local development / testing
//   - Works in any deployment (K8s, docker-compose, bare metal)
//   - No external dependencies
//
// DEFAULTS:
//   - Provider: "mock" (safe for development)
//   - This ensures the controller starts even without config
//
// =============================================================================
func (f *Factory) loadConfig() (*ProviderConfig, error) {
	ctx := context.Background()
	logger := log.FromContext(ctx)

	config := &ProviderConfig{}

	// Read provider type
	providerStr := os.Getenv("STEWARD_PROVIDER")
	if providerStr == "" {
		providerStr = "mock" // Safe default
		logger.Info("STEWARD_PROVIDER not set, defaulting to mock provider")
	}

	switch providerStr {
	case "aws":
		config.Provider = ProviderAWS
	case "gcp":
		config.Provider = ProviderGCP
	case "kubernetes":
		config.Provider = ProviderKubernetes
	case "local":
		config.Provider = ProviderLocal
	case "mock":
		config.Provider = ProviderMock
	default:
		return nil, fmt.Errorf("invalid STEWARD_PROVIDER: %s", providerStr)
	}

	// Load provider-specific config
	switch config.Provider {
	case ProviderAWS:
		awsConfig, err := loadAWSConfig()
		if err != nil {
			return nil, err
		}
		config.AWS = awsConfig

	case ProviderGCP:
		gcpConfig, err := loadGCPConfig()
		if err != nil {
			return nil, err
		}
		config.GCP = gcpConfig

	case ProviderKubernetes:
		config.Kubernetes = &KubernetesConfig{}

	case ProviderLocal, ProviderMock:
		config.Local = &LocalConfig{
			UseMocks: true,
		}
	}

	return config, nil
}

// loadAWSConfig loads AWS-specific configuration from environment.
func loadAWSConfig() (*AWSConfig, error) {
	region := os.Getenv("STEWARD_AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = "us-east-1" // Default
	}

	config := &AWSConfig{
		Region: region,
	}

	// State backend config
	stateBucket := os.Getenv("STEWARD_STATE_BUCKET")
	if stateBucket != "" {
		config.StateBackend = &S3BackendConfig{
			Bucket:        stateBucket,
			Region:        os.Getenv("STEWARD_STATE_REGION"),
			DynamoDBTable: os.Getenv("STEWARD_LOCK_TABLE"),
			KeyPrefix:     os.Getenv("STEWARD_STATE_PREFIX"),
			Encrypt:       true,
		}
		if config.StateBackend.Region == "" {
			config.StateBackend.Region = region
		}
		if config.StateBackend.DynamoDBTable == "" {
			config.StateBackend.DynamoDBTable = "steward-locks"
		}
	}

	// Networking defaults
	config.DefaultVPCID = os.Getenv("STEWARD_VPC_ID")
	// Subnets can be comma-separated
	subnetEnv := os.Getenv("STEWARD_SUBNET_IDS")
	if subnetEnv != "" {
		// Simple split - production would use proper parsing
		config.DefaultSubnetIDs = splitAndTrim(subnetEnv)
	}

	// Default size mappings
	config.SizeMapping = getDefaultAWSSizeMapping()

	return config, nil
}

// loadGCPConfig loads GCP-specific configuration from environment.
func loadGCPConfig() (*GCPConfig, error) {
	project := os.Getenv("STEWARD_GCP_PROJECT")
	if project == "" {
		project = os.Getenv("GOOGLE_PROJECT")
	}
	if project == "" {
		return nil, fmt.Errorf("STEWARD_GCP_PROJECT or GOOGLE_PROJECT must be set")
	}

	region := os.Getenv("STEWARD_GCP_REGION")
	if region == "" {
		region = "us-central1"
	}

	config := &GCPConfig{
		Project: project,
		Region:  region,
		Zone:    os.Getenv("STEWARD_GCP_ZONE"),
		Network: os.Getenv("STEWARD_GCP_NETWORK"),
	}

	// State backend config
	stateBucket := os.Getenv("STEWARD_GCS_STATE_BUCKET")
	if stateBucket != "" {
		config.StateBackend = &GCSBackendConfig{
			Bucket: stateBucket,
			Prefix: os.Getenv("STEWARD_GCS_STATE_PREFIX"),
		}
	}

	return config, nil
}

// getDefaultAWSSizeMapping returns the default size to instance type mappings.
// =============================================================================
// SIZE ABSTRACTION:
//
//	Users specify: size: small
//	Platform maps: db.t3.medium, cache.t3.small
//
//	This allows:
//	1. Users don't need to know AWS instance types
//	2. Platform team controls cost/performance balance
//	3. Easy to change mappings without updating applications
//
// INSTANCE CLASS SELECTION RATIONALE:
//
//	small  → t3.micro/small  (burstable, cheap, dev/test)
//	medium → t3.medium/large (burstable, moderate workloads)
//	large  → m5.large/xlarge (general purpose, production)
//	xlarge → m5.2xlarge+     (high performance, heavy workloads)
//
// =============================================================================
func getDefaultAWSSizeMapping() map[string]AWSSizeMapping {
	return map[string]AWSSizeMapping{
		"small": {
			RDSInstanceClass:    "db.t3.micro",
			ElastiCacheNodeType: "cache.t3.micro",
		},
		"medium": {
			RDSInstanceClass:    "db.t3.medium",
			ElastiCacheNodeType: "cache.t3.small",
		},
		"large": {
			RDSInstanceClass:    "db.m5.large",
			ElastiCacheNodeType: "cache.m5.large",
		},
		"xlarge": {
			RDSInstanceClass:    "db.m5.2xlarge",
			ElastiCacheNodeType: "cache.m5.xlarge",
		},
	}
}

// splitAndTrim splits a string by comma and trims whitespace.
func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := make([]string, 0)
	for _, p := range splitString(s, ',') {
		if trimmed := trimSpace(p); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}

// splitString splits a string by separator without importing strings package
func splitString(s string, sep byte) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	result = append(result, s[start:])
	return result
}

// trimSpace removes leading and trailing whitespace
func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
