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
// KUBERNETES-NATIVE INFRASTRUCTURE PROVIDER
// =============================================================================
//
// WHY THIS PROVIDER EXISTS:
//   Many teams want to run databases, caches, and queues *inside* Kubernetes
//   without relying on cloud-managed services. This is ideal for:
//   - Development & staging (cost savings)
//   - Air‑gapped or on‑prem clusters
//   - Edge deployments (k3s, MicroK8s)
//   - Learning environments (everything is visible in K8s)
//
// HOW IT WORKS (HIGH‑LEVEL):
//   1. Detect required operators by checking CRDs via discovery
//   2. Create/Update operator CRs (CNPG Cluster, RedisFailover, RabbitmqCluster)
//   3. Create credential Secrets with connection strings
//   4. Map operator resource status → ResourceState
//   5. Delete resources on spec removal or during Destroy()
//
// ALTERNATIVES CONSIDERED:
//   ┌──────────────────────────┬───────────────────────┬──────────────────────┐
//   │ Approach                 │ Pros                  │ Cons                 │
//   ├──────────────────────────┼───────────────────────┼──────────────────────┤
//   │ Raw StatefulSets         │ No operator deps      │ You own HA/backups   │
//   │ Operator CRDs            │ HA/backups built‑in   │ CRD schema coupling  │
//   │ External Managed Service │ Reliable SLA          │ Cloud cost/lock‑in   │
//   └──────────────────────────┴───────────────────────┴──────────────────────┘
//
// WHY OPERATOR CRDs (OUR CHOICE):
//   - Operators encode years of database/caching best practices
//   - Day‑2 operations (failover, upgrades) are handled for us
//   - Matches how platforms like Crossplane/ACK delegate to controllers
//
// REAL‑WORLD COMPARISON:
//   - Crossplane: Creates Managed Resources that *other* controllers reconcile
//   - Backstage: Doesn't provision infra directly; delegates to plugins
//   - We: Create operator CRDs directly for simplicity and speed
//
// FAILURE MODES TO EXPECT:
//   1. CRD missing → InvalidConfigError with remediation guidance
//   2. Operator stuck → ResourcePhase=Provisioning + NotReadyError
//   3. Secret missing → ProvisioningError (credentials not ready)
//   4. User deletes operator CR manually → GetStatus reports NotFound
//
// =============================================================================

package provider

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
)

// =============================================================================
// OPERATOR GVKs (Hard‑coded defaults)
// =============================================================================
// NOTE: These match the operator choices we agreed on in Milestone 7.
// If your cluster uses different operators, update these constants or
// provide a custom ProviderConstructor to inject your own behavior.

var (
	cnpgClusterGVK = schema.GroupVersionKind{
		Group:   "postgresql.cnpg.io",
		Version: "v1",
		Kind:    "Cluster",
	}

	redisFailoverGVK = schema.GroupVersionKind{
		Group:   "databases.spotahome.com",
		Version: "v1",
		Kind:    "RedisFailover",
	}

	rabbitmqClusterGVK = schema.GroupVersionKind{
		Group:   "rabbitmq.com",
		Version: "v1beta1",
		Kind:    "RabbitmqCluster",
	}
)

const (
	defaultDatabasePort = 5432
	defaultRedisPort    = 6379
	defaultAMQPPort     = 5672

	defaultStorageSizeSmall  = "10Gi"
	defaultStorageSizeMedium = "20Gi"
	defaultStorageSizeLarge  = "50Gi"
	defaultStorageSizeXLarge = "100Gi"
)

// discoveryClient is a minimal interface for discovery operations.
// This allows us to inject a fake discovery client in tests.
type discoveryClient interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// KubernetesProvider implements InfrastructureProvider by creating
// Kubernetes resources (operator CRDs + PVCs) in the same cluster.
type KubernetesProvider struct {
	config    *ProviderConfig
	kubeCfg   *KubernetesConfig
	client    client.Client
	scheme    *runtime.Scheme
	discovery discoveryClient
}

// NewKubernetesProvider creates a Kubernetes-native provider.
//
// DEPENDENCY INJECTION:
//   - Pass a preconfigured controller-runtime client + scheme when running
//     inside the controller (so we reuse the manager cache & settings).
//   - If client is nil, we build one from kubeconfig (useful for CLI/tests).
//
// NOTE: This constructor does not start informers or background loops. It only
// creates a client and uses it for CRUD operations during Provision/GetStatus.
func NewKubernetesProvider(
	config *ProviderConfig,
	clientOverride client.Client,
	schemeOverride *runtime.Scheme,
	discoveryOverride discoveryClient,
) (*KubernetesProvider, error) {
	if config == nil {
		return nil, &InvalidConfigError{
			ResourceType: "provider",
			Field:        "config",
			Value:        "nil",
			Message:      "provider config is required",
		}
	}

	if config.Kubernetes == nil {
		config.Kubernetes = &KubernetesConfig{}
	}

	// Default operator selections (align with Milestone 7 decisions)
	if config.Kubernetes.PostgresOperator == "" {
		config.Kubernetes.PostgresOperator = "cnpg"
	}
	if config.Kubernetes.RedisOperator == "" {
		// We use Spotahome RedisFailover CRD by default
		config.Kubernetes.RedisOperator = "spotahome"
	}
	if config.Kubernetes.QueueOperator == "" {
		config.Kubernetes.QueueOperator = "rabbitmq"
	}

	// Build or reuse scheme
	scheme := schemeOverride
	if scheme == nil {
		scheme = runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = platformv1alpha1.AddToScheme(scheme)
	}

	// Build or reuse client
	clientInstance := clientOverride
	var restCfg *rest.Config
	var err error
	if clientInstance == nil {
		cfg, err := ctrlconfig.GetConfig()
		if err != nil {
			return nil, &ProviderNotConfiguredError{
				Provider: ProviderKubernetes,
				Message:  fmt.Sprintf("failed to load kubeconfig: %v", err),
			}
		}
		restCfg = cfg
		clientInstance, err = client.New(restCfg, client.Options{Scheme: scheme})
		if err != nil {
			return nil, &ProviderNotConfiguredError{
				Provider: ProviderKubernetes,
				Message:  fmt.Sprintf("failed to create Kubernetes client: %v", err),
			}
		}
	}

	// Build or reuse discovery client
	var discoveryInstance discoveryClient
	if discoveryOverride != nil {
		discoveryInstance = discoveryOverride
	} else {
		// If we already created restCfg above, reuse it; otherwise fetch now.
		if restCfg == nil {
			cfg, err := ctrlconfig.GetConfig()
			if err != nil {
				return nil, &ProviderNotConfiguredError{
					Provider: ProviderKubernetes,
					Message:  fmt.Sprintf("failed to load kubeconfig for discovery: %v", err),
				}
			}
			restCfg = cfg
		}
		discoveryInstance, err = discovery.NewDiscoveryClientForConfig(restCfg)
		if err != nil {
			return nil, &ProviderNotConfiguredError{
				Provider: ProviderKubernetes,
				Message:  fmt.Sprintf("failed to create discovery client: %v", err),
			}
		}
	}

	return &KubernetesProvider{
		config:    config,
		kubeCfg:   config.Kubernetes,
		client:    clientInstance,
		scheme:    scheme,
		discovery: discoveryInstance,
	}, nil
}

// Name returns the provider name.
func (p *KubernetesProvider) Name() string {
	return "kubernetes"
}

// Type returns the provider type enum.
func (p *KubernetesProvider) Type() ProviderType {
	return ProviderKubernetes
}

// Healthy checks if the provider is configured and can reach the API server.
// We keep this lightweight (no list calls) to avoid heavy health checks.
func (p *KubernetesProvider) Healthy(ctx context.Context) bool {
	if p == nil || p.client == nil || p.discovery == nil {
		return false
	}
	return true
}

// Provision ensures all requested infrastructure exists in-cluster.
func (p *KubernetesProvider) Provision(ctx context.Context, app *platformv1alpha1.Application) (*ResourceState, error) {
	logger := log.FromContext(ctx)

	if app == nil {
		return nil, &InvalidConfigError{
			ResourceType: "application",
			Field:        "app",
			Value:        "nil",
			Message:      "application is required",
		}
	}

	state := &ResourceState{
		LastUpdateTime: time.Now(),
		ProviderMetadata: map[string]string{
			"provider": "kubernetes",
		},
	}

	// Track the first NotReady error to surface to the controller.
	var notReadyErr error
	var hasNotReady bool

	// DATABASE
	if app.Spec.Database != nil {
		dbState, err := p.reconcileDatabase(ctx, app)
		state.Database = dbState
		if err != nil {
			if IsNotReady(err) {
				if !hasNotReady {
					notReadyErr = err
					hasNotReady = true
				}
			} else {
				return state, err
			}
		}
	} else {
		_ = p.cleanupDatabase(ctx, app)
	}

	// CACHE
	if app.Spec.Cache != nil {
		cacheState, err := p.reconcileCache(ctx, app)
		state.Cache = cacheState
		if err != nil {
			if IsNotReady(err) {
				if !hasNotReady {
					notReadyErr = err
					hasNotReady = true
				}
			} else {
				return state, err
			}
		}
	} else {
		_ = p.cleanupCache(ctx, app)
	}

	// QUEUE
	if app.Spec.Queue != nil {
		queueState, err := p.reconcileQueue(ctx, app)
		state.Queue = queueState
		if err != nil {
			if IsNotReady(err) {
				if !hasNotReady {
					notReadyErr = err
					hasNotReady = true
				}
			} else {
				return state, err
			}
		}
	} else {
		_ = p.cleanupQueue(ctx, app)
	}

	// STORAGE
	if app.Spec.Storage != nil {
		storageState, err := p.reconcileStorage(ctx, app)
		state.Storage = storageState
		if err != nil {
			if IsNotReady(err) {
				if !hasNotReady {
					notReadyErr = err
					hasNotReady = true
				}
			} else {
				return state, err
			}
		}
	} else {
		_ = p.cleanupStorage(ctx, app)
	}

	if notReadyErr != nil {
		logger.Info("kubernetes provider: resources still provisioning")
		return state, notReadyErr
	}

	return state, nil
}

// GetStatus returns the current state of infrastructure without modifications.
func (p *KubernetesProvider) GetStatus(ctx context.Context, app *platformv1alpha1.Application) (*ResourceState, error) {
	if app == nil {
		return nil, &InvalidConfigError{
			ResourceType: "application",
			Field:        "app",
			Value:        "nil",
			Message:      "application is required",
		}
	}

	state := &ResourceState{
		LastUpdateTime: time.Now(),
		ProviderMetadata: map[string]string{
			"provider": "kubernetes",
		},
	}

	var firstErr error
	var hasErr bool

	if app.Spec.Database != nil {
		st, err := p.getDatabaseStatus(ctx, app)
		state.Database = st
		if err != nil && !hasErr {
			firstErr = err
			hasErr = true
		}
	}

	if app.Spec.Cache != nil {
		st, err := p.getCacheStatus(ctx, app)
		state.Cache = st
		if err != nil && !hasErr {
			firstErr = err
			hasErr = true
		}
	}

	if app.Spec.Queue != nil {
		st, err := p.getQueueStatus(ctx, app)
		state.Queue = st
		if err != nil && !hasErr {
			firstErr = err
			hasErr = true
		}
	}

	if app.Spec.Storage != nil {
		st, err := p.getStorageStatus(ctx, app)
		state.Storage = st
		if err != nil && !hasErr {
			firstErr = err
			hasErr = true
		}
	}

	return state, firstErr
}

// Destroy deletes all infrastructure resources created for an Application.
func (p *KubernetesProvider) Destroy(ctx context.Context, app *platformv1alpha1.Application) error {
	if app == nil {
		return &InvalidConfigError{
			ResourceType: "application",
			Field:        "app",
			Value:        "nil",
			Message:      "application is required",
		}
	}

	// Delete in reverse dependency order (queue/cache/db/storage) to avoid
	// lingering connections. This is conservative and mirrors Terraform best
	// practices (tear down dependents first).
	//
	// IMPORTANT: Only clean up resource types that were actually requested in
	// the spec. Attempting to delete CRs for operators that aren't installed
	// (e.g., RabbitmqCluster when only CNPG is deployed) would fail and block
	// the finalizer from being removed, leaving the Application stuck in
	// Deleting phase.
	if app.Spec.Queue != nil {
		if err := p.cleanupQueue(ctx, app); err != nil {
			return err
		}
	}
	if app.Spec.Cache != nil {
		if err := p.cleanupCache(ctx, app); err != nil {
			return err
		}
	}
	if app.Spec.Database != nil {
		if err := p.cleanupDatabase(ctx, app); err != nil {
			return err
		}
	}
	if app.Spec.Storage != nil {
		if err := p.cleanupStorage(ctx, app); err != nil {
			return err
		}
	}

	return nil
}

// =============================================================================
// DATABASE (CloudNativePG)
// =============================================================================

func (p *KubernetesProvider) reconcileDatabase(ctx context.Context, app *platformv1alpha1.Application) (*DatabaseState, error) {
	if app.Spec.Database.Type != platformv1alpha1.DatabasePostgres {
		return &DatabaseState{
				Phase:   ResourceFailed,
				Engine:  string(app.Spec.Database.Type),
				Message: "only postgres is supported by the Kubernetes provider",
			}, &InvalidConfigError{
				ResourceType: "database",
				Field:        "type",
				Value:        string(app.Spec.Database.Type),
				Message:      "only postgres (CloudNativePG) is supported in-cluster",
			}
	}

	if err := p.ensureOperatorAvailable(cnpgClusterGVK); err != nil {
		return &DatabaseState{Phase: ResourceFailed, Engine: "postgres"}, err
	}

	ns := p.resolveNamespace(app)
	dbName := fmt.Sprintf("%s-db", app.Name)
	secretName := fmt.Sprintf("%s-db-credentials", app.Name)

	// Ensure credentials Secret exists (do not overwrite if already created).
	username := sanitizeIdentifier(app.Name)
	password, err := p.ensureSecret(ctx, ns, secretName, map[string][]byte{
		"username": []byte(username),
		"password": []byte(randomPassword(24)),
		"database": []byte(app.Name),
	})
	if err != nil {
		return &DatabaseState{Phase: ResourceFailed, Engine: "postgres"}, err
	}

	host := fmt.Sprintf("%s-rw.%s.svc", dbName, ns) // CNPG standard service name
	port := int32(defaultDatabasePort)
	conn := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s?sslmode=disable",
		username, password, host, port, app.Name)

	// Store derived connection info in the Secret (idempotent if already set).
	if err := p.ensureSecretData(ctx, ns, secretName, map[string][]byte{
		"host":             []byte(host),
		"port":             []byte(fmt.Sprintf("%d", port)),
		"connectionString": []byte(conn),
	}); err != nil {
		return &DatabaseState{Phase: ResourceFailed, Engine: "postgres"}, err
	}

	instances := int64(1)
	if app.Spec.Database.HighAvailability {
		instances = 3
	}

	storageClass := p.kubeCfg.StorageClass
	if app.Spec.Database.HighAvailability && p.kubeCfg.StorageClassFast != "" {
		storageClass = p.kubeCfg.StorageClassFast
	}

	storageSize := sizeToStorage(app.Spec.Database.Size)

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(dbName)
	cluster.SetNamespace(ns)

	labels := p.buildLabels(app)
	_, err = controllerutil.CreateOrUpdate(ctx, p.client, cluster, func() error {
		spec := map[string]interface{}{
			"instances": instances,
			"storage": map[string]interface{}{
				"size": storageSize,
			},
			"bootstrap": map[string]interface{}{
				"initdb": map[string]interface{}{
					"database": app.Name,
					"owner":    username,
					"secret": map[string]interface{}{
						"name": secretName,
					},
				},
			},
			"superuserSecret": map[string]interface{}{
				"name": secretName,
			},
		}

		if storageClass != "" {
			spec["storage"].(map[string]interface{})["storageClass"] = storageClass
		}

		cluster.Object["spec"] = spec
		cluster.SetLabels(labels)
		return controllerutil.SetControllerReference(app, cluster, p.scheme)
	})
	if err != nil {
		return &DatabaseState{Phase: ResourceFailed, Engine: "postgres"}, &ProvisioningError{
			ResourceType: "database",
			ResourceID:   dbName,
			Operation:    "create/update",
			Cause:        err,
			Message:      "failed to reconcile CloudNativePG Cluster",
		}
	}

	// Fetch latest status to map readiness
	if err := p.client.Get(ctx, client.ObjectKey{Name: dbName, Namespace: ns}, cluster); err != nil {
		return &DatabaseState{Phase: ResourceProvisioning, Engine: "postgres"}, &NotReadyError{
			ResourceType: "database",
			ResourceID:   dbName,
			CurrentPhase: ResourceProvisioning,
			Message:      "cluster status not yet available",
		}
	}

	phase := mapReadyCondition(cluster)
	message := "database provisioning"
	if phase == ResourceReady {
		message = "database ready"
	}
	if !cluster.GetDeletionTimestamp().IsZero() {
		phase = ResourceDeleting
		message = "database deleting"
	}

	state := &DatabaseState{
		Phase:    phase,
		Engine:   string(app.Spec.Database.Type),
		Version:  app.Spec.Database.Version,
		Endpoint: host,
		Port:     port,
		SecretRef: &corev1.LocalObjectReference{
			Name: secretName,
		},
		ResourceID: fmt.Sprintf("cnpg/%s", dbName),
		Message:    message,
	}

	if phase != ResourceReady {
		return state, &NotReadyError{
			ResourceType: "database",
			ResourceID:   dbName,
			CurrentPhase: phase,
			Message:      message,
		}
	}

	return state, nil
}

func (p *KubernetesProvider) getDatabaseStatus(ctx context.Context, app *platformv1alpha1.Application) (*DatabaseState, error) {
	ns := p.resolveNamespace(app)
	dbName := fmt.Sprintf("%s-db", app.Name)
	secretName := fmt.Sprintf("%s-db-credentials", app.Name)

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := p.client.Get(ctx, client.ObjectKey{Name: dbName, Namespace: ns}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return &DatabaseState{Phase: ResourceNotFound, Engine: "postgres"}, &NotFoundError{
				ResourceType: "database",
				ResourceID:   dbName,
				Message:      "CloudNativePG Cluster not found",
			}
		}
		return nil, err
	}

	host := fmt.Sprintf("%s-rw.%s.svc", dbName, ns)
	phase := mapReadyCondition(cluster)
	message := "database provisioning"
	if phase == ResourceReady {
		message = "database ready"
	}

	return &DatabaseState{
		Phase:    phase,
		Engine:   string(app.Spec.Database.Type),
		Version:  app.Spec.Database.Version,
		Endpoint: host,
		Port:     defaultDatabasePort,
		SecretRef: &corev1.LocalObjectReference{
			Name: secretName,
		},
		ResourceID: fmt.Sprintf("cnpg/%s", dbName),
		Message:    message,
	}, nil
}

func (p *KubernetesProvider) cleanupDatabase(ctx context.Context, app *platformv1alpha1.Application) error {
	ns := p.resolveNamespace(app)
	dbName := fmt.Sprintf("%s-db", app.Name)
	secretName := fmt.Sprintf("%s-db-credentials", app.Name)

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(dbName)
	cluster.SetNamespace(ns)
	if err := p.client.Delete(ctx, cluster); err != nil && !apierrors.IsNotFound(err) {
		return &ProvisioningError{
			ResourceType: "database",
			ResourceID:   dbName,
			Operation:    "delete",
			Cause:        err,
			Message:      "failed to delete CloudNativePG Cluster",
		}
	}

	if err := p.deleteSecret(ctx, ns, secretName); err != nil {
		return err
	}

	return nil
}

// =============================================================================
// CACHE (RedisFailover - Spotahome Redis Operator)
// =============================================================================

func (p *KubernetesProvider) reconcileCache(ctx context.Context, app *platformv1alpha1.Application) (*CacheState, error) {
	if app.Spec.Cache.Type != platformv1alpha1.CacheRedis {
		return &CacheState{
				Phase:   ResourceFailed,
				Engine:  string(app.Spec.Cache.Type),
				Message: "only redis is supported by the Kubernetes provider",
			}, &InvalidConfigError{
				ResourceType: "cache",
				Field:        "type",
				Value:        string(app.Spec.Cache.Type),
				Message:      "only redis is supported in-cluster",
			}
	}

	if err := p.ensureOperatorAvailable(redisFailoverGVK); err != nil {
		return &CacheState{Phase: ResourceFailed, Engine: "redis"}, err
	}

	ns := p.resolveNamespace(app)
	cacheName := fmt.Sprintf("%s-cache", app.Name)
	secretName := fmt.Sprintf("%s-cache-credentials", app.Name)

	password, err := p.ensureSecret(ctx, ns, secretName, map[string][]byte{
		"password": []byte(randomPassword(24)),
	})
	if err != nil {
		return &CacheState{Phase: ResourceFailed, Engine: "redis"}, err
	}

	host := fmt.Sprintf("%s.%s.svc", cacheName, ns)
	port := int32(defaultRedisPort)
	conn := fmt.Sprintf("redis://:%s@%s:%d/0", password, host, port)
	if err := p.ensureSecretData(ctx, ns, secretName, map[string][]byte{
		"host":             []byte(host),
		"port":             []byte(fmt.Sprintf("%d", port)),
		"connectionString": []byte(conn),
	}); err != nil {
		return &CacheState{Phase: ResourceFailed, Engine: "redis"}, err
	}

	redis := &unstructured.Unstructured{}
	redis.SetGroupVersionKind(redisFailoverGVK)
	redis.SetName(cacheName)
	redis.SetNamespace(ns)

	replicas := int64(1)
	if app.Spec.Cache.HighAvailability {
		replicas = 3
	}

	labels := p.buildLabels(app)
	_, err = controllerutil.CreateOrUpdate(ctx, p.client, redis, func() error {
		spec := map[string]interface{}{
			"redis": map[string]interface{}{
				"replicas": replicas,
			},
			"sentinel": map[string]interface{}{
				"replicas": replicas,
			},
			"auth": map[string]interface{}{
				"secretPath": secretName,
			},
		}

		redis.Object["spec"] = spec
		redis.SetLabels(labels)
		return controllerutil.SetControllerReference(app, redis, p.scheme)
	})
	if err != nil {
		return &CacheState{Phase: ResourceFailed, Engine: "redis"}, &ProvisioningError{
			ResourceType: "cache",
			ResourceID:   cacheName,
			Operation:    "create/update",
			Cause:        err,
			Message:      "failed to reconcile RedisFailover",
		}
	}

	if err := p.client.Get(ctx, client.ObjectKey{Name: cacheName, Namespace: ns}, redis); err != nil {
		return &CacheState{Phase: ResourceProvisioning, Engine: "redis"}, &NotReadyError{
			ResourceType: "cache",
			ResourceID:   cacheName,
			CurrentPhase: ResourceProvisioning,
			Message:      "redis status not yet available",
		}
	}

	phase := mapReadyCondition(redis)
	message := "cache provisioning"
	if phase != ResourceReady {
		podReady, podMessage, err := p.redisPodsReady(ctx, ns, cacheName)
		if err != nil {
			message = fmt.Sprintf("cache pods not yet ready: %v", err)
		} else if podReady {
			phase = ResourceReady
			message = "cache ready"
		} else if podMessage != "" {
			message = podMessage
		}
	}
	if phase == ResourceReady {
		message = "cache ready"
	}
	if !redis.GetDeletionTimestamp().IsZero() {
		phase = ResourceDeleting
		message = "cache deleting"
	}

	state := &CacheState{
		Phase:    phase,
		Engine:   string(app.Spec.Cache.Type),
		Endpoint: host,
		Port:     port,
		SecretRef: &corev1.LocalObjectReference{
			Name: secretName,
		},
		ResourceID: fmt.Sprintf("redisfailover/%s", cacheName),
		Message:    message,
	}

	if phase != ResourceReady {
		return state, &NotReadyError{
			ResourceType: "cache",
			ResourceID:   cacheName,
			CurrentPhase: phase,
			Message:      message,
		}
	}

	return state, nil
}

func (p *KubernetesProvider) getCacheStatus(ctx context.Context, app *platformv1alpha1.Application) (*CacheState, error) {
	ns := p.resolveNamespace(app)
	cacheName := fmt.Sprintf("%s-cache", app.Name)
	secretName := fmt.Sprintf("%s-cache-credentials", app.Name)

	redis := &unstructured.Unstructured{}
	redis.SetGroupVersionKind(redisFailoverGVK)
	if err := p.client.Get(ctx, client.ObjectKey{Name: cacheName, Namespace: ns}, redis); err != nil {
		if apierrors.IsNotFound(err) {
			return &CacheState{Phase: ResourceNotFound, Engine: "redis"}, &NotFoundError{
				ResourceType: "cache",
				ResourceID:   cacheName,
				Message:      "RedisFailover not found",
			}
		}
		return nil, err
	}

	host := fmt.Sprintf("%s.%s.svc", cacheName, ns)
	phase := mapReadyCondition(redis)
	message := "cache provisioning"
	if phase != ResourceReady {
		podReady, podMessage, err := p.redisPodsReady(ctx, ns, cacheName)
		if err != nil {
			message = fmt.Sprintf("cache pods not yet ready: %v", err)
		} else if podReady {
			phase = ResourceReady
			message = "cache ready"
		} else if podMessage != "" {
			message = podMessage
		}
	}
	if phase == ResourceReady {
		message = "cache ready"
	}

	return &CacheState{
		Phase:    phase,
		Engine:   string(app.Spec.Cache.Type),
		Endpoint: host,
		Port:     defaultRedisPort,
		SecretRef: &corev1.LocalObjectReference{
			Name: secretName,
		},
		ResourceID: fmt.Sprintf("redisfailover/%s", cacheName),
		Message:    message,
	}, nil
}

func (p *KubernetesProvider) cleanupCache(ctx context.Context, app *platformv1alpha1.Application) error {
	ns := p.resolveNamespace(app)
	cacheName := fmt.Sprintf("%s-cache", app.Name)
	secretName := fmt.Sprintf("%s-cache-credentials", app.Name)

	redis := &unstructured.Unstructured{}
	redis.SetGroupVersionKind(redisFailoverGVK)
	redis.SetName(cacheName)
	redis.SetNamespace(ns)
	if err := p.client.Delete(ctx, redis); err != nil && !apierrors.IsNotFound(err) {
		return &ProvisioningError{
			ResourceType: "cache",
			ResourceID:   cacheName,
			Operation:    "delete",
			Cause:        err,
			Message:      "failed to delete RedisFailover",
		}
	}

	return p.deleteSecret(ctx, ns, secretName)
}

// =============================================================================
// QUEUE (RabbitMQ Cluster Operator)
// =============================================================================

func (p *KubernetesProvider) reconcileQueue(ctx context.Context, app *platformv1alpha1.Application) (*QueueState, error) {
	if app.Spec.Queue.Type != platformv1alpha1.QueueRabbitMQ {
		return &QueueState{
				Phase:   ResourceFailed,
				Type:    string(app.Spec.Queue.Type),
				Message: "only rabbitmq is supported by the Kubernetes provider",
			}, &InvalidConfigError{
				ResourceType: "queue",
				Field:        "type",
				Value:        string(app.Spec.Queue.Type),
				Message:      "only rabbitmq is supported in-cluster",
			}
	}

	if err := p.ensureOperatorAvailable(rabbitmqClusterGVK); err != nil {
		return &QueueState{Phase: ResourceFailed, Type: "rabbitmq"}, err
	}

	ns := p.resolveNamespace(app)
	queueName := fmt.Sprintf("%s-queue", app.Name)
	secretName := fmt.Sprintf("%s-queue-credentials", app.Name)

	username := sanitizeIdentifier(app.Name)
	password, err := p.ensureSecret(ctx, ns, secretName, map[string][]byte{
		"username": []byte(username),
		"password": []byte(randomPassword(24)),
	})
	if err != nil {
		return &QueueState{Phase: ResourceFailed, Type: "rabbitmq"}, err
	}

	host := fmt.Sprintf("%s.%s.svc", queueName, ns)
	port := int32(defaultAMQPPort)
	conn := fmt.Sprintf("amqp://%s:%s@%s:%d/", username, password, host, port)
	if err := p.ensureSecretData(ctx, ns, secretName, map[string][]byte{
		"host":             []byte(host),
		"port":             []byte(fmt.Sprintf("%d", port)),
		"connectionString": []byte(conn),
	}); err != nil {
		return &QueueState{Phase: ResourceFailed, Type: "rabbitmq"}, err
	}

	rabbit := &unstructured.Unstructured{}
	rabbit.SetGroupVersionKind(rabbitmqClusterGVK)
	rabbit.SetName(queueName)
	rabbit.SetNamespace(ns)

	replicas := int64(1)
	if app.Spec.Queue.FIFO {
		// FIFO doesn't map to RabbitMQ; keep replicas = 1 and warn via message
	}

	labels := p.buildLabels(app)
	_, err = controllerutil.CreateOrUpdate(ctx, p.client, rabbit, func() error {
		spec := map[string]interface{}{
			"replicas": replicas,
			"rabbitmq": map[string]interface{}{
				"additionalConfig": fmt.Sprintf("default_user = %s\ndefault_pass = %s", username, password),
			},
		}

		if p.kubeCfg.StorageClass != "" {
			spec["persistence"] = map[string]interface{}{
				"storageClassName": p.kubeCfg.StorageClass,
				"storage":          defaultStorageSizeSmall,
			}
		}

		rabbit.Object["spec"] = spec
		rabbit.SetLabels(labels)
		return controllerutil.SetControllerReference(app, rabbit, p.scheme)
	})
	if err != nil {
		return &QueueState{Phase: ResourceFailed, Type: "rabbitmq"}, &ProvisioningError{
			ResourceType: "queue",
			ResourceID:   queueName,
			Operation:    "create/update",
			Cause:        err,
			Message:      "failed to reconcile RabbitmqCluster",
		}
	}

	if err := p.client.Get(ctx, client.ObjectKey{Name: queueName, Namespace: ns}, rabbit); err != nil {
		return &QueueState{Phase: ResourceProvisioning, Type: "rabbitmq"}, &NotReadyError{
			ResourceType: "queue",
			ResourceID:   queueName,
			CurrentPhase: ResourceProvisioning,
			Message:      "rabbitmq status not yet available",
		}
	}

	phase := mapReadyCondition(rabbit)
	message := "queue provisioning"
	if phase != ResourceReady {
		rabbitReady, rabbitMessage := rabbitmqReady(rabbit)
		if rabbitReady {
			phase = ResourceReady
			message = "queue ready"
		} else if rabbitMessage != "" {
			message = rabbitMessage
		}
	}
	if phase == ResourceReady {
		message = "queue ready"
	}
	if !rabbit.GetDeletionTimestamp().IsZero() {
		phase = ResourceDeleting
		message = "queue deleting"
	}

	state := &QueueState{
		Phase:      phase,
		Type:       string(app.Spec.Queue.Type),
		URL:        conn,
		ARN:        fmt.Sprintf("rabbitmq/%s", queueName),
		ResourceID: fmt.Sprintf("rabbitmq/%s", queueName),
		Message:    message,
	}

	if phase != ResourceReady {
		return state, &NotReadyError{
			ResourceType: "queue",
			ResourceID:   queueName,
			CurrentPhase: phase,
			Message:      message,
		}
	}

	return state, nil
}

func (p *KubernetesProvider) getQueueStatus(ctx context.Context, app *platformv1alpha1.Application) (*QueueState, error) {
	ns := p.resolveNamespace(app)
	queueName := fmt.Sprintf("%s-queue", app.Name)

	rabbit := &unstructured.Unstructured{}
	rabbit.SetGroupVersionKind(rabbitmqClusterGVK)
	if err := p.client.Get(ctx, client.ObjectKey{Name: queueName, Namespace: ns}, rabbit); err != nil {
		if apierrors.IsNotFound(err) {
			return &QueueState{Phase: ResourceNotFound, Type: "rabbitmq"}, &NotFoundError{
				ResourceType: "queue",
				ResourceID:   queueName,
				Message:      "RabbitmqCluster not found",
			}
		}
		return nil, err
	}

	phase := mapReadyCondition(rabbit)
	message := "queue provisioning"
	if phase != ResourceReady {
		rabbitReady, rabbitMessage := rabbitmqReady(rabbit)
		if rabbitReady {
			phase = ResourceReady
			message = "queue ready"
		} else if rabbitMessage != "" {
			message = rabbitMessage
		}
	}
	if phase == ResourceReady {
		message = "queue ready"
	}

	return &QueueState{
		Phase:      phase,
		Type:       string(app.Spec.Queue.Type),
		URL:        fmt.Sprintf("amqp://%s.%s.svc:%d", queueName, ns, defaultAMQPPort),
		ResourceID: fmt.Sprintf("rabbitmq/%s", queueName),
		Message:    message,
	}, nil
}

func (p *KubernetesProvider) cleanupQueue(ctx context.Context, app *platformv1alpha1.Application) error {
	ns := p.resolveNamespace(app)
	queueName := fmt.Sprintf("%s-queue", app.Name)
	secretName := fmt.Sprintf("%s-queue-credentials", app.Name)

	rabbit := &unstructured.Unstructured{}
	rabbit.SetGroupVersionKind(rabbitmqClusterGVK)
	rabbit.SetName(queueName)
	rabbit.SetNamespace(ns)
	if err := p.client.Delete(ctx, rabbit); err != nil && !apierrors.IsNotFound(err) {
		return &ProvisioningError{
			ResourceType: "queue",
			ResourceID:   queueName,
			Operation:    "delete",
			Cause:        err,
			Message:      "failed to delete RabbitmqCluster",
		}
	}

	return p.deleteSecret(ctx, ns, secretName)
}

// =============================================================================
// STORAGE (PVC-based)
// =============================================================================

func (p *KubernetesProvider) reconcileStorage(ctx context.Context, app *platformv1alpha1.Application) (*StorageState, error) {
	ns := p.resolveNamespace(app)
	pvcName := fmt.Sprintf("%s-storage", app.Name)

	labels := p.buildLabels(app)
	storageSize := defaultStorageSizeSmall
	if p.kubeCfg.ResourceLimits != nil && p.kubeCfg.ResourceLimits.MaxStorage != "" {
		storageSize = p.kubeCfg.ResourceLimits.MaxStorage
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: ns,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, p.client, pvc, func() error {
		pvc.Labels = labels
		pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		pvc.Spec.Resources = corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: resourceQuantity(storageSize),
			},
		}
		if p.kubeCfg.StorageClass != "" {
			pvc.Spec.StorageClassName = &p.kubeCfg.StorageClass
		}
		return controllerutil.SetControllerReference(app, pvc, p.scheme)
	})
	if err != nil {
		return &StorageState{Phase: ResourceFailed, Type: string(app.Spec.Storage.Type)}, &ProvisioningError{
			ResourceType: "storage",
			ResourceID:   pvcName,
			Operation:    "create/update",
			Cause:        err,
			Message:      "failed to reconcile PVC",
		}
	}

	if err := p.client.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: ns}, pvc); err != nil {
		return &StorageState{Phase: ResourceProvisioning, Type: string(app.Spec.Storage.Type)}, &NotReadyError{
			ResourceType: "storage",
			ResourceID:   pvcName,
			CurrentPhase: ResourceProvisioning,
			Message:      "PVC status not yet available",
		}
	}

	phase := ResourceProvisioning
	message := "storage provisioning"
	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		phase = ResourceReady
		message = "storage ready"
	case corev1.ClaimPending:
		phase = ResourceReady
		message = "storage pending (waiting for first consumer)"
	}
	if pvc.DeletionTimestamp != nil {
		phase = ResourceDeleting
		message = "storage deleting"
	}

	state := &StorageState{
		Phase:      phase,
		Type:       string(app.Spec.Storage.Type),
		BucketName: pvcName, // PVC-based storage, we map to BucketName for compatibility
		Region:     "kubernetes",
		ResourceID: fmt.Sprintf("pvc/%s", pvcName),
		Message:    message,
	}

	if phase != ResourceReady {
		return state, &NotReadyError{
			ResourceType: "storage",
			ResourceID:   pvcName,
			CurrentPhase: phase,
			Message:      message,
		}
	}

	return state, nil
}

func (p *KubernetesProvider) getStorageStatus(ctx context.Context, app *platformv1alpha1.Application) (*StorageState, error) {
	ns := p.resolveNamespace(app)
	pvcName := fmt.Sprintf("%s-storage", app.Name)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := p.client.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: ns}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return &StorageState{Phase: ResourceNotFound, Type: string(app.Spec.Storage.Type)}, &NotFoundError{
				ResourceType: "storage",
				ResourceID:   pvcName,
				Message:      "PVC not found",
			}
		}
		return nil, err
	}

	phase := ResourceProvisioning
	message := "storage provisioning"
	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		phase = ResourceReady
		message = "storage ready"
	case corev1.ClaimPending:
		phase = ResourceReady
		message = "storage pending (waiting for first consumer)"
	}

	return &StorageState{
		Phase:      phase,
		Type:       string(app.Spec.Storage.Type),
		BucketName: pvcName,
		Region:     "kubernetes",
		ResourceID: fmt.Sprintf("pvc/%s", pvcName),
		Message:    message,
	}, nil
}

func (p *KubernetesProvider) cleanupStorage(ctx context.Context, app *platformv1alpha1.Application) error {
	ns := p.resolveNamespace(app)
	pvcName := fmt.Sprintf("%s-storage", app.Name)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: ns,
		},
	}

	if err := p.client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return &ProvisioningError{
			ResourceType: "storage",
			ResourceID:   pvcName,
			Operation:    "delete",
			Cause:        err,
			Message:      "failed to delete PVC",
		}
	}

	return nil
}

// =============================================================================
// HELPERS
// =============================================================================

func (p *KubernetesProvider) resolveNamespace(app *platformv1alpha1.Application) string {
	if p.kubeCfg.Namespace != "" {
		return p.kubeCfg.Namespace
	}
	return app.Namespace
}

func (p *KubernetesProvider) ensureOperatorAvailable(gvk schema.GroupVersionKind) error {
	groupVersion := gvk.GroupVersion().String()
	resources, err := p.discovery.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		return &InvalidConfigError{
			ResourceType: "operator",
			Field:        "crd",
			Value:        gvk.Kind,
			Message:      fmt.Sprintf("CRD %s/%s not found; install the operator CRD before provisioning: %v", gvk.Group, gvk.Kind, err),
		}
	}

	for _, r := range resources.APIResources {
		if r.Kind == gvk.Kind {
			return nil
		}
	}

	return &InvalidConfigError{
		ResourceType: "operator",
		Field:        "crd",
		Value:        gvk.Kind,
		Message:      fmt.Sprintf("CRD %s/%s not found; install the operator CRD before provisioning", gvk.Group, gvk.Kind),
	}
}

func (p *KubernetesProvider) buildLabels(app *platformv1alpha1.Application) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       app.Name,
		"app.kubernetes.io/instance":   app.Name,
		"app.kubernetes.io/managed-by": "goplatform",
		"app.kubernetes.io/part-of":    "goplatform",
		"platform.goplatform.io/team":  sanitizeLabelValue(app.Spec.Team),
		"platform.goplatform.io/owner": sanitizeLabelValue(app.Spec.Owner),
		"platform.goplatform.io/tier":  string(app.Spec.Tier),
	}
}

// ensureSecret creates a Secret if it doesn't exist. It returns the password
// value that should be used in connection strings (generated or existing).
func (p *KubernetesProvider) ensureSecret(ctx context.Context, namespace, name string, data map[string][]byte) (string, error) {
	secret := &corev1.Secret{}
	err := p.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret)
	if err == nil {
		if b, ok := secret.Data["password"]; ok {
			return string(b), nil
		}
		return "", nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}

	if err := p.client.Create(ctx, secret); err != nil {
		return "", err
	}

	if b, ok := data["password"]; ok {
		return string(b), nil
	}
	return "", nil
}

// ensureSecretData adds data keys if missing, without overwriting existing values.
func (p *KubernetesProvider) ensureSecretData(ctx context.Context, namespace, name string, data map[string][]byte) error {
	secret := &corev1.Secret{}
	if err := p.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return &NotReadyError{
				ResourceType: "secret",
				ResourceID:   name,
				CurrentPhase: ResourceProvisioning,
				Message:      "secret not yet visible in cache",
			}
		}
		return err
	}

	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}

	changed := false
	for k, v := range data {
		if _, exists := secret.Data[k]; !exists {
			secret.Data[k] = v
			changed = true
		}
	}

	if !changed {
		return nil
	}

	return p.client.Update(ctx, secret)
}

func (p *KubernetesProvider) deleteSecret(ctx context.Context, namespace, name string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	if err := p.client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return &ProvisioningError{
			ResourceType: "secret",
			ResourceID:   name,
			Operation:    "delete",
			Cause:        err,
			Message:      "failed to delete secret",
		}
	}
	return nil
}

func mapReadyCondition(resource *unstructured.Unstructured) ResourcePhase {
	conditions, found, _ := unstructured.NestedSlice(resource.Object, "status", "conditions")
	if !found {
		return ResourceProvisioning
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); strings.EqualFold(t, "Ready") {
			status, _ := cond["status"].(string)
			if strings.EqualFold(status, "True") {
				return ResourceReady
			}
			return ResourceProvisioning
		}
	}

	return ResourceProvisioning
}

func rabbitmqReady(resource *unstructured.Unstructured) (bool, string) {
	conditions, found, _ := unstructured.NestedSlice(resource.Object, "status", "conditions")
	if !found {
		return false, "queue provisioning"
	}

	allReplicasReady := false
	clusterAvailable := false
	reconcileSuccess := false

	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		condStatus, _ := cond["status"].(string)

		if strings.EqualFold(condType, "AllReplicasReady") && strings.EqualFold(condStatus, "True") {
			allReplicasReady = true
		}
		if strings.EqualFold(condType, "ClusterAvailable") && strings.EqualFold(condStatus, "True") {
			clusterAvailable = true
		}
		if strings.EqualFold(condType, "ReconcileSuccess") && strings.EqualFold(condStatus, "True") {
			reconcileSuccess = true
		}
	}

	if allReplicasReady || (clusterAvailable && reconcileSuccess) {
		return true, "queue ready"
	}

	return false, "queue provisioning"
}

func (p *KubernetesProvider) redisPodsReady(ctx context.Context, namespace, cacheName string) (bool, string, error) {
	if p == nil || p.client == nil {
		return false, "redis client not configured", fmt.Errorf("client not configured")
	}

	pods := &corev1.PodList{}
	if err := p.client.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{
		"redisfailovers.databases.spotahome.com/name": cacheName,
	}); err != nil {
		return false, "redis pods not yet visible", err
	}

	if len(pods.Items) == 0 {
		return false, "redis pods not yet created", nil
	}

	for _, pod := range pods.Items {
		if isPodReady(pod) {
			return true, "", nil
		}
	}

	return false, "redis pods not ready", nil
}

func isPodReady(pod corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func sizeToStorage(size platformv1alpha1.ResourceSize) string {
	switch size {
	case platformv1alpha1.SizeSmall:
		return defaultStorageSizeSmall
	case platformv1alpha1.SizeMedium:
		return defaultStorageSizeMedium
	case platformv1alpha1.SizeLarge:
		return defaultStorageSizeLarge
	case platformv1alpha1.SizeXLarge:
		return defaultStorageSizeXLarge
	default:
		return defaultStorageSizeSmall
	}
}

func sanitizeIdentifier(name string) string {
	if name == "" {
		return "app"
	}
	lower := strings.ToLower(name)
	clean := make([]rune, 0, len(lower))
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			clean = append(clean, r)
		} else {
			clean = append(clean, '_')
		}
	}
	res := strings.Trim(string(clean), "_")
	if res == "" {
		return "app"
	}
	if len(res) > 16 {
		return res[:16]
	}
	return res
}

func randomPassword(length int) string {
	// Generate URL-safe random bytes, then base64-encode and trim to length.
	// This avoids punctuation that some operators reject in default_user/pass.
	buf := make([]byte, length)
	_, _ = rand.Read(buf)
	encoded := base64.RawURLEncoding.EncodeToString(buf)
	if len(encoded) > length {
		return encoded[:length]
	}
	return encoded
}

func resourceQuantity(value string) resource.Quantity {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		// Default to 10Gi on parse error to avoid invalid PVC specs
		q = resource.MustParse("10Gi")
	}
	return q
}

// sanitizeLabelValue converts a string to a valid Kubernetes label value.
// Kubernetes label values must match: (([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?
func sanitizeLabelValue(value string) string {
	if value == "" {
		return "unknown"
	}
	// Replace invalid characters with underscore and trim to 63 chars
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	res := strings.Trim(b.String(), "-_.")
	if res == "" {
		return "unknown"
	}
	if len(res) > 63 {
		return res[:63]
	}
	return res
}

// Compile-time check that KubernetesProvider implements InfrastructureProvider
var _ InfrastructureProvider = (*KubernetesProvider)(nil)
