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

// =============================================================================
// APPLICATION CRD TYPE DEFINITIONS
// =============================================================================
//
// This file defines the Go structs that represent our Application CRD.
// Kubebuilder uses these structs + special comments (markers) to:
//   1. Generate CRD YAML schemas (make manifests)
//   2. Generate DeepCopy methods (make generate)
//   3. Configure validation, defaults, and RBAC
//
// HOW CRDs WORK:
//   1. You define Go structs with json tags
//   2. Run `make manifests` → controller-gen reads structs + markers
//   3. Generates OpenAPI schema embedded in CRD YAML
//   4. Apply CRD to cluster → API server knows about "Application" resource
//   5. Users can now: kubectl apply -f my-application.yaml
//
// ALTERNATIVES CONSIDERED:
//   ┌──────────────────────────────────────────────────────────────────────────┐
//   │ Approach              │ Pros                 │ Cons                      │
//   ├───────────────────────┼──────────────────────┼───────────────────────────┤
//   │ Go structs + markers  │ Type-safe, IDE       │ Requires Go knowledge     │
//   │ (what we use)         │ support, validated   │                           │
//   ├───────────────────────┼──────────────────────┼───────────────────────────┤
//   │ Raw CRD YAML          │ Direct control       │ Error-prone, no types     │
//   ├───────────────────────┼──────────────────────┼───────────────────────────┤
//   │ OpenAPI spec first    │ Language-agnostic    │ No DeepCopy generation    │
//   └───────────────────────┴──────────────────────┴───────────────────────────┘
//
// MARKER REFERENCE (the +kubebuilder comments):
//   +kubebuilder:validation:Required      → Field must be present
//   +kubebuilder:validation:Optional      → Field can be omitted
//   +kubebuilder:validation:Enum=a;b;c    → Only these values allowed
//   +kubebuilder:validation:Minimum=1     → Numeric minimum
//   +kubebuilder:validation:Pattern=...   → Regex pattern for strings
//   +kubebuilder:default=value            → Default value if omitted
//   +kubebuilder:subresource:status       → Status is separate subresource
//   +kubebuilder:printcolumn              → Shown in kubectl get output
//
// =============================================================================

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// =============================================================================
// CONSTANTS AND ENUMS
// =============================================================================
//
// WHY TYPED CONSTANTS:
//   Instead of raw strings like "postgres", we define typed constants.
//   This enables:
//   1. IDE autocomplete
//   2. Compile-time typo detection
//   3. Single source of truth for allowed values
//
// =============================================================================

// ApplicationPhase represents the current lifecycle phase of the Application.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed;Deleting
type ApplicationPhase string

const (
	// ApplicationPending - Application created but processing not started
	ApplicationPending ApplicationPhase = "Pending"
	// ApplicationProvisioning - Infrastructure is being provisioned
	ApplicationProvisioning ApplicationPhase = "Provisioning"
	// ApplicationReady - All resources provisioned and healthy
	ApplicationReady ApplicationPhase = "Ready"
	// ApplicationFailed - Provisioning failed, see conditions for details
	ApplicationFailed ApplicationPhase = "Failed"
	// ApplicationDeleting - Application is being deleted, cleanup in progress
	ApplicationDeleting ApplicationPhase = "Deleting"
)

// ServiceTier defines the importance level of the application.
// This affects SLAs, resource allocation, and alerting thresholds.
// +kubebuilder:validation:Enum=critical;standard;development
type ServiceTier string

const (
	// TierCritical - Production-critical, highest SLA (99.99%)
	TierCritical ServiceTier = "critical"
	// TierStandard - Standard production workload (99.9% SLA)
	TierStandard ServiceTier = "standard"
	// TierDevelopment - Non-production, can be disrupted
	TierDevelopment ServiceTier = "development"
)

// DatabaseType defines supported database engines.
// +kubebuilder:validation:Enum=postgres;mysql
type DatabaseType string

const (
	DatabasePostgres DatabaseType = "postgres"
	DatabaseMySQL    DatabaseType = "mysql"
)

// CacheType defines supported cache engines.
// +kubebuilder:validation:Enum=redis;memcached
type CacheType string

const (
	CacheRedis     CacheType = "redis"
	CacheMemcached CacheType = "memcached"
)

// QueueType defines supported queue/messaging systems.
// +kubebuilder:validation:Enum=sqs;rabbitmq;kafka
type QueueType string

const (
	QueueSQS      QueueType = "sqs"
	QueueRabbitMQ QueueType = "rabbitmq"
	QueueKafka    QueueType = "kafka"
)

// StorageType defines supported object storage systems.
// +kubebuilder:validation:Enum=s3;gcs
type StorageType string

const (
	StorageS3  StorageType = "s3"
	StorageGCS StorageType = "gcs"
)

// ResourceSize is an abstraction over instance sizes.
// The platform maps these to provider-specific instance types.
//
// WHY SIZE ABSTRACTION:
//
//	┌─────────────────────────────────────────────────────────────────────────┐
//	│ Instead of asking developers to specify:                                │
//	│   - db.t3.medium (AWS-specific)                                         │
//	│   - 2 vCPUs, 4GB RAM (too low-level)                                    │
//	│                                                                         │
//	│ We ask for:                                                             │
//	│   - size: small                                                         │
//	│                                                                         │
//	│ Platform maps to provider:                                              │
//	│   - AWS: db.t3.medium                                                   │
//	│   - GCP: db-custom-2-4096                                               │
//	│   - Local: 512Mi memory limit                                           │
//	│                                                                         │
//	│ BENEFITS:                                                               │
//	│   1. Developers don't need AWS/GCP knowledge                            │
//	│   2. Easy to migrate between clouds                                     │
//	│   3. Platform team controls costs                                       │
//	│   4. Consistent naming across services                                  │
//	└─────────────────────────────────────────────────────────────────────────┘
//
// +kubebuilder:validation:Enum=small;medium;large;xlarge
type ResourceSize string

const (
	SizeSmall  ResourceSize = "small"
	SizeMedium ResourceSize = "medium"
	SizeLarge  ResourceSize = "large"
	SizeXLarge ResourceSize = "xlarge"
)

// LogFormat defines the logging output format.
// +kubebuilder:validation:Enum=json;logfmt;text
type LogFormat string

const (
	LogFormatJSON   LogFormat = "json"
	LogFormatLogfmt LogFormat = "logfmt"
	LogFormatText   LogFormat = "text"
)

// =============================================================================
// APPLICATION SPEC - DESIRED STATE
// =============================================================================
//
// The Spec is what the user specifies - the DESIRED state.
// The controller reads this and works to make reality match.
//
// KUBERNETES API CONVENTION:
//   Spec = "I want X"
//   Status = "Current state is Y"
//   Controller = "Make Y equal X"
//
// =============================================================================

// ApplicationSpec defines the desired state of an Application.
// This is a cloud-agnostic specification that the platform interprets
// based on the target infrastructure provider.
type ApplicationSpec struct {
	// ===========================================================================
	// OWNERSHIP - Who owns this service
	// ===========================================================================
	//
	// WHY OWNERSHIP MATTERS:
	//   - Service catalog: "Who do I contact about payments-api?"
	//   - Cost allocation: "How much is Team X spending?"
	//   - Access control: Only team members can modify
	//   - Incident routing: PagerDuty/Slack alerts go to right people
	//
	// HOW BACKSTAGE DOES IT:
	//   - Uses catalog-info.yaml with ownership metadata
	//   - Links to identity providers for team membership
	//
	// ===========================================================================

	// Team that owns this application. Used for access control,
	// cost allocation, and service discovery.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Team string `json:"team"`

	// Owner is the email or identifier of the primary owner.
	// This person receives critical alerts and is the escalation point.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Owner string `json:"owner"`

	// Tier indicates the service level for this application.
	// Affects SLAs, resource allocation, and monitoring thresholds.
	//   - critical: 99.99% uptime SLA, highest priority
	//   - standard: 99.9% uptime SLA, normal priority
	//   - development: No SLA, can be disrupted during off-hours
	// +kubebuilder:default=standard
	// +optional
	Tier ServiceTier `json:"tier,omitempty"`

	// ===========================================================================
	// WORKLOAD - What to deploy
	// ===========================================================================
	//
	// WHY WE INCLUDE WORKLOAD:
	//   Unlike Crossplane (infrastructure-only) or ArgoCD (deployment-only),
	//   GoPlatform manages both infrastructure AND workload.
	//
	// This enables:
	//   1. One CRD = complete application (infra + deployment)
	//   2. Infrastructure knows about workload (can inject connection strings)
	//   3. Atomic deployments (infra + app move together)
	//
	// ALTERNATIVES:
	//   - Separate CRDs for Infra vs Workload (more flexible, more complexity)
	//   - Reference existing Deployment (less control, harder to template)
	//   - No workload management (users bring their own Deployments)
	//
	// ===========================================================================

	// Workload defines the container workload to deploy.
	// If not specified, only infrastructure is provisioned.
	// +optional
	Workload *WorkloadSpec `json:"workload,omitempty"`

	// Scaling defines horizontal pod autoscaling configuration.
	// +optional
	Scaling *ScalingSpec `json:"scaling,omitempty"`

	// ===========================================================================
	// INFRASTRUCTURE - Cloud resources needed
	// ===========================================================================
	//
	// These are CLOUD-AGNOSTIC specifications.
	// The InfrastructureProvider maps them to actual cloud resources.
	//
	// Example mapping for Database:
	//   ┌─────────────────────────────────────────────────────────────────────┐
	//   │ Spec                  │ AWS RDS             │ Local                 │
	//   ├───────────────────────┼─────────────────────┼───────────────────────┤
	//   │ type: postgres        │ engine: postgres    │ CloudNativePG         │
	//   │ size: small           │ db.t3.medium        │ 512Mi memory          │
	//   │ version: "15"         │ engine_version: 15  │ image: postgres:15    │
	//   │ highAvailability: true│ multi_az: true      │ replicas: 2           │
	//   └───────────────────────┴─────────────────────┴───────────────────────┘
	//
	// ===========================================================================

	// Database configures a managed database for this application.
	// +optional
	Database *DatabaseSpec `json:"database,omitempty"`

	// Cache configures a managed cache (Redis/Memcached) for this application.
	// +optional
	Cache *CacheSpec `json:"cache,omitempty"`

	// Queue configures a managed message queue for this application.
	// +optional
	Queue *QueueSpec `json:"queue,omitempty"`

	// Storage configures object storage (S3/GCS) for this application.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`

	// ===========================================================================
	// OBSERVABILITY - Monitoring configuration
	// ===========================================================================

	// Observability configures metrics, tracing, and logging.
	// +optional
	Observability *ObservabilitySpec `json:"observability,omitempty"`

	// ===========================================================================
	// DEPENDENCIES - What this service depends on
	// ===========================================================================
	//
	// WHY TRACK DEPENDENCIES:
	//   1. Service graph visualization (Backstage catalog)
	//   2. Deployment ordering (don't deploy if dependency down)
	//   3. Impact analysis (if A fails, what's affected?)
	//   4. Preview environments (which services to include)
	//
	// HOW NETFLIX DOES IT:
	//   - Uses Conductor for workflow orchestration
	//   - Dependencies tracked in metadata service
	//
	// ===========================================================================

	// Dependencies lists other Applications this service depends on.
	// Used for service discovery, deployment ordering, and impact analysis.
	// +optional
	Dependencies []DependencySpec `json:"dependencies,omitempty"`
}

// =============================================================================
// WORKLOAD SPECIFICATION
// =============================================================================

// WorkloadSpec defines the container workload configuration.
type WorkloadSpec struct {
	// Image is the container image to run.
	// Examples: ghcr.io/company/app:v1.0.0, nginx:latest
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Replicas is the desired number of pod replicas.
	// This is the initial count; HPA may scale up/down.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources defines CPU and memory requests/limits.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Ports defines the ports the container exposes.
	// +optional
	Ports []ContainerPort `json:"ports,omitempty"`

	// HealthCheck configures liveness and readiness probes.
	// +optional
	HealthCheck *HealthCheckSpec `json:"healthCheck,omitempty"`

	// Env defines environment variables for the container.
	// If InjectCredentials is true (default), infrastructure credentials are
	// appended automatically. User-defined vars take precedence over injected ones.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvFrom allows bulk-injecting environment variables from Secrets or ConfigMaps.
	// Use this for external credentials (Stripe keys, Google API keys, etc.)
	// that are managed outside the platform.
	//
	// The referenced Secrets/ConfigMaps must already exist in the same namespace.
	// They can be created manually, via CI/CD, Sealed Secrets, or
	// External Secrets Operator.
	//
	// HOW IT WORKS:
	//   Every key in the referenced Secret becomes an env var in the container.
	//   For example, a Secret with data {"STRIPE_KEY": "sk_live_..."} injects
	//   STRIPE_KEY=sk_live_... into the pod.
	//
	// COMPARISON WITH Env:
	//   ┌──────────────────────────────────────────────────────────────────────┐
	//   │ Field     │ Use Case                    │ Granularity               │
	//   ├───────────┼─────────────────────────────┼───────────────────────────┤
	//   │ env       │ Individual vars or overrides │ One var at a time         │
	//   │ envFrom   │ Bulk-mount external Secrets  │ Entire Secret/ConfigMap   │
	//   └───────────┴─────────────────────────────┴───────────────────────────┘
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// InjectCredentials controls whether the platform automatically injects
	// environment variables for provisioned infrastructure (database, cache, queue).
	//
	// When true (the default), well-known env vars are injected into the container
	// from the auto-created credential Secrets:
	//   - Database (postgres): DATABASE_URL, PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE
	//   - Database (mysql):    DATABASE_URL, MYSQL_HOST, MYSQL_PORT, MYSQL_USER, MYSQL_PASSWORD, MYSQL_DATABASE
	//   - Cache (redis):       REDIS_URL, REDIS_HOST, REDIS_PORT, REDIS_PASSWORD
	//   - Queue (rabbitmq):    AMQP_URL, RABBITMQ_HOST, RABBITMQ_PORT, RABBITMQ_USER, RABBITMQ_PASSWORD
	//
	// Set to false only if you need full control over how credentials reach your app
	// (e.g., a sidecar reads credentials from a file instead of env vars).
	//
	// WHY AUTO-INJECT:
	//   ┌──────────────────────────────────────────────────────────────────────┐
	//   │ Platform        │ Credential Strategy                               │
	//   ├─────────────────┼───────────────────────────────────────────────────┤
	//   │ Heroku          │ Auto-inject DATABASE_URL into every dyno          │
	//   │ Railway         │ Auto-inject ${{Postgres.DATABASE_URL}}            │
	//   │ Render          │ Auto-inject connection strings for linked DBs     │
	//   │ GoPlatform      │ Auto-inject well-known env vars (this feature)    │
	//   │ Crossplane      │ Manual: user wires Secrets via compositionRef     │
	//   └─────────────────┴───────────────────────────────────────────────────┘
	//
	// +kubebuilder:default=true
	// +optional
	InjectCredentials *bool `json:"injectCredentials,omitempty"`

	// Command overrides the container entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args provides arguments to the command.
	// +optional
	Args []string `json:"args,omitempty"`
}

// ContainerPort defines a port exposed by the container.
type ContainerPort struct {
	// Name is the port name (e.g., "http", "grpc", "metrics").
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// ContainerPort is the port number on the container.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	ContainerPort int32 `json:"containerPort"`

	// Protocol is the network protocol (TCP/UDP).
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// HealthCheckSpec defines health check configuration.
type HealthCheckSpec struct {
	// Path is the HTTP path for health checks (e.g., "/health").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/.*$`
	Path string `json:"path"`

	// Port is the port to check.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// InitialDelaySeconds is seconds before first probe.
	// +kubebuilder:default=10
	// +optional
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`

	// PeriodSeconds is how often to probe.
	// +kubebuilder:default=10
	// +optional
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`

	// FailureThreshold is consecutive failures to mark unhealthy.
	// +kubebuilder:default=3
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// =============================================================================
// SCALING SPECIFICATION
// =============================================================================

// ScalingSpec defines auto-scaling configuration.
type ScalingSpec struct {
	// MinReplicas is the minimum number of replicas.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the maximum number of replicas.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// Metrics defines the scaling metrics to use.
	// +optional
	Metrics []ScalingMetric `json:"metrics,omitempty"`
}

// ScalingMetric defines a metric for autoscaling.
type ScalingMetric struct {
	// Type is the metric type (cpu, memory, custom).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=cpu;memory;custom
	Type string `json:"type"`

	// Target is the target value (percentage for cpu/memory).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Target int32 `json:"target"`
}

// =============================================================================
// INFRASTRUCTURE SPECIFICATIONS (CLOUD-AGNOSTIC)
// =============================================================================

// DatabaseSpec defines a managed database requirement.
type DatabaseSpec struct {
	// Type is the database engine (postgres, mysql).
	// +kubebuilder:validation:Required
	Type DatabaseType `json:"type"`

	// Size is the resource size abstraction.
	// The platform maps this to provider-specific instance types.
	// +kubebuilder:default=small
	// +optional
	Size ResourceSize `json:"size,omitempty"`

	// Version is the major version (e.g., "15" for PostgreSQL 15).
	// Minor/patch versions are managed by the platform.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+$`
	Version string `json:"version"`

	// HighAvailability enables multi-AZ/replica deployment.
	// Critical tier applications should always enable this.
	// +kubebuilder:default=false
	// +optional
	HighAvailability bool `json:"highAvailability,omitempty"`

	// Backup configures automated backup settings.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`
}

// BackupSpec defines backup configuration.
type BackupSpec struct {
	// Enabled turns on automated backups.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// RetentionDays is how long to keep backups.
	// +kubebuilder:default=7
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=35
	// +optional
	RetentionDays *int32 `json:"retentionDays,omitempty"`

	// Window is the preferred backup window (e.g., "03:00-04:00").
	// +kubebuilder:validation:Pattern=`^([01]?[0-9]|2[0-3]):[0-5][0-9]-([01]?[0-9]|2[0-3]):[0-5][0-9]$`
	// +optional
	Window string `json:"window,omitempty"`
}

// CacheSpec defines a managed cache requirement.
type CacheSpec struct {
	// Type is the cache engine (redis, memcached).
	// +kubebuilder:validation:Required
	Type CacheType `json:"type"`

	// Size is the resource size abstraction.
	// +kubebuilder:default=small
	// +optional
	Size ResourceSize `json:"size,omitempty"`

	// HighAvailability enables replication.
	// +kubebuilder:default=false
	// +optional
	HighAvailability bool `json:"highAvailability,omitempty"`
}

// QueueSpec defines a managed message queue requirement.
type QueueSpec struct {
	// Type is the queue system (sqs, rabbitmq, kafka).
	// +kubebuilder:validation:Required
	Type QueueType `json:"type"`

	// FIFO enables exactly-once, ordered delivery.
	// Only supported by some queue types (SQS FIFO, Kafka).
	// +kubebuilder:default=false
	// +optional
	FIFO bool `json:"fifo,omitempty"`

	// DeadLetterQueue configures failed message handling.
	// +optional
	DeadLetterQueue *DeadLetterQueueSpec `json:"deadLetterQueue,omitempty"`
}

// DeadLetterQueueSpec defines DLQ configuration.
type DeadLetterQueueSpec struct {
	// Enabled turns on dead letter queue.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// MaxReceiveCount is how many times to retry before DLQ.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxReceiveCount *int32 `json:"maxReceiveCount,omitempty"`
}

// StorageSpec defines object storage requirement.
type StorageSpec struct {
	// Type is the storage system (s3, gcs).
	// +kubebuilder:validation:Required
	Type StorageType `json:"type"`

	// Versioning enables object versioning.
	// +kubebuilder:default=false
	// +optional
	Versioning bool `json:"versioning,omitempty"`

	// Encryption enables server-side encryption.
	// +kubebuilder:default=true
	// +optional
	Encryption *bool `json:"encryption,omitempty"`
}

// =============================================================================
// OBSERVABILITY SPECIFICATION
// =============================================================================

// ObservabilitySpec defines monitoring configuration.
type ObservabilitySpec struct {
	// Metrics configures Prometheus metrics scraping.
	// +optional
	Metrics *MetricsSpec `json:"metrics,omitempty"`

	// Tracing configures distributed tracing.
	// +optional
	Tracing *TracingSpec `json:"tracing,omitempty"`

	// Logging configures log collection.
	// +optional
	Logging *LoggingSpec `json:"logging,omitempty"`
}

// MetricsSpec defines Prometheus metrics configuration.
type MetricsSpec struct {
	// Enabled turns on metrics collection.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Path is the metrics endpoint (e.g., "/metrics").
	// +kubebuilder:default="/metrics"
	// +optional
	Path string `json:"path,omitempty"`

	// Port is the metrics port.
	// +kubebuilder:default=9090
	// +optional
	Port int32 `json:"port,omitempty"`
}

// TracingSpec defines distributed tracing configuration.
type TracingSpec struct {
	// Enabled turns on distributed tracing.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// SampleRate is the percentage of requests to trace (0.0-1.0).
	// +kubebuilder:default="0.1"
	// +optional
	SampleRate resource.Quantity `json:"sampleRate,omitempty"`
}

// LoggingSpec defines log configuration.
type LoggingSpec struct {
	// Format is the log output format.
	// +kubebuilder:default=json
	// +optional
	Format LogFormat `json:"format,omitempty"`
}

// =============================================================================
// DEPENDENCY SPECIFICATION
// =============================================================================

// DependencySpec defines a service dependency.
type DependencySpec struct {
	// Name is the name of the dependent Application.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the dependent Application.
	// Defaults to the same namespace as this Application.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Required indicates if this dependency must be healthy.
	// If true, the application won't start if dependency is down.
	// +kubebuilder:default=false
	// +optional
	Required bool `json:"required,omitempty"`
}

// =============================================================================
// APPLICATION STATUS - OBSERVED STATE
// =============================================================================
//
// Status is what the controller reports - the CURRENT state.
// Users NEVER write to status; only controllers do.
//
// KUBERNETES API CONVENTION:
//   - Status is a separate subresource (+kubebuilder:subresource:status)
//   - Controller updates status without triggering CRD update reconcile
//   - observedGeneration tracks which spec version status reflects
//
// WHY SUBRESOURCE:
//   Without subresource:
//     1. User updates spec → triggers reconcile
//     2. Controller updates status → triggers ANOTHER reconcile
//     3. Infinite loop!
//
//   With subresource:
//     1. PATCH /api/.../applications/foo/status doesn't trigger spec reconcile
//     2. Clean separation of concerns
//
// =============================================================================

// ApplicationStatus defines the observed state of Application.
type ApplicationStatus struct {
	// Phase is the high-level summary of application state.
	// +optional
	Phase ApplicationPhase `json:"phase,omitempty"`

	// ObservedGeneration is the spec generation last processed.
	// If this doesn't match metadata.generation, controller is still processing.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions provide detailed status information.
	// Standard K8s conditions pattern - each condition type has status True/False/Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Infrastructure contains endpoints and references for provisioned resources.
	// Populated after successful provisioning.
	// +optional
	Infrastructure *InfrastructureStatus `json:"infrastructure,omitempty"`

	// EstimatedMonthlyCost shows the estimated monthly cost.
	// Updated during provisioning based on selected resources.
	// +optional
	EstimatedMonthlyCost *CostEstimate `json:"estimatedMonthlyCost,omitempty"`
}

// InfrastructureStatus contains provisioned infrastructure details.
type InfrastructureStatus struct {
	// Database contains database connection information.
	// +optional
	Database *DatabaseStatus `json:"database,omitempty"`

	// Cache contains cache connection information.
	// +optional
	Cache *CacheStatus `json:"cache,omitempty"`

	// Queue contains queue URLs and ARNs.
	// +optional
	Queue *QueueStatus `json:"queue,omitempty"`

	// Storage contains bucket information.
	// +optional
	Storage *StorageStatus `json:"storage,omitempty"`
}

// DatabaseStatus contains database endpoint and credentials reference.
type DatabaseStatus struct {
	// Endpoint is the database host (e.g., mydb.xxx.rds.amazonaws.com).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Port is the database port.
	// +optional
	Port int32 `json:"port,omitempty"`

	// SecretRef references the Secret containing credentials.
	// Secret contains: username, password, database, host, port, and connection_url.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// CacheStatus contains cache endpoint information.
type CacheStatus struct {
	// Endpoint is the cache host.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Port is the cache port.
	// +optional
	Port int32 `json:"port,omitempty"`
}

// QueueStatus contains queue endpoint information.
type QueueStatus struct {
	// URL is the queue URL for sending messages.
	// +optional
	URL string `json:"url,omitempty"`

	// ARN is the queue ARN (for AWS SQS).
	// +optional
	ARN string `json:"arn,omitempty"`

	// DeadLetterQueueURL is the DLQ URL if enabled.
	// +optional
	DeadLetterQueueURL string `json:"deadLetterQueueUrl,omitempty"`
}

// StorageStatus contains bucket information.
type StorageStatus struct {
	// BucketName is the bucket name.
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// Region is the bucket region.
	// +optional
	Region string `json:"region,omitempty"`
}

// CostEstimate shows estimated costs.
type CostEstimate struct {
	// Amount is the total estimated monthly cost.
	// +optional
	Amount string `json:"amount,omitempty"`

	// Currency is the currency code (USD, EUR, etc.).
	// +kubebuilder:default=USD
	// +optional
	Currency string `json:"currency,omitempty"`

	// Breakdown shows per-resource costs.
	// +optional
	Breakdown map[string]string `json:"breakdown,omitempty"`
}

// =============================================================================
// APPLICATION - THE ROOT TYPE
// =============================================================================
//
// This is what users actually create:
//
//   apiVersion: platform.goplatform.io/v1alpha1
//   kind: Application
//   metadata:
//     name: payments-api
//   spec:
//     team: payments
//     ...
//
// MARKERS EXPLAINED:
//   +kubebuilder:object:root=true
//     → This type is the root GVK (Group-Version-Kind)
//     → Generates scheme registration code
//
//   +kubebuilder:subresource:status
//     → Status is separate subresource (/status endpoint)
//     → Controllers can update status without triggering spec reconcile
//
//   +kubebuilder:printcolumn:...
//     → Columns shown in `kubectl get applications` output
//     → JSONPath expressions point to fields
//
// =============================================================================

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Team",type=string,JSONPath=`.spec.team`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Application is the Schema for the applications API.
// It represents a complete application with its workload and infrastructure.
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state of the Application.
	// +kubebuilder:validation:Required
	Spec ApplicationSpec `json:"spec"`

	// Status defines the observed state of the Application.
	// +optional
	Status ApplicationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ApplicationList contains a list of Application.
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Application `json:"items"`
}

// =============================================================================
// SCHEME REGISTRATION
// =============================================================================
//
// This init() function runs when the package is imported.
// It registers our types with the Kubernetes scheme so the client knows
// how to serialize/deserialize Application objects.
//
// =============================================================================

func init() {
	SchemeBuilder.Register(&Application{}, &ApplicationList{})
}

// =============================================================================
// CONDITION TYPES (CONSTANTS)
// =============================================================================
//
// These are the condition types we'll report in Status.Conditions.
// Each represents a specific aspect of the application's state.
//
// WHY USE CONDITIONS:
//   A simple "Ready: true/false" doesn't tell you WHY it's not ready.
//   Conditions provide granular status:
//     - WorkloadReady: is the Deployment healthy?
//     - DatabaseReady: is the database provisioned?
//     - CacheReady: is the cache provisioned?
//
// HOW OTHER OPERATORS DO IT:
//   - Cert-Manager: Ready, Issuing conditions
//   - ArgoCD: Healthy, Synced, Progressing conditions
//   - Crossplane: Ready, Synced conditions
//
// =============================================================================

const (
	// ConditionTypeReady indicates overall readiness.
	ConditionTypeReady = "Ready"
	// ConditionTypeWorkloadReady indicates workload (Deployment) is healthy.
	ConditionTypeWorkloadReady = "WorkloadReady"
	// ConditionTypeDatabaseReady indicates database is provisioned.
	ConditionTypeDatabaseReady = "DatabaseReady"
	// ConditionTypeCacheReady indicates cache is provisioned.
	ConditionTypeCacheReady = "CacheReady"
	// ConditionTypeQueueReady indicates queue is provisioned.
	ConditionTypeQueueReady = "QueueReady"
	// ConditionTypeStorageReady indicates storage is provisioned.
	ConditionTypeStorageReady = "StorageReady"
	// ConditionTypeMonitoringReady indicates monitoring resources (ServiceMonitor, PrometheusRule) are configured.
	ConditionTypeMonitoringReady = "MonitoringReady"
	// ConditionTypeDriftDetected indicates external drift was found this reconcile.
	// It is True on a pass where a child resource was corrected (or infrastructure
	// drift was detected) without a corresponding spec change, and False once the
	// observed state matches desired state again.
	ConditionTypeDriftDetected = "DriftDetected"
)
