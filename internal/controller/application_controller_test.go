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
// APPLICATION CONTROLLER TESTS
// =============================================================================
//
// These tests verify the controller's reconciliation logic using envtest.
//
// WHAT IS ENVTEST:
// ━━━━━━━━━━━━━━━━
//   Envtest runs a REAL Kubernetes API server and etcd (from test binaries),
//   giving us a genuine environment without needing a full cluster.
//
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │                         ENVTEST ARCHITECTURE                            │
//   │                                                                         │
//   │   ┌───────────────┐      ┌───────────────┐      ┌───────────────┐       │
//   │   │  Our Tests    │─────►│  kube-apiserver│─────►│    etcd       │       │
//   │   │               │      │  (from binaries)│     │  (from binaries)│     │
//   │   │  - Create CRDs│      │                │      │                │      │
//   │   │  - Reconcile  │      │  - Validates   │      │  - Stores      │      │
//   │   │  - Assert     │      │  - Serializes  │      │  - Watches     │      │
//   │   └───────────────┘      └───────────────┘      └───────────────┘       │
//   │                                                                         │
//   │   NO CONTROLLERS RUNNING BY DEFAULT                                     │
//   │   - We manually call Reconcile() to test it                             │
//   │   - This gives us deterministic, step-by-step testing                   │
//   │                                                                         │
//   └─────────────────────────────────────────────────────────────────────────┘
//
// WHY USE ENVTEST (vs alternatives):
//
//   ┌──────────────────────────────────────────────────────────────────────────┐
//   │ Approach          │ Pros                    │ Cons                       │
//   ├───────────────────┼─────────────────────────┼────────────────────────────┤
//   │ Unit tests with   │ Fast, isolated          │ Mocks can diverge from     │
//   │ mocks             │                         │ real K8s behavior          │
//   ├───────────────────┼─────────────────────────┼────────────────────────────┤
//   │ Envtest (we use)  │ Real API server,        │ Slower (~5s startup),      │
//   │                   │ catches schema bugs     │ no nodes/kubelet           │
//   ├───────────────────┼─────────────────────────┼────────────────────────────┤
//   │ kind/minikube     │ Full cluster, can test  │ Very slow, resource        │
//   │                   │ pod scheduling          │ intensive                  │
//   └──────────────────────────────────────────────────────────────────────────┘
//
// TESTING PATTERNS:
// ━━━━━━━━━━━━━━━━━
//   1. Create test resource
//   2. Manually call Reconcile()
//   3. Assert expected state (child resources created, status updated)
//   4. Repeat for edge cases (updates, deletes, errors)
//
// GINKGO/GOMEGA:
//   - BDD-style test framework (Describe/Context/It)
//   - Used by all Kubernetes projects
//   - Eventually() for async assertions
//
// =============================================================================

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
)

// =============================================================================
// TEST SUITE
// =============================================================================
//
// Each Describe block groups related tests.
// Context blocks provide different scenarios.
// It blocks are individual test cases.
//
// =============================================================================

var _ = Describe("Application Controller", func() {

	// =========================================================================
	// COMMON TEST FIXTURES
	// =========================================================================

	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	// Helper to create a basic Application for testing
	createTestApplication := func(name, namespace string, withWorkload bool) *platformv1alpha1.Application {
		app := &platformv1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: platformv1alpha1.ApplicationSpec{
				Team:  "platform",
				Owner: "test@example.com",
				Tier:  platformv1alpha1.TierStandard,
			},
		}

		if withWorkload {
			replicas := int32(1)
			app.Spec.Workload = &platformv1alpha1.WorkloadSpec{
				Image:    "nginx:latest",
				Replicas: &replicas,
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
				Ports: []platformv1alpha1.ContainerPort{
					{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
				},
				HealthCheck: &platformv1alpha1.HealthCheckSpec{
					Path:                "/health",
					Port:                8080,
					InitialDelaySeconds: 5,
					PeriodSeconds:       10,
					FailureThreshold:    3,
				},
			}
		}

		return app
	}

	// Helper to create reconciler
	// Uses a FakeRecorder to capture events during tests
	createReconciler := func() *ApplicationReconciler {
		return &ApplicationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			// FakeRecorder with buffer of 100 events
			// In tests, we can check recorder.Events channel for emitted events
			Recorder: record.NewFakeRecorder(100),
		}
	}

	// Helper to create reconciler with a custom cleanup hook
	createReconcilerWithCleanup := func(cleanup func(ctx context.Context, app *platformv1alpha1.Application) error) *ApplicationReconciler {
		reconciler := createReconciler()
		reconciler.CleanupExternalResources = cleanup
		return reconciler
	}

	// =========================================================================
	// TEST: BASIC RECONCILIATION
	// =========================================================================
	//
	// Verifies that reconciling a basic Application:
	//   - Adds a finalizer
	//   - Creates a Deployment
	//   - Creates a Service
	//   - Updates status with conditions
	//
	// =========================================================================

	Context("When reconciling an Application with workload", func() {
		const resourceName = "test-app-workload"
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: "default",
			}

			// Create the Application
			app := createTestApplication(resourceName, "default", true)
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			// Verify it was created
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, &platformv1alpha1.Application{})
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			// Clean up
			app := &platformv1alpha1.Application{}
			if err := k8sClient.Get(ctx, typeNamespacedName, app); err == nil {
				// Remove finalizer first to allow deletion
				app.Finalizers = nil
				_ = k8sClient.Update(ctx, app)
				_ = k8sClient.Delete(ctx, app)
			}

			// Clean up Deployment
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, typeNamespacedName, deploy); err == nil {
				_ = k8sClient.Delete(ctx, deploy)
			}

			// Clean up Service
			svc := &corev1.Service{}
			if err := k8sClient.Get(ctx, typeNamespacedName, svc); err == nil {
				_ = k8sClient.Delete(ctx, svc)
			}
		})

		It("should add a finalizer and provision in the same reconcile", func() {
			// =====================================================================
			// TEST: FINALIZER ADDITION
			// =====================================================================
			//
			// The first reconcile adds our finalizer so we can clean up external
			// resources before deletion, and then continues provisioning in the
			// same pass. Adding a finalizer is a metadata write that does not bump
			// metadata.generation, so GenerationChangedPredicate would swallow the
			// resulting watch event — returning early here would strand the object
			// until the next resync.
			//
			// =====================================================================

			By("Reconciling the created resource")
			reconciler := createReconciler()
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the finalizer was added")
			app := &platformv1alpha1.Application{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, app)).To(Succeed())
			Expect(app.Finalizers).To(ContainElement(applicationFinalizer))

			By("Verifying the same pass also provisioned children and scheduled a resync")
			Expect(result.RequeueAfter).To(Equal(requeueAfterSuccess))
			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
		})

		It("should create a Deployment on reconcile", func() {
			// =====================================================================
			// TEST: DEPLOYMENT CREATION
			// =====================================================================
			//
			// After adding the finalizer, subsequent reconciles should create
			// the Deployment that runs the workload.
			//
			// =====================================================================

			By("Reconciling twice (first adds finalizer, second creates resources)")
			reconciler := createReconciler()

			// First reconcile - adds finalizer
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile - creates Deployment
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Deployment was created")
			deployment := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, deployment)
			}, timeout, interval).Should(Succeed())

			// Verify Deployment spec matches Application spec
			Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:latest"))
			Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))

			// Verify labels
			Expect(deployment.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", resourceName))
			Expect(deployment.Labels).To(HaveKeyWithValue("platform.goplatform.io/team", "platform"))
			Expect(deployment.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "goplatform"))

			// Verify owner reference
			Expect(deployment.OwnerReferences).To(HaveLen(1))
			Expect(deployment.OwnerReferences[0].Kind).To(Equal("Application"))
		})

		It("should create a Service on reconcile", func() {
			// =====================================================================
			// TEST: SERVICE CREATION
			// =====================================================================
			//
			// A Service is created to provide stable networking
			// for the Deployment's pods.
			//
			// =====================================================================

			By("Reconciling twice")
			reconciler := createReconciler()

			// First reconcile - adds finalizer
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			// Second reconcile - creates resources
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Service was created")
			service := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, service)
			}, timeout, interval).Should(Succeed())

			// Verify Service spec
			Expect(service.Spec.Ports).To(HaveLen(1))
			Expect(service.Spec.Ports[0].Name).To(Equal("http"))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(service.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))

			// Verify selector matches Deployment pods
			Expect(service.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/name", resourceName))
		})

		It("should update Application status", func() {
			// =====================================================================
			// TEST: STATUS UPDATES
			// =====================================================================
			//
			// Status should reflect the current state of child resources.
			// Conditions provide granular status information.
			//
			// =====================================================================

			By("Reconciling the resource")
			reconciler := createReconciler()

			// First reconcile
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			// Second reconcile
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("Verifying status was updated")
			app := &platformv1alpha1.Application{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, typeNamespacedName, app); err != nil {
					return false
				}
				return len(app.Status.Conditions) > 0
			}, timeout, interval).Should(BeTrue())

			// Verify conditions exist
			readyCondition := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())

			workloadCondition := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeWorkloadReady)
			Expect(workloadCondition).NotTo(BeNil())

			databaseCondition := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeDatabaseReady)
			Expect(databaseCondition).NotTo(BeNil())
			Expect(databaseCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(databaseCondition.Reason).To(Equal("DatabaseNotRequested"))

			cacheCondition := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeCacheReady)
			Expect(cacheCondition).NotTo(BeNil())
			Expect(cacheCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(cacheCondition.Reason).To(Equal("CacheNotRequested"))

			queueCondition := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeQueueReady)
			Expect(queueCondition).NotTo(BeNil())
			Expect(queueCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(queueCondition.Reason).To(Equal("QueueNotRequested"))

			storageCondition := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeStorageReady)
			Expect(storageCondition).NotTo(BeNil())
			Expect(storageCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(storageCondition.Reason).To(Equal("StorageNotRequested"))

			// Verify observed generation
			Expect(app.Status.ObservedGeneration).To(Equal(app.Generation))
		})
	})

	// =========================================================================
	// TEST: APPLICATION WITHOUT WORKLOAD
	// =========================================================================
	//
	// Applications without a workload spec should:
	//   - Not create a Deployment or Service
	//   - Still update status correctly
	//   - Be marked Ready immediately
	//
	// USE CASE: Infrastructure-only applications (database, cache, etc.)
	//
	// =========================================================================

	Context("When reconciling an Application without workload", func() {
		const resourceName = "test-app-no-workload"
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: "default",
			}

			// Create Application without workload
			app := createTestApplication(resourceName, "default", false)
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
		})

		AfterEach(func() {
			app := &platformv1alpha1.Application{}
			if err := k8sClient.Get(ctx, typeNamespacedName, app); err == nil {
				app.Finalizers = nil
				_ = k8sClient.Update(ctx, app)
				_ = k8sClient.Delete(ctx, app)
			}
		})

		It("should not create Deployment or Service", func() {
			By("Reconciling the resource")
			reconciler := createReconciler()

			// First reconcile - adds finalizer
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			// Second reconcile
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no Deployment was created")
			deployment := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, typeNamespacedName, deployment)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			By("Verifying no Service was created")
			service := &corev1.Service{}
			err = k8sClient.Get(ctx, typeNamespacedName, service)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should mark as Ready", func() {
			By("Reconciling the resource")
			reconciler := createReconciler()

			// Reconcile until status is updated
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("Verifying Ready status")
			app := &platformv1alpha1.Application{}
			Eventually(func() platformv1alpha1.ApplicationPhase {
				if err := k8sClient.Get(ctx, typeNamespacedName, app); err != nil {
					return ""
				}
				return app.Status.Phase
			}, timeout, interval).Should(Equal(platformv1alpha1.ApplicationReady))

			readyCondition := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	// =========================================================================
	// TEST: INFRASTRUCTURE CONDITIONS (REQUESTED COMPONENTS)
	// =========================================================================

	Context("When reconciling an Application with requested infrastructure", func() {
		const resourceName = "test-app-infra-requested"
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: "default",
			}

			app := createTestApplication(resourceName, "default", true)
			app.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "15",
			}

			Expect(k8sClient.Create(ctx, app)).To(Succeed())
		})

		AfterEach(func() {
			app := &platformv1alpha1.Application{}
			if err := k8sClient.Get(ctx, typeNamespacedName, app); err == nil {
				app.Finalizers = nil
				_ = k8sClient.Update(ctx, app)
				_ = k8sClient.Delete(ctx, app)
			}
		})

		It("should set database condition to ready when provider reports ready", func() {
			// createReconciler() has no ProviderFactory, so getProvider()
			// creates a default Factory → loads env (defaults to "mock") →
			// creates MockProvider with delay=0 → Provision returns Ready immediately.
			reconciler := createReconciler()

			// First reconcile - adds finalizer
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			// Second reconcile - provisions infrastructure
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("Verifying DatabaseReady condition is set")
			app := &platformv1alpha1.Application{}
			Eventually(func() *metav1.Condition {
				if err := k8sClient.Get(ctx, typeNamespacedName, app); err != nil {
					return nil
				}
				return meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeDatabaseReady)
			}, timeout, interval).ShouldNot(BeNil())

			databaseCondition := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeDatabaseReady)
			Expect(databaseCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(databaseCondition.Reason).To(Equal("DatabaseReady"))
		})
	})

	// =========================================================================
	// TEST: RESOURCE GENERATION (MILESTONE 3)
	// =========================================================================

	Context("When generating specialized resources", func() {
		const resourceName = "test-app-resources"
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: "default",
			}
		})

		AfterEach(func() {
			app := &platformv1alpha1.Application{}
			if err := k8sClient.Get(ctx, typeNamespacedName, app); err == nil {
				app.Finalizers = nil
				_ = k8sClient.Update(ctx, app)
				_ = k8sClient.Delete(ctx, app)
			}
		})

		It("should create ConfigMap, Secret, HPA, and PDB", func() {
			By("Creating the Application")
			app := createTestApplication(resourceName, "default", true)

			// Configure Scaling
			minReplicas := int32(2)
			app.Spec.Scaling = &platformv1alpha1.ScalingSpec{
				MinReplicas: &minReplicas,
				MaxReplicas: 5,
				Metrics: []platformv1alpha1.ScalingMetric{
					{Type: "cpu", Target: 80},
				},
			}

			// Configure High Availability (trigger PDB)
			app.Spec.Tier = platformv1alpha1.TierCritical

			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			By("Reconciling")
			reconciler := createReconciler()

			// Initial reconcile
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			// Second reconcile to proceed past finalizers
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

			By("Verifying ConfigMap")
			cm := &corev1.ConfigMap{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, cm)
			}, timeout, interval).Should(Succeed())
			Expect(cm.Data["APP_NAME"]).To(Equal(resourceName))

			By("Verifying Secret")
			secret := &corev1.Secret{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, secret)
			}, timeout, interval).Should(Succeed())

			By("Verifying HPA")
			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, hpa)
			}, timeout, interval).Should(Succeed())
			Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))
			Expect(*hpa.Spec.MinReplicas).To(Equal(int32(2)))

			By("Verifying PDB")
			pdb := &policyv1.PodDisruptionBudget{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, pdb)
			}, timeout, interval).Should(Succeed())
			Expect(pdb.Spec.MinAvailable.IntVal).To(Equal(int32(1)))
		})
	})

	// =========================================================================
	// TEST: DELETION HANDLING
	// =========================================================================
	//
	// When an Application is deleted:
	//   1. deletionTimestamp is set
	//   2. Our finalizer blocks actual deletion
	//   3. handleDeletion() cleans up resources
	//   4. Finalizer is removed
	//   5. Kubernetes deletes the object
	//
	// =========================================================================

	Context("When deleting an Application", func() {
		const resourceName = "test-app-delete"
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: "default",
			}

			// Create and reconcile Application
			app := createTestApplication(resourceName, "default", true)
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			reconciler := createReconciler()
			// Add finalizer
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			// Create resources
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
		})

		AfterEach(func() {
			// Final cleanup if test didn't complete deletion
			app := &platformv1alpha1.Application{}
			if err := k8sClient.Get(ctx, typeNamespacedName, app); err == nil {
				app.Finalizers = nil
				_ = k8sClient.Update(ctx, app)
				_ = k8sClient.Delete(ctx, app)
			}
		})

		It("should remove finalizer and allow deletion", func() {
			By("Deleting the Application")
			app := &platformv1alpha1.Application{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, app)).To(Succeed())
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())

			By("Reconciling to handle deletion")
			reconciler := createReconciler()

			// handleDeletion stamps the deletion-start annotation and then runs
			// cleanup in the same pass, so one reconcile is enough. Extra passes
			// are harmless (the flow is idempotent) but should not be needed.
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Application was deleted")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespacedName, app)
				return errors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
		})

		It("should keep finalizer when cleanup fails", func() {
			By("Deleting the Application")
			app := &platformv1alpha1.Application{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, app)).To(Succeed())
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())

			By("Reconciling with a cleanup hook that fails")
			reconciler := createReconcilerWithCleanup(func(ctx context.Context, app *platformv1alpha1.Application) error {
				return fmt.Errorf("simulated cleanup failure")
			})
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).To(HaveOccurred())

			// controller-runtime ignores Result when the error is non-nil, so the
			// failure path must not pretend to schedule its own retry — the error
			// alone drives the rate-limited, backed-off requeue.
			Expect(result.RequeueAfter).To(BeZero(),
				"a non-nil error must be paired with a zero Result")

			By("Verifying the failure is retried on subsequent passes")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).To(HaveOccurred())

			By("Verifying finalizer is still present")
			eventuallyApp := &platformv1alpha1.Application{}
			Eventually(func() []string {
				if err := k8sClient.Get(ctx, typeNamespacedName, eventuallyApp); err != nil {
					return nil
				}
				return eventuallyApp.Finalizers
			}, timeout, interval).Should(ContainElement(applicationFinalizer))
		})
	})

	// =========================================================================
	// TEST: RECONCILING DELETED RESOURCE
	// =========================================================================
	//
	// If reconcile is called for a resource that no longer exists
	// (race condition, event coalescing), we should handle gracefully.
	//
	// =========================================================================

	Context("When reconciling a non-existent Application", func() {
		It("should return without error", func() {
			By("Reconciling a non-existent resource")
			reconciler := createReconciler()
			result, err := reconciler.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "does-not-exist",
					Namespace: "default",
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})

	// =========================================================================
	// TEST: CREDENTIAL INJECTION - UNIT TESTS
	// =========================================================================
	//
	// These tests validate the credential injection functions directly
	// without requiring envtest. They cover:
	//   - secretEnvVar: building a single env var from a Secret key
	//   - buildDatabaseEnvVars: postgres vs mysql env var names
	//   - buildCacheEnvVars: redis env vars
	//   - buildQueueEnvVars: rabbitmq env vars
	//   - appendIfNotDefined: user-defined precedence logic
	//   - buildEnvVars: full orchestration with opt-out and precedence
	//
	// WHY UNIT TEST THESE:
	//   A broken secretKeyRef or wrong secret name silently results in
	//   empty env vars at runtime — the pod starts but the app can't
	//   connect to anything. These tests catch naming mismatches early.
	//
	// =========================================================================

	Context("Credential injection unit tests", func() {

		// =====================================================================
		// secretEnvVar
		// =====================================================================

		Describe("secretEnvVar", func() {
			It("should build an EnvVar with a secretKeyRef", func() {
				envVar := secretEnvVar("DATABASE_URL", "my-app-db-credentials", "connectionString")

				Expect(envVar.Name).To(Equal("DATABASE_URL"))
				Expect(envVar.Value).To(BeEmpty(), "Value must be empty when using ValueFrom")
				Expect(envVar.ValueFrom).NotTo(BeNil())
				Expect(envVar.ValueFrom.SecretKeyRef).NotTo(BeNil())
				Expect(envVar.ValueFrom.SecretKeyRef.Name).To(Equal("my-app-db-credentials"))
				Expect(envVar.ValueFrom.SecretKeyRef.Key).To(Equal("connectionString"))
			})
		})

		// =====================================================================
		// buildDatabaseEnvVars
		// =====================================================================

		Describe("buildDatabaseEnvVars", func() {
			It("should return PostgreSQL-specific env vars for postgres type", func() {
				vars := buildDatabaseEnvVars("my-app-db-credentials", platformv1alpha1.DatabasePostgres)

				// Should produce: DATABASE_URL, PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE
				Expect(vars).To(HaveLen(6))

				names := envVarNames(vars)
				Expect(names).To(ConsistOf(
					"DATABASE_URL",
					"PGHOST",
					"PGPORT",
					"PGUSER",
					"PGPASSWORD",
					"PGDATABASE",
				))

				// Verify secret references
				for _, v := range vars {
					Expect(v.ValueFrom).NotTo(BeNil(), "env var %s should use ValueFrom", v.Name)
					Expect(v.ValueFrom.SecretKeyRef.Name).To(Equal("my-app-db-credentials"),
						"env var %s should reference the correct secret", v.Name)
				}

				// Verify specific key mappings
				Expect(findEnvVar(vars, "DATABASE_URL").ValueFrom.SecretKeyRef.Key).To(Equal("connectionString"))
				Expect(findEnvVar(vars, "PGHOST").ValueFrom.SecretKeyRef.Key).To(Equal("host"))
				Expect(findEnvVar(vars, "PGPORT").ValueFrom.SecretKeyRef.Key).To(Equal("port"))
				Expect(findEnvVar(vars, "PGUSER").ValueFrom.SecretKeyRef.Key).To(Equal("username"))
				Expect(findEnvVar(vars, "PGPASSWORD").ValueFrom.SecretKeyRef.Key).To(Equal("password"))
				Expect(findEnvVar(vars, "PGDATABASE").ValueFrom.SecretKeyRef.Key).To(Equal("database"))
			})

			It("should return MySQL-specific env vars for mysql type", func() {
				vars := buildDatabaseEnvVars("my-app-db-credentials", platformv1alpha1.DatabaseMySQL)

				// Should produce: DATABASE_URL, MYSQL_HOST, MYSQL_PORT, MYSQL_USER, MYSQL_PASSWORD, MYSQL_DATABASE
				Expect(vars).To(HaveLen(6))

				names := envVarNames(vars)
				Expect(names).To(ConsistOf(
					"DATABASE_URL",
					"MYSQL_HOST",
					"MYSQL_PORT",
					"MYSQL_USER",
					"MYSQL_PASSWORD",
					"MYSQL_DATABASE",
				))

				// Verify specific key mappings for MySQL
				Expect(findEnvVar(vars, "DATABASE_URL").ValueFrom.SecretKeyRef.Key).To(Equal("connectionString"))
				Expect(findEnvVar(vars, "MYSQL_HOST").ValueFrom.SecretKeyRef.Key).To(Equal("host"))
				Expect(findEnvVar(vars, "MYSQL_PORT").ValueFrom.SecretKeyRef.Key).To(Equal("port"))
				Expect(findEnvVar(vars, "MYSQL_USER").ValueFrom.SecretKeyRef.Key).To(Equal("username"))
				Expect(findEnvVar(vars, "MYSQL_PASSWORD").ValueFrom.SecretKeyRef.Key).To(Equal("password"))
				Expect(findEnvVar(vars, "MYSQL_DATABASE").ValueFrom.SecretKeyRef.Key).To(Equal("database"))
			})

			It("should return only DATABASE_URL for unknown type", func() {
				// The switch statement has no default case, so only DATABASE_URL
				// is added for an unrecognized type. This is a safety net.
				vars := buildDatabaseEnvVars("my-app-db-credentials", "unknown")

				Expect(vars).To(HaveLen(1))
				Expect(vars[0].Name).To(Equal("DATABASE_URL"))
			})
		})

		// =====================================================================
		// buildCacheEnvVars
		// =====================================================================

		Describe("buildCacheEnvVars", func() {
			It("should return Redis env vars", func() {
				vars := buildCacheEnvVars("my-app-cache-credentials")

				Expect(vars).To(HaveLen(4))

				names := envVarNames(vars)
				Expect(names).To(ConsistOf(
					"REDIS_URL",
					"REDIS_HOST",
					"REDIS_PORT",
					"REDIS_PASSWORD",
				))

				// Verify all reference the correct secret
				for _, v := range vars {
					Expect(v.ValueFrom.SecretKeyRef.Name).To(Equal("my-app-cache-credentials"))
				}

				// Verify key mappings
				Expect(findEnvVar(vars, "REDIS_URL").ValueFrom.SecretKeyRef.Key).To(Equal("connectionString"))
				Expect(findEnvVar(vars, "REDIS_HOST").ValueFrom.SecretKeyRef.Key).To(Equal("host"))
				Expect(findEnvVar(vars, "REDIS_PORT").ValueFrom.SecretKeyRef.Key).To(Equal("port"))
				Expect(findEnvVar(vars, "REDIS_PASSWORD").ValueFrom.SecretKeyRef.Key).To(Equal("password"))
			})
		})

		// =====================================================================
		// buildQueueEnvVars
		// =====================================================================

		Describe("buildQueueEnvVars", func() {
			It("should return RabbitMQ env vars", func() {
				vars := buildQueueEnvVars("my-app-queue-credentials")

				Expect(vars).To(HaveLen(5))

				names := envVarNames(vars)
				Expect(names).To(ConsistOf(
					"AMQP_URL",
					"RABBITMQ_HOST",
					"RABBITMQ_PORT",
					"RABBITMQ_USER",
					"RABBITMQ_PASSWORD",
				))

				// Verify all reference the correct secret
				for _, v := range vars {
					Expect(v.ValueFrom.SecretKeyRef.Name).To(Equal("my-app-queue-credentials"))
				}

				// Verify key mappings
				Expect(findEnvVar(vars, "AMQP_URL").ValueFrom.SecretKeyRef.Key).To(Equal("connectionString"))
				Expect(findEnvVar(vars, "RABBITMQ_HOST").ValueFrom.SecretKeyRef.Key).To(Equal("host"))
				Expect(findEnvVar(vars, "RABBITMQ_PORT").ValueFrom.SecretKeyRef.Key).To(Equal("port"))
				Expect(findEnvVar(vars, "RABBITMQ_USER").ValueFrom.SecretKeyRef.Key).To(Equal("username"))
				Expect(findEnvVar(vars, "RABBITMQ_PASSWORD").ValueFrom.SecretKeyRef.Key).To(Equal("password"))
			})
		})

		// =====================================================================
		// appendIfNotDefined
		// =====================================================================

		Describe("appendIfNotDefined", func() {
			It("should append vars that are not in the user-defined set", func() {
				existing := []corev1.EnvVar{
					{Name: "USER_VAR", Value: "user-value"},
				}
				userDefined := map[string]struct{}{
					"USER_VAR": {},
				}
				toAppend := []corev1.EnvVar{
					{Name: "NEW_VAR", Value: "new-value"},
					{Name: "USER_VAR", Value: "should-be-skipped"},
				}

				result := appendIfNotDefined(existing, userDefined, toAppend...)

				Expect(result).To(HaveLen(2))
				Expect(result[0].Name).To(Equal("USER_VAR"))
				Expect(result[0].Value).To(Equal("user-value"))
				Expect(result[1].Name).To(Equal("NEW_VAR"))
				Expect(result[1].Value).To(Equal("new-value"))
			})

			It("should append all vars when no conflicts exist", func() {
				existing := []corev1.EnvVar{}
				userDefined := map[string]struct{}{}
				toAppend := []corev1.EnvVar{
					{Name: "A", Value: "1"},
					{Name: "B", Value: "2"},
				}

				result := appendIfNotDefined(existing, userDefined, toAppend...)
				Expect(result).To(HaveLen(2))
			})

			It("should skip all vars when all conflict", func() {
				existing := []corev1.EnvVar{
					{Name: "A", Value: "user"},
					{Name: "B", Value: "user"},
				}
				userDefined := map[string]struct{}{
					"A": {},
					"B": {},
				}
				toAppend := []corev1.EnvVar{
					{Name: "A", Value: "injected"},
					{Name: "B", Value: "injected"},
				}

				result := appendIfNotDefined(existing, userDefined, toAppend...)
				Expect(result).To(HaveLen(2))
				Expect(result[0].Value).To(Equal("user"))
				Expect(result[1].Value).To(Equal("user"))
			})
		})

		// =====================================================================
		// buildEnvVars — the orchestrator
		// =====================================================================

		Describe("buildEnvVars", func() {
			var reconciler *ApplicationReconciler

			BeforeEach(func() {
				reconciler = createReconciler()
			})

			It("should inject database env vars when database is in spec", func() {
				app := &platformv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
					Spec: platformv1alpha1.ApplicationSpec{
						Team:  "platform",
						Owner: "test@example.com",
						Workload: &platformv1alpha1.WorkloadSpec{
							Image: "nginx:latest",
						},
						Database: &platformv1alpha1.DatabaseSpec{
							Type:    platformv1alpha1.DatabasePostgres,
							Version: "16",
						},
					},
				}

				vars := reconciler.buildEnvVars(app)

				names := envVarNames(vars)
				Expect(names).To(ContainElements(
					"DATABASE_URL", "PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE",
				))

				// Verify secret name follows convention: {app.Name}-db-credentials
				dbURL := findEnvVar(vars, "DATABASE_URL")
				Expect(dbURL.ValueFrom.SecretKeyRef.Name).To(Equal("my-app-db-credentials"))
			})

			It("should inject cache env vars when cache is in spec", func() {
				app := &platformv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
					Spec: platformv1alpha1.ApplicationSpec{
						Team:  "platform",
						Owner: "test@example.com",
						Workload: &platformv1alpha1.WorkloadSpec{
							Image: "nginx:latest",
						},
						Cache: &platformv1alpha1.CacheSpec{
							Type: platformv1alpha1.CacheRedis,
						},
					},
				}

				vars := reconciler.buildEnvVars(app)

				names := envVarNames(vars)
				Expect(names).To(ContainElements(
					"REDIS_URL", "REDIS_HOST", "REDIS_PORT", "REDIS_PASSWORD",
				))

				redisURL := findEnvVar(vars, "REDIS_URL")
				Expect(redisURL.ValueFrom.SecretKeyRef.Name).To(Equal("my-app-cache-credentials"))
			})

			It("should inject queue env vars when queue is in spec", func() {
				app := &platformv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
					Spec: platformv1alpha1.ApplicationSpec{
						Team:  "platform",
						Owner: "test@example.com",
						Workload: &platformv1alpha1.WorkloadSpec{
							Image: "nginx:latest",
						},
						Queue: &platformv1alpha1.QueueSpec{
							Type: platformv1alpha1.QueueRabbitMQ,
						},
					},
				}

				vars := reconciler.buildEnvVars(app)

				names := envVarNames(vars)
				Expect(names).To(ContainElements(
					"AMQP_URL", "RABBITMQ_HOST", "RABBITMQ_PORT", "RABBITMQ_USER", "RABBITMQ_PASSWORD",
				))

				amqpURL := findEnvVar(vars, "AMQP_URL")
				Expect(amqpURL.ValueFrom.SecretKeyRef.Name).To(Equal("my-app-queue-credentials"))
			})

			It("should inject all infrastructure env vars when all are in spec", func() {
				app := &platformv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "full-app", Namespace: "default"},
					Spec: platformv1alpha1.ApplicationSpec{
						Team:  "platform",
						Owner: "test@example.com",
						Workload: &platformv1alpha1.WorkloadSpec{
							Image: "nginx:latest",
						},
						Database: &platformv1alpha1.DatabaseSpec{
							Type:    platformv1alpha1.DatabasePostgres,
							Version: "16",
						},
						Cache: &platformv1alpha1.CacheSpec{
							Type: platformv1alpha1.CacheRedis,
						},
						Queue: &platformv1alpha1.QueueSpec{
							Type: platformv1alpha1.QueueRabbitMQ,
						},
					},
				}

				vars := reconciler.buildEnvVars(app)

				// 6 (postgres) + 4 (redis) + 5 (rabbitmq) = 15
				Expect(vars).To(HaveLen(15))

				names := envVarNames(vars)
				Expect(names).To(ContainElements("DATABASE_URL", "PGHOST"))
				Expect(names).To(ContainElements("REDIS_URL", "REDIS_HOST"))
				Expect(names).To(ContainElements("AMQP_URL", "RABBITMQ_HOST"))
			})

			It("should not inject any credentials when InjectCredentials is false", func() {
				injectFalse := false
				app := &platformv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
					Spec: platformv1alpha1.ApplicationSpec{
						Team:  "platform",
						Owner: "test@example.com",
						Workload: &platformv1alpha1.WorkloadSpec{
							Image:             "nginx:latest",
							InjectCredentials: &injectFalse,
						},
						Database: &platformv1alpha1.DatabaseSpec{
							Type:    platformv1alpha1.DatabasePostgres,
							Version: "16",
						},
						Cache: &platformv1alpha1.CacheSpec{
							Type: platformv1alpha1.CacheRedis,
						},
					},
				}

				vars := reconciler.buildEnvVars(app)

				Expect(vars).To(BeEmpty(), "no env vars should be injected when InjectCredentials=false")
			})

			It("should preserve user-defined env vars when InjectCredentials is false", func() {
				injectFalse := false
				app := &platformv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
					Spec: platformv1alpha1.ApplicationSpec{
						Team:  "platform",
						Owner: "test@example.com",
						Workload: &platformv1alpha1.WorkloadSpec{
							Image:             "nginx:latest",
							InjectCredentials: &injectFalse,
							Env: []corev1.EnvVar{
								{Name: "MY_VAR", Value: "my-value"},
							},
						},
						Database: &platformv1alpha1.DatabaseSpec{
							Type:    platformv1alpha1.DatabasePostgres,
							Version: "16",
						},
					},
				}

				vars := reconciler.buildEnvVars(app)

				Expect(vars).To(HaveLen(1))
				Expect(vars[0].Name).To(Equal("MY_VAR"))
				Expect(vars[0].Value).To(Equal("my-value"))
			})

			It("should let user-defined DATABASE_URL take precedence over injected one", func() {
				app := &platformv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
					Spec: platformv1alpha1.ApplicationSpec{
						Team:  "platform",
						Owner: "test@example.com",
						Workload: &platformv1alpha1.WorkloadSpec{
							Image: "nginx:latest",
							Env: []corev1.EnvVar{
								{Name: "DATABASE_URL", Value: "postgres://external-pooler:5432/db"},
							},
						},
						Database: &platformv1alpha1.DatabaseSpec{
							Type:    platformv1alpha1.DatabasePostgres,
							Version: "16",
						},
					},
				}

				vars := reconciler.buildEnvVars(app)

				// DATABASE_URL should be the user's value (plain string), not a secretKeyRef
				dbURL := findEnvVar(vars, "DATABASE_URL")
				Expect(dbURL.Value).To(Equal("postgres://external-pooler:5432/db"))
				Expect(dbURL.ValueFrom).To(BeNil(), "user-defined var should not use ValueFrom")

				// Other PG vars should still be injected (user only overrode DATABASE_URL)
				names := envVarNames(vars)
				Expect(names).To(ContainElements("PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE"))
			})

			It("should not inject anything when no infrastructure is in spec", func() {
				app := &platformv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
					Spec: platformv1alpha1.ApplicationSpec{
						Team:  "platform",
						Owner: "test@example.com",
						Workload: &platformv1alpha1.WorkloadSpec{
							Image: "nginx:latest",
						},
					},
				}

				vars := reconciler.buildEnvVars(app)
				Expect(vars).To(BeEmpty())
			})

			It("should inject by default when InjectCredentials is nil (pointer default)", func() {
				// InjectCredentials defaults to true. When nil, injection should proceed.
				app := &platformv1alpha1.Application{
					ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
					Spec: platformv1alpha1.ApplicationSpec{
						Team:  "platform",
						Owner: "test@example.com",
						Workload: &platformv1alpha1.WorkloadSpec{
							Image:             "nginx:latest",
							InjectCredentials: nil, // not set — should default to inject
						},
						Database: &platformv1alpha1.DatabaseSpec{
							Type:    platformv1alpha1.DatabasePostgres,
							Version: "16",
						},
					},
				}

				vars := reconciler.buildEnvVars(app)
				Expect(vars).To(HaveLen(6))
			})
		})
	})

	// =========================================================================
	// TEST: CREDENTIAL INJECTION - INTEGRATION TESTS
	// =========================================================================
	//
	// These tests verify that credential env vars actually end up in the
	// Deployment created by the controller. Unlike the unit tests above
	// (which test functions directly), these tests go through the full
	// reconciliation loop with envtest.
	//
	// =========================================================================

	Context("Credential injection integration tests", func() {
		const resourceName = "test-cred-inject"
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: "default",
			}
		})

		AfterEach(func() {
			app := &platformv1alpha1.Application{}
			if err := k8sClient.Get(ctx, typeNamespacedName, app); err == nil {
				app.Finalizers = nil
				_ = k8sClient.Update(ctx, app)
				_ = k8sClient.Delete(ctx, app)
			}
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, typeNamespacedName, deploy); err == nil {
				_ = k8sClient.Delete(ctx, deploy)
			}
			svc := &corev1.Service{}
			if err := k8sClient.Get(ctx, typeNamespacedName, svc); err == nil {
				_ = k8sClient.Delete(ctx, svc)
			}
		})

		It("should inject database env vars into the Deployment", func() {
			By("Creating an Application with a postgres database and workload")
			replicas := int32(1)
			app := &platformv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: platformv1alpha1.ApplicationSpec{
					Team:  "platform",
					Owner: "test@example.com",
					Tier:  platformv1alpha1.TierStandard,
					Workload: &platformv1alpha1.WorkloadSpec{
						Image:    "nginx:latest",
						Replicas: &replicas,
						Ports: []platformv1alpha1.ContainerPort{
							{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
						},
					},
					Database: &platformv1alpha1.DatabaseSpec{
						Type:    platformv1alpha1.DatabasePostgres,
						Size:    platformv1alpha1.SizeSmall,
						Version: "16",
					},
				},
			}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			By("Reconciling (finalizer + resource creation)")
			reconciler := createReconciler()
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Deployment has injected env vars")
			deployment := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, deployment)
			}, timeout, interval).Should(Succeed())

			container := deployment.Spec.Template.Spec.Containers[0]
			names := containerEnvVarNames(container.Env)

			Expect(names).To(ContainElements(
				"DATABASE_URL", "PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE",
			))

			// Verify the secret name follows convention
			dbURL := findContainerEnvVar(container.Env, "DATABASE_URL")
			Expect(dbURL).NotTo(BeNil())
			Expect(dbURL.ValueFrom.SecretKeyRef.Name).To(Equal(resourceName + "-db-credentials"))
		})

		It("should not inject credentials when InjectCredentials is false", func() {
			By("Creating an Application with InjectCredentials=false")
			replicas := int32(1)
			injectFalse := false
			app := &platformv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: platformv1alpha1.ApplicationSpec{
					Team:  "platform",
					Owner: "test@example.com",
					Tier:  platformv1alpha1.TierStandard,
					Workload: &platformv1alpha1.WorkloadSpec{
						Image:             "nginx:latest",
						Replicas:          &replicas,
						InjectCredentials: &injectFalse,
						Ports: []platformv1alpha1.ContainerPort{
							{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
						},
					},
					Database: &platformv1alpha1.DatabaseSpec{
						Type:    platformv1alpha1.DatabasePostgres,
						Size:    platformv1alpha1.SizeSmall,
						Version: "16",
					},
				},
			}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			By("Reconciling")
			reconciler := createReconciler()
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Deployment has no injected env vars")
			deployment := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, deployment)
			}, timeout, interval).Should(Succeed())

			container := deployment.Spec.Template.Spec.Containers[0]
			Expect(container.Env).To(BeEmpty(), "no env vars should be present when injection is disabled")
		})

		It("should pass envFrom sources to the Deployment container", func() {
			By("Creating an Application with envFrom")
			replicas := int32(1)
			app := &platformv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: platformv1alpha1.ApplicationSpec{
					Team:  "platform",
					Owner: "test@example.com",
					Tier:  platformv1alpha1.TierStandard,
					Workload: &platformv1alpha1.WorkloadSpec{
						Image:    "nginx:latest",
						Replicas: &replicas,
						EnvFrom: []corev1.EnvFromSource{
							{
								SecretRef: &corev1.SecretEnvSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "external-api-keys",
									},
								},
							},
						},
						Ports: []platformv1alpha1.ContainerPort{
							{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			By("Reconciling")
			reconciler := createReconciler()
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Deployment container has envFrom")
			deployment := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, deployment)
			}, timeout, interval).Should(Succeed())

			container := deployment.Spec.Template.Spec.Containers[0]
			Expect(container.EnvFrom).To(HaveLen(1))
			Expect(container.EnvFrom[0].SecretRef).NotTo(BeNil())
			Expect(container.EnvFrom[0].SecretRef.Name).To(Equal("external-api-keys"))
		})

		It("should let user-defined env vars take precedence in the Deployment", func() {
			By("Creating an Application with a user-defined DATABASE_URL")
			replicas := int32(1)
			app := &platformv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: platformv1alpha1.ApplicationSpec{
					Team:  "platform",
					Owner: "test@example.com",
					Tier:  platformv1alpha1.TierStandard,
					Workload: &platformv1alpha1.WorkloadSpec{
						Image:    "nginx:latest",
						Replicas: &replicas,
						Env: []corev1.EnvVar{
							{Name: "DATABASE_URL", Value: "postgres://external:5432/mydb"},
						},
						Ports: []platformv1alpha1.ContainerPort{
							{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
						},
					},
					Database: &platformv1alpha1.DatabaseSpec{
						Type:    platformv1alpha1.DatabasePostgres,
						Size:    platformv1alpha1.SizeSmall,
						Version: "16",
					},
				},
			}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			By("Reconciling")
			reconciler := createReconciler()
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Deployment uses the user-defined DATABASE_URL")
			deployment := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, deployment)
			}, timeout, interval).Should(Succeed())

			container := deployment.Spec.Template.Spec.Containers[0]
			dbURL := findContainerEnvVar(container.Env, "DATABASE_URL")
			Expect(dbURL).NotTo(BeNil())
			Expect(dbURL.Value).To(Equal("postgres://external:5432/mydb"),
				"user-defined DATABASE_URL should win over auto-injected")
			Expect(dbURL.ValueFrom).To(BeNil(),
				"user-defined var should be a plain value, not a secretKeyRef")

			By("Verifying other PG vars are still injected")
			names := containerEnvVarNames(container.Env)
			Expect(names).To(ContainElements("PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE"))
		})
	})
})

// =============================================================================
// TEST HELPERS
// =============================================================================

// envVarNames extracts the names from a slice of EnvVars for easy assertion.
func envVarNames(vars []corev1.EnvVar) []string {
	names := make([]string, len(vars))
	for i, v := range vars {
		names[i] = v.Name
	}
	return names
}

// containerEnvVarNames is an alias for envVarNames — same type, different
// semantic context (container env vs function return).
func containerEnvVarNames(vars []corev1.EnvVar) []string {
	return envVarNames(vars)
}

// findEnvVar finds an env var by name in a slice. Returns the EnvVar or panics.
func findEnvVar(vars []corev1.EnvVar, name string) corev1.EnvVar {
	for _, v := range vars {
		if v.Name == name {
			return v
		}
	}
	panic(fmt.Sprintf("env var %q not found in %v", name, envVarNames(vars)))
}

// findContainerEnvVar finds an env var by name, returning a pointer (nil if not found).
func findContainerEnvVar(vars []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range vars {
		if vars[i].Name == name {
			return &vars[i]
		}
	}
	return nil
}
