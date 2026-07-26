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
	"time"

	corev1 "k8s.io/api/core/v1"
)

// =============================================================================
// CORE TYPES FOR INFRASTRUCTURE PROVISIONING
// =============================================================================
//
// These types represent the state of infrastructure resources.
// They are cloud-agnostic - AWS, GCP, and Local providers all return
// the same types, just with different underlying implementations.
//
// WHY SEPARATE TYPES (not reusing API types):
//   1. Decoupling: Provider package doesn't import API types
//   2. Flexibility: Can add provider-internal fields not exposed in API
//   3. Testing: Easier to construct test data
//   4. Evolution: API types and provider types can evolve independently
//
// HOW CROSSPLANE DOES IT:
//   - Managed Resources have Observation fields (provider-specific)
//   - These map to Status fields in the CRD
//   - We do similar: ProviderState → ApplicationStatus
//
// =============================================================================

// ProviderType identifies which infrastructure provider is in use.
// =============================================================================
// WHY STRING TYPE:
//
//	Could use iota consts, but string is more readable in logs/configs
//	and easier to extend (add new providers without recompiling).
//
// =============================================================================
type ProviderType string

const (
	// ProviderAWS uses AWS services via Terraform
	// (RDS, ElastiCache, SQS, S3, IAM)
	ProviderAWS ProviderType = "aws"

	// ProviderGCP uses Google Cloud services via Terraform
	// (Cloud SQL, Memorystore, Pub/Sub, GCS, IAM)
	ProviderGCP ProviderType = "gcp"

	// ProviderKubernetes deploys everything in-cluster using Kubernetes operators
	// ==========================================================================
	// WHY KUBERNETES-NATIVE PROVIDER:
	//
	//   1. PORTABILITY: No cloud vendor lock-in. Works on any Kubernetes cluster
	//      (EKS, GKE, AKS, bare-metal, k3s, kind, minikube)
	//
	//   2. SIMPLICITY: No Terraform, no cloud credentials, no IAM roles.
	//      Just deploy operators and Steward creates resources.
	//
	//   3. COST: Perfect for dev/staging where managed services are overkill.
	//      A PostgreSQL pod is free vs $50+/month for RDS.
	//
	//   4. LATENCY: In-cluster databases have lower latency than external services.
	//
	//   5. OFFLINE: Works in air-gapped environments with no cloud access.
	//
	// WHAT IT PROVISIONS:
	//   - Database: CloudNativePG (postgres), Vitess (mysql), or StatefulSets
	//   - Cache: Redis via Bitnami Helm or Redis Operator
	//   - Queue: RabbitMQ Operator, NATS, or Kafka Strimzi
	//   - Storage: PersistentVolumeClaims (PVCs)
	//
	// TRADEOFFS vs MANAGED SERVICES:
	//   ┌─────────────────┬─────────────────────┬─────────────────────────────┐
	//   │ Aspect          │ K8s-Native          │ AWS/GCP Managed             │
	//   ├─────────────────┼─────────────────────┼─────────────────────────────┤
	//   │ Setup           │ Install operators   │ Configure cloud creds       │
	//   │ Cost            │ Pod resources only  │ Per-hour service charges    │
	//   │ HA              │ StatefulSet replicas│ Multi-AZ built-in           │
	//   │ Backups         │ Manual / VolumeSnap │ Automated snapshots         │
	//   │ Scaling         │ Manual / VPA        │ Auto-scaling                │
	//   │ Maintenance     │ You manage upgrades │ Provider handles patches    │
	//   │ SLA             │ Your responsibility │ Provider SLA (99.95%+)      │
	//   │ Compliance      │ You configure       │ SOC2/HIPAA pre-configured   │
	//   └─────────────────┴─────────────────────┴─────────────────────────────┘
	//
	// BEST FOR:
	//   - Development and staging environments
	//   - Cost-sensitive teams
	//   - Air-gapped or on-prem deployments
	//   - Ephemeral per-PR preview environments (created and torn down often)
	//   - Edge deployments (k3s on Raspberry Pi)
	//
	// NOT RECOMMENDED FOR:
	//   - Production workloads requiring high SLA
	//   - Compliance-heavy environments (unless you configure properly)
	//   - Teams without Kubernetes expertise
	//
	// ==========================================================================
	ProviderKubernetes ProviderType = "kubernetes"

	// ProviderLocal uses in-cluster or mock resources for local development
	// (CloudNativePG, Redis Operator, or pure mocks)
	// DEPRECATED: Use ProviderKubernetes for real in-cluster resources
	// or ProviderMock for testing
	ProviderLocal ProviderType = "local"

	// ProviderMock is for unit testing - returns configured responses
	// without any real infrastructure
	ProviderMock ProviderType = "mock"
)

// ResourcePhase represents the lifecycle phase of an infrastructure resource.
// =============================================================================
// WHY NOT REUSE ApplicationPhase:
//
//	ApplicationPhase is for the overall Application lifecycle.
//	ResourcePhase is for individual infrastructure resources.
//	An Application might have Database=Ready but Cache=Provisioning.
//
// PHASE STATE MACHINE:
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│                                                                         │
//	│   ┌─────────┐      ┌──────────────┐      ┌─────────┐                    │
//	│   │ Unknown │ ────►│ Provisioning │ ────►│  Ready  │◄───┐               │
//	│   └─────────┘      └──────────────┘      └────┬────┘    │               │
//	│        │                  │                   │         │               │
//	│        │                  │ error             │ update  │ recover       │
//	│        │                  ▼                   ▼         │               │
//	│        │            ┌──────────┐        ┌──────────┐    │               │
//	│        └───────────►│  Failed  │◄───────│ Updating │────┘               │
//	│                     └──────────┘        └──────────┘                    │
//	│                          │                                              │
//	│                          │ delete                                       │
//	│                          ▼                                              │
//	│                    ┌──────────┐                                         │
//	│                    │ Deleting │                                         │
//	│                    └──────────┘                                         │
//	│                                                                         │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// =============================================================================
type ResourcePhase string

const (
	// ResourceUnknown - Initial state, status not yet determined
	ResourceUnknown ResourcePhase = "Unknown"

	// ResourceProvisioning - Resource is being created
	ResourceProvisioning ResourcePhase = "Provisioning"

	// ResourceReady - Resource is fully provisioned and healthy
	ResourceReady ResourcePhase = "Ready"

	// ResourceUpdating - Resource is being updated (e.g., scaling, version change)
	ResourceUpdating ResourcePhase = "Updating"

	// ResourceFailed - Resource provisioning or operation failed
	ResourceFailed ResourcePhase = "Failed"

	// ResourceDeleting - Resource is being destroyed
	ResourceDeleting ResourcePhase = "Deleting"

	// ResourceNotFound - Resource doesn't exist (not requested in spec)
	ResourceNotFound ResourcePhase = "NotFound"
)

// ResourceState contains the current state of all infrastructure for an Application.
// This is returned by InfrastructureProvider.Provision() and GetStatus().
// =============================================================================
// WHY AGGREGATE STATE:
//
//	Instead of returning just one resource's state, we return ALL resources
//	for an Application in one call. This:
//	  1. Reduces round-trips (one Provision call, not one per resource)
//	  2. Enables atomic status updates
//	  3. Allows provider to optimize (batch Terraform operations)
//
// HOW CROSSPLANE DIFFERS:
//
//	Crossplane has one CRD per resource (RDSInstance, ElastiCacheCluster).
//	Each is reconciled independently. We aggregate for simplicity.
//
// =============================================================================
type ResourceState struct {
	// Database contains the state of the database resource (if requested)
	Database *DatabaseState `json:"database,omitempty"`

	// Cache contains the state of the cache resource (if requested)
	Cache *CacheState `json:"cache,omitempty"`

	// Queue contains the state of the queue resource (if requested)
	Queue *QueueState `json:"queue,omitempty"`

	// Storage contains the state of the storage resource (if requested)
	Storage *StorageState `json:"storage,omitempty"`

	// ProvisioningStartTime tracks when provisioning started.
	// Used to detect stuck operations and calculate provisioning duration.
	ProvisioningStartTime *time.Time `json:"provisioningStartTime,omitempty"`

	// LastUpdateTime is when this state was last refreshed from the provider.
	LastUpdateTime time.Time `json:"lastUpdateTime"`

	// ProviderMetadata contains provider-specific information (for debugging).
	// Example: Terraform run ID, AWS request IDs, etc.
	ProviderMetadata map[string]string `json:"providerMetadata,omitempty"`
}

// IsReady returns true if ALL requested resources are in Ready phase.
func (s *ResourceState) IsReady() bool {
	if s.Database != nil && s.Database.Phase != ResourceReady {
		return false
	}
	if s.Cache != nil && s.Cache.Phase != ResourceReady {
		return false
	}
	if s.Queue != nil && s.Queue.Phase != ResourceReady {
		return false
	}
	if s.Storage != nil && s.Storage.Phase != ResourceReady {
		return false
	}
	return true
}

// HasFailures returns true if any resource is in Failed phase.
func (s *ResourceState) HasFailures() bool {
	if s.Database != nil && s.Database.Phase == ResourceFailed {
		return true
	}
	if s.Cache != nil && s.Cache.Phase == ResourceFailed {
		return true
	}
	if s.Queue != nil && s.Queue.Phase == ResourceFailed {
		return true
	}
	if s.Storage != nil && s.Storage.Phase == ResourceFailed {
		return true
	}
	return false
}

// IsProvisioning returns true if any resource is still being provisioned.
func (s *ResourceState) IsProvisioning() bool {
	if s.Database != nil && s.Database.Phase == ResourceProvisioning {
		return true
	}
	if s.Cache != nil && s.Cache.Phase == ResourceProvisioning {
		return true
	}
	if s.Queue != nil && s.Queue.Phase == ResourceProvisioning {
		return true
	}
	if s.Storage != nil && s.Storage.Phase == ResourceProvisioning {
		return true
	}
	return false
}

// DatabaseState represents the current state of a database resource.
// =============================================================================
// CONNECTION INFO:
//
//	After provisioning, applications need to connect. We provide:
//	  - Endpoint: DNS name or IP
//	  - Port: Connection port
//	  - SecretRef: Kubernetes Secret with credentials
//
//	The SecretRef pattern:
//	  1. Provider creates Secret with username/password
//	  2. Provider creates this state with SecretRef pointing to Secret
//	  3. Controller updates Application status with SecretRef
//	  4. Application's Deployment mounts Secret as env vars
//
// WHY SECRET REFERENCE (not inline credentials):
//   - Secrets are encrypted at rest in etcd
//   - RBAC can limit who sees credentials
//   - Easy to rotate (update Secret, not Application)
//   - Standard K8s pattern that tools understand
//
// =============================================================================
type DatabaseState struct {
	// Phase is the current lifecycle phase
	Phase ResourcePhase `json:"phase"`

	// Endpoint is the connection endpoint (DNS name or IP)
	// Empty until provisioning completes
	Endpoint string `json:"endpoint,omitempty"`

	// Port is the connection port (typically 5432 for PostgreSQL)
	Port int32 `json:"port,omitempty"`

	// SecretRef points to the Kubernetes Secret containing credentials
	// The Secret contains keys: username, password, and optionally connection-string
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// ResourceID is the provider-specific identifier (e.g., AWS ARN)
	// Useful for debugging and external lookups
	ResourceID string `json:"resourceId,omitempty"`

	// Message provides human-readable status information
	// Especially useful when Phase is Failed or Provisioning
	Message string `json:"message,omitempty"`

	// Engine is the database engine (postgres, mysql)
	Engine string `json:"engine,omitempty"`

	// Version is the actual provisioned version
	Version string `json:"version,omitempty"`
}

// CacheState represents the current state of a cache resource.
type CacheState struct {
	// Phase is the current lifecycle phase
	Phase ResourcePhase `json:"phase"`

	// Endpoint is the connection endpoint
	Endpoint string `json:"endpoint,omitempty"`

	// Port is the connection port (typically 6379 for Redis)
	Port int32 `json:"port,omitempty"`

	// SecretRef points to the Secret containing auth token (if any)
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// ResourceID is the provider-specific identifier
	ResourceID string `json:"resourceId,omitempty"`

	// Message provides human-readable status information
	Message string `json:"message,omitempty"`

	// Engine is the cache engine (redis, memcached)
	Engine string `json:"engine,omitempty"`

	// Version is the actual provisioned version
	Version string `json:"version,omitempty"`
}

// QueueState represents the current state of a queue resource.
type QueueState struct {
	// Phase is the current lifecycle phase
	Phase ResourcePhase `json:"phase"`

	// URL is the queue endpoint URL
	// For SQS: https://sqs.us-east-1.amazonaws.com/123456789/my-queue
	URL string `json:"url,omitempty"`

	// ARN is the Amazon Resource Name or equivalent identifier
	ARN string `json:"arn,omitempty"`

	// DeadLetterQueueURL is the DLQ URL if configured
	DeadLetterQueueURL string `json:"deadLetterQueueUrl,omitempty"`

	// DeadLetterQueueARN is the DLQ ARN if configured
	DeadLetterQueueARN string `json:"deadLetterQueueArn,omitempty"`

	// ResourceID is the provider-specific identifier
	ResourceID string `json:"resourceId,omitempty"`

	// Message provides human-readable status information
	Message string `json:"message,omitempty"`

	// Type is the queue type (sqs, rabbitmq, kafka)
	Type string `json:"type,omitempty"`
}

// StorageState represents the current state of an object storage resource.
type StorageState struct {
	// Phase is the current lifecycle phase
	Phase ResourcePhase `json:"phase"`

	// BucketName is the name of the storage bucket
	BucketName string `json:"bucketName,omitempty"`

	// BucketARN is the Amazon Resource Name or equivalent identifier
	BucketARN string `json:"bucketArn,omitempty"`

	// Region is where the bucket is located
	Region string `json:"region,omitempty"`

	// ResourceID is the provider-specific identifier
	ResourceID string `json:"resourceId,omitempty"`

	// Message provides human-readable status information
	Message string `json:"message,omitempty"`

	// Type is the storage type (s3, gcs)
	Type string `json:"type,omitempty"`
}

// ProviderConfig holds configuration for infrastructure providers.
// =============================================================================
// WHY CONFIG STRUCT (not CRD yet):
//
//	For MVP, we use a simple config struct that can be loaded from:
//	  - Environment variables (STEWARD_PROVIDER=aws)
//	  - ConfigMap (mounted as file)
//	  - Command-line flags
//
//	Promoting this to a ProviderConfig CRD would buy:
//	  - Per-namespace provider overrides
//	  - Multi-cloud deployments
//	  - Dynamic provider configuration
//
// HOW CROSSPLANE DOES IT:
//   - ProviderConfig CRD per provider (ProviderConfig.aws.crossplane.io)
//   - Stores credentials reference (Secret or IRSA)
//   - Managed Resources reference ProviderConfig by name
//
// =============================================================================
type ProviderConfig struct {
	// Provider is which infrastructure provider to use
	Provider ProviderType `json:"provider"`

	// AWS contains AWS-specific configuration
	AWS *AWSConfig `json:"aws,omitempty"`

	// GCP contains GCP-specific configuration
	GCP *GCPConfig `json:"gcp,omitempty"`

	// Kubernetes contains in-cluster provider configuration
	Kubernetes *KubernetesConfig `json:"kubernetes,omitempty"`

	// Local contains local/mock provider configuration
	// DEPRECATED: Use Kubernetes for real in-cluster resources
	Local *LocalConfig `json:"local,omitempty"`
}

// AWSConfig contains AWS-specific provider configuration.
type AWSConfig struct {
	// Region is the AWS region for resource provisioning
	// Example: us-east-1, eu-west-2
	Region string `json:"region"`

	// StateBackend configures Terraform state storage
	StateBackend *S3BackendConfig `json:"stateBackend,omitempty"`

	// DefaultVPCID is the VPC to provision resources in
	// Can be overridden per-application in the future
	DefaultVPCID string `json:"defaultVpcId,omitempty"`

	// DefaultSubnetIDs are the subnets for multi-AZ deployments
	DefaultSubnetIDs []string `json:"defaultSubnetIds,omitempty"`

	// DefaultSecurityGroupID is applied to all resources
	DefaultSecurityGroupID string `json:"defaultSecurityGroupId,omitempty"`

	// RolePath is the IAM role path for created roles
	// Example: /steward/
	RolePath string `json:"rolePath,omitempty"`

	// TagPrefix is prepended to all resource tags
	// Example: steward- → steward-payments-api
	TagPrefix string `json:"tagPrefix,omitempty"`

	// SizeMapping maps abstract sizes to AWS instance types
	// If not provided, defaults are used
	SizeMapping map[string]AWSSizeMapping `json:"sizeMapping,omitempty"`
}

// S3BackendConfig configures Terraform's S3 backend for state storage.
// =============================================================================
// WHY S3 + DYNAMODB:
//
//	Terraform state must be:
//	  1. Durable (survives controller restarts) → S3
//	  2. Locked (prevent concurrent modifications) → DynamoDB
//	  3. Isolated (per-application state files) → Key path pattern
//
// STATE KEY PATTERN:
//
//	apps/{namespace}/{name}/terraform.tfstate
//
//	Example:
//	apps/default/payments-api/terraform.tfstate
//
// HOW TERRAFORM CLOUD DIFFERS:
//   - TFC stores state in their backend (no S3 needed)
//   - Locking handled by TFC
//   - We use S3 for self-hosted simplicity
//
// =============================================================================
type S3BackendConfig struct {
	// Bucket is the S3 bucket name for state storage
	Bucket string `json:"bucket"`

	// Region is the bucket's region (can differ from resource region)
	Region string `json:"region,omitempty"`

	// DynamoDBTable is the table name for state locking
	DynamoDBTable string `json:"dynamoDbTable"`

	// KeyPrefix is prepended to state file paths
	// Final key: {keyPrefix}/apps/{namespace}/{name}/terraform.tfstate
	KeyPrefix string `json:"keyPrefix,omitempty"`

	// Encrypt enables server-side encryption for state files
	Encrypt bool `json:"encrypt"`

	// KMSKeyID is the KMS key for state encryption (optional)
	KMSKeyID string `json:"kmsKeyId,omitempty"`
}

// AWSSizeMapping maps an abstract size to AWS-specific instance types.
type AWSSizeMapping struct {
	// RDSInstanceClass is the RDS instance type
	// Example: db.t3.micro, db.t3.medium, db.m5.large
	RDSInstanceClass string `json:"rdsInstanceClass,omitempty"`

	// ElastiCacheNodeType is the ElastiCache node type
	// Example: cache.t3.micro, cache.t3.medium, cache.m5.large
	ElastiCacheNodeType string `json:"elastiCacheNodeType,omitempty"`
}

// GCPConfig contains GCP-specific provider configuration.
type GCPConfig struct {
	// Project is the GCP project ID
	Project string `json:"project"`

	// Region is the GCP region
	Region string `json:"region"`

	// Zone is the default zone (for zonal resources)
	Zone string `json:"zone,omitempty"`

	// Network is the VPC network name or self-link
	Network string `json:"network,omitempty"`

	// Subnetwork is the subnetwork name or self-link
	Subnetwork string `json:"subnetwork,omitempty"`

	// StateBackend configures Terraform GCS backend
	StateBackend *GCSBackendConfig `json:"stateBackend,omitempty"`
}

// GCSBackendConfig configures Terraform's GCS backend for state storage.
type GCSBackendConfig struct {
	// Bucket is the GCS bucket name
	Bucket string `json:"bucket"`

	// Prefix is the path prefix for state files
	Prefix string `json:"prefix,omitempty"`
}

// LocalConfig contains configuration for local/development provider.
type LocalConfig struct {
	// UseMocks if true, doesn't provision any real resources
	// Returns fake endpoints for testing
	UseMocks bool `json:"useMocks"`

	// UseCloudNativePG if true, provisions PostgreSQL via CloudNativePG operator
	UseCloudNativePG bool `json:"useCloudNativePG,omitempty"`

	// UseRedisOperator if true, provisions Redis via Redis Operator
	UseRedisOperator bool `json:"useRedisOperator,omitempty"`

	// MockDelay simulates provisioning delay (for testing async behavior)
	MockDelay time.Duration `json:"mockDelay,omitempty"`
}

// KubernetesConfig configures the in-cluster Kubernetes-native provider.
// =============================================================================
// KUBERNETES-NATIVE PROVIDER
//
// This provider deploys all infrastructure as Kubernetes resources within
// the same cluster. No external cloud services, no Terraform, just K8s.
//
// ARCHITECTURE:
//
//	┌───────────────────────────────────────────────────────────────────────┐
//	│                         Kubernetes Cluster                            │
//	├───────────────────────────────────────────────────────────────────────┤
//	│                                                                       │
//	│  ┌─────────────┐     ┌──────────────────────────────────────────────┐ │
//	│  │ Steward     │────►│            Operator CRDs                     │ │
//	│  │ Controller  │     │  ┌──────────┐ ┌──────────┐ ┌───────────────┐ │ │
//	│  └─────────────┘     │  │ Cluster  │ │ Redis    │ │RabbitmqCluster│ │ │
//	│         │            │  │ (CNPG)   │ │ (Bitnami)│ │ (RabbitMQ Op) │ │ │
//	│         │            │  └────┬─────┘ └────┬─────┘ └──────┬────────┘ │ │
//	│         │            └───────┼────────────┼──────────────┼──────────┘ │
//	│         │                    │            │              │            │
//	│         │                    ▼            ▼              ▼            │
//	│         │            ┌─────────────────────────────────────────────┐  │
//	│         │            │              StatefulSets / Pods            │  │
//	│         │            │   postgres-0   redis-0   rabbitmq-0         │  │
//	│         │            └─────────────────────────────────────────────┘  │
//	│         │                    │            │              │            │
//	│         │                    ▼            ▼              ▼            │
//	│         │            ┌─────────────────────────────────────────────┐  │
//	│         │            │           PersistentVolumeClaims            │  │
//	│         └───────────►│   db-pvc        redis-pvc    rabbitmq-pvc   │  │
//	│                      └─────────────────────────────────────────────┘  │
//	│                                                                       │
//	└───────────────────────────────────────────────────────────────────────┘
//
// SUPPORTED OPERATORS:
//
//	┌────────────┬─────────────────────────────────────────────────────────────┐
//	│ Resource   │ Operator Options                                            │
//	├────────────┼─────────────────────────────────────────────────────────────┤
//	│ PostgreSQL │ CloudNativePG (cnpg.io) - Production-grade, recommended     │
//	│            │ Zalando Postgres Operator - Feature-rich, patroni-based     │
//	│            │ CrunchyData PGO - Enterprise-focused                        │
//	│            │ Fallback: Simple StatefulSet + PVC                          │
//	├────────────┼─────────────────────────────────────────────────────────────┤
//	│ MySQL      │ Vitess Operator - Sharding, HA replicas                     │
//	│            │ Percona XtraDB - Galera cluster                             │
//	│            │ Fallback: StatefulSet + PVC                                 │
//	├────────────┼─────────────────────────────────────────────────────────────┤
//	│ Redis      │ Bitnami Redis Helm - Simple, battle-tested                  │
//	│            │ Redis Operator (Spotahome) - CRD-based                      │
//	│            │ Redis Cluster Operator - Clustering support                 │
//	├────────────┼─────────────────────────────────────────────────────────────┤
//	│ Queue      │ RabbitMQ Cluster Operator - Full-featured                   │
//	│            │ NATS Operator - Lightweight, cloud-native                   │
//	│            │ Strimzi (Kafka) - Event streaming                           │
//	├────────────┼─────────────────────────────────────────────────────────────┤
//	│ Storage    │ PersistentVolumeClaim - Native K8s                          │
//	│            │ MinIO (S3-compatible) - Object storage                      │
//	└────────────┴─────────────────────────────────────────────────────────────┘
//
// WHAT'S NOT SUPPORTED:
//
//	Some resources don't have good K8s-native equivalents:
//	  - IAM Roles / Service Accounts with cloud permissions
//	  - CDN (CloudFront, Cloud CDN)
//	  - DNS (Route53, Cloud DNS) - though external-dns can help
//	  - Managed ML services (SageMaker, Vertex AI)
//
//	For these, the provider returns UnsupportedResourceError with guidance.
//
// STORAGE CLASSES:
//
//	The provider uses storage classes for PVCs. Different classes offer:
//	  - standard: Default, usually network-attached
//	  - fast/premium: SSD-backed for databases
//	  - retain: Prevents data loss on PVC deletion
//
// =============================================================================
type KubernetesConfig struct {
	// Namespace is the default namespace for provisioned resources
	// If empty, uses the Application's namespace
	Namespace string `json:"namespace,omitempty"`

	// PostgresOperator selects which PostgreSQL operator to use
	// Options: "cnpg" (default), "zalando", "crunchydata", "statefulset"
	PostgresOperator string `json:"postgresOperator,omitempty"`

	// MySQLOperator selects which MySQL operator to use
	// Options: "vitess", "percona", "statefulset"
	MySQLOperator string `json:"mysqlOperator,omitempty"`

	// RedisOperator selects which Redis operator to use
	// Options: "bitnami" (default), "spotahome", "statefulset"
	RedisOperator string `json:"redisOperator,omitempty"`

	// QueueOperator selects which queue operator to use
	// Options: "rabbitmq" (default), "nats", "strimzi"
	QueueOperator string `json:"queueOperator,omitempty"`

	// StorageClass is the default storage class for PVCs
	// If empty, uses cluster default
	StorageClass string `json:"storageClass,omitempty"`

	// StorageClassFast is the storage class for high-performance needs
	// Used for databases that need SSD
	StorageClassFast string `json:"storageClassFast,omitempty"`

	// UseMinIO if true, provisions MinIO for S3-compatible storage
	// If false, uses PVCs for storage (less feature-rich but simpler)
	UseMinIO bool `json:"useMinIO,omitempty"`

	// ResourceLimits constrains resources for provisioned pods
	// Useful for dev/staging to prevent resource exhaustion
	ResourceLimits *ResourceLimitsConfig `json:"resourceLimits,omitempty"`

	// InstallOperators if true, auto-installs required operators if missing
	// Requires cluster-admin permissions
	InstallOperators bool `json:"installOperators,omitempty"`
}

// ResourceLimitsConfig sets default resource constraints for K8s-native resources.
type ResourceLimitsConfig struct {
	// MaxCPU is the maximum CPU per pod (e.g., "2", "500m")
	MaxCPU string `json:"maxCpu,omitempty"`

	// MaxMemory is the maximum memory per pod (e.g., "4Gi", "512Mi")
	MaxMemory string `json:"maxMemory,omitempty"`

	// MaxStorage is the maximum PVC size (e.g., "100Gi", "10Gi")
	MaxStorage string `json:"maxStorage,omitempty"`
}
