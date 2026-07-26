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

// Package provider defines the abstraction layer for cloud infrastructure
// provisioning in Steward.
//
// # Why This Package Exists
//
// Steward aims to be cloud-agnostic. Users specify what they need
// (database: postgres, size: medium) without knowing AWS RDS vs GCP Cloud SQL.
// This package provides the interface that different cloud providers implement.
//
// # Available Providers
//
//   - aws: Uses AWS managed services (RDS, ElastiCache, SQS, S3):w
//   - gcp: Uses GCP managed services (Cloud SQL, Memorystore, Pub/Sub, GCS)
//   - kubernetes: Deploys everything in-cluster using Kubernetes operators
//   - mock: Returns fake responses for testing
//
// # Kubernetes-Native Provider
//
// The kubernetes provider is ideal for:
//   - Development and staging environments
//   - Cost-sensitive deployments
//   - Air-gapped or on-prem clusters
//   - Portability (no cloud vendor lock-in)
//
// It provisions resources using Kubernetes operators:
//   - PostgreSQL: CloudNativePG, Zalando, or StatefulSet
//   - Redis: Bitnami Helm chart or Redis Operator
//   - Queue: RabbitMQ Cluster Operator, NATS, or Strimzi
//   - Storage: PersistentVolumeClaims
//
// # Design Pattern: Strategy + Factory
//
// The Strategy pattern lets us swap provisioning algorithms at runtime.
// Each cloud provider is a "strategy" for infrastructure provisioning.
// The Factory creates the right provider based on configuration.
//
// # Package Structure
//
//   - doc.go: This file - package documentation
//   - types.go: Core types (ResourceState, ResourceStatus, KubernetesConfig)
//   - errors.go: Error types (NotFound, Provisioning, etc.)
//   - interface.go: InfrastructureProvider interface
//   - factory.go: ProviderFactory for provider instantiation
//   - mock.go: MockProvider for testing
//
// # Usage
//
//	// In ApplicationReconciler
//	type ApplicationReconciler struct {
//	    client.Client
//	    Scheme   *runtime.Scheme
//	    Provider provider.InfrastructureProvider
//	}
//
//	func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) {
//	    state, err := r.Provider.Provision(ctx, &app)
//	    if err != nil {
//	        if provider.IsNotReady(err) {
//	            return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
//	        }
//	        return ctrl.Result{}, err
//	    }
//	    // Update status with state
//	}
package provider
