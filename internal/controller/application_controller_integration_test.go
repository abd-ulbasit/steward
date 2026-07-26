package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
	"github.com/abd-ulbasit/goplatform/internal/provider"
)

// =============================================================================
// INTEGRATION TESTS: CONTROLLER ↔ PROVIDER FLOW
// =============================================================================
//
// These tests validate the complete reconciliation flow when infrastructure
// is requested. Each test configures a MockProvider with specific behavior,
// creates an Application, and verifies the controller correctly:
//   - Calls Provision()/Destroy() at the right times
//   - Maps ResourceState to Application status conditions
//   - Handles errors (retryable vs terminal) appropriately
//   - Populates status.infrastructure with connection info
//
// WHY MOCK (not real KubernetesProvider):
//   - Deterministic: No waiting for real CNPG pods
//   - Fast: Tests complete in milliseconds, not minutes
//   - Controllable: Can inject specific errors and states
//   - Focused: Tests controller logic, not provider logic
//   Real cluster validation is done by hack/validate-e2e.sh
//
// =============================================================================

var _ = Describe("Application Controller - Infrastructure Integration", func() {

	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	// createReconcilerWithMock creates a reconciler wired to a MockProvider
	// through the Factory. Returns both so tests can configure mock behavior
	// and inspect call records.
	createReconcilerWithMock := func() (*ApplicationReconciler, *provider.MockProvider) {
		mock := provider.NewMockProvider(nil)
		factory := provider.NewFactory()
		factory.SetProvider(mock)

		reconciler := &ApplicationReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			Recorder:        record.NewFakeRecorder(100),
			ProviderFactory: factory,
		}
		return reconciler, mock
	}

	// createInfraApp creates an Application with a database spec for infra testing.
	// Uses a unique name per test to avoid collisions.
	createInfraApp := func(name string) (*platformv1alpha1.Application, types.NamespacedName) {
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		app := &platformv1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Spec: platformv1alpha1.ApplicationSpec{
				Team:  "platform",
				Owner: "test@example.com",
				Tier:  platformv1alpha1.TierStandard,
				Database: &platformv1alpha1.DatabaseSpec{
					Type:    platformv1alpha1.DatabasePostgres,
					Size:    platformv1alpha1.SizeSmall,
					Version: "16",
				},
			},
		}
		return app, nn
	}

	// reconcileUntilStable runs Reconcile until two consecutive passes return the
	// same result, then returns that result. Reconcile is idempotent, so a stable
	// result means the loop has converged.
	//
	// This deliberately does not inspect Result.Requeue (deprecated in
	// controller-runtime v0.20): the reconciler now adds the finalizer and
	// provisions in a single pass, so there is no "immediate requeue" signal to
	// key off any more.
	reconcileUntilStable := func(r *ApplicationReconciler, nn types.NamespacedName) (reconcile.Result, error) {
		var result, previous reconcile.Result
		var err error
		for i := 0; i < 5; i++ {
			result, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			if err != nil {
				return result, err
			}
			if i > 0 && result == previous {
				return result, nil
			}
			previous = result
		}
		return result, err
	}

	// cleanupApp removes an Application and its finalizer for test teardown.
	cleanupApp := func(nn types.NamespacedName) {
		app := &platformv1alpha1.Application{}
		if err := k8sClient.Get(ctx, nn, app); err == nil {
			app.Finalizers = nil
			_ = k8sClient.Update(ctx, app)
			_ = k8sClient.Delete(ctx, app)
		}
	}

	// =========================================================================
	// TEST: INFRASTRUCTURE PROVISIONING HAPPY PATH
	// =========================================================================
	//
	// Validates the full lifecycle:
	//   1. Create Application with database spec
	//   2. MockProvider returns Ready state (no delay configured)
	//   3. Controller maps state to conditions
	//   4. Application reaches phase=Ready
	//
	// =========================================================================

	Context("When provisioning infrastructure successfully", func() {
		const appName = "test-infra-happy"
		var nn types.NamespacedName

		BeforeEach(func() {
			app, namespacedName := createInfraApp(appName)
			nn = namespacedName
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx, nn, &platformv1alpha1.Application{})
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			cleanupApp(nn)
		})

		It("should provision and reach Ready phase", func() {
			reconciler, mock := createReconcilerWithMock()

			By("Reconciling until stable (adds finalizer, calls Provision)")
			result, err := reconcileUntilStable(reconciler, nn)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying provider was called")
			Expect(mock.ProvisionCallCount()).To(BeNumerically(">=", 1))

			By("Verifying Application status reflects Ready infrastructure")
			var app platformv1alpha1.Application
			Expect(k8sClient.Get(ctx, nn, &app)).To(Succeed())

			dbCond := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeDatabaseReady)
			Expect(dbCond).NotTo(BeNil())
			Expect(dbCond.Status).To(Equal(metav1.ConditionTrue))

			readyCond := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

			Expect(app.Status.Phase).To(Equal(platformv1alpha1.ApplicationReady))

			By("Verifying status.infrastructure populated with connection info")
			Expect(app.Status.Infrastructure).NotTo(BeNil())
			Expect(app.Status.Infrastructure.Database).NotTo(BeNil())
			// MockProvider generates: "{appName}-db.mock.local"
			Expect(app.Status.Infrastructure.Database.Endpoint).To(Equal("test-infra-happy-db.mock.local"))
			Expect(app.Status.Infrastructure.Database.Port).To(Equal(int32(5432)))

			By("Verifying requeue is set for periodic resync")
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
		})
	})

	// =========================================================================
	// TEST: PARTIAL INFRASTRUCTURE READINESS
	// =========================================================================
	//
	// When one component is Ready and another is Provisioning,
	// the overall Ready condition should be False.
	//
	// =========================================================================

	Context("When infrastructure is partially ready", func() {
		const appName = "test-infra-partial"
		var nn types.NamespacedName

		BeforeEach(func() {
			nn = types.NamespacedName{Name: appName, Namespace: "default"}
			app := &platformv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      appName,
					Namespace: "default",
				},
				Spec: platformv1alpha1.ApplicationSpec{
					Team:  "platform",
					Owner: "test@example.com",
					Tier:  platformv1alpha1.TierStandard,
					Database: &platformv1alpha1.DatabaseSpec{
						Type:    platformv1alpha1.DatabasePostgres,
						Size:    platformv1alpha1.SizeSmall,
						Version: "16",
					},
					Cache: &platformv1alpha1.CacheSpec{
						Type: platformv1alpha1.CacheRedis,
						Size: platformv1alpha1.SizeSmall,
					},
				},
			}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx, nn, &platformv1alpha1.Application{})
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			cleanupApp(nn)
		})

		It("should report partial readiness in conditions", func() {
			reconciler, mock := createReconcilerWithMock()

			// Use a large ProvisionDelay so Provision() returns state as-is
			// without calling updateStateToReady(). This lets us inject mixed
			// state via SetState after the initial provisioning call.
			mock.ProvisionDelay = 10 * time.Minute

			By("Reconciling to add finalizer and trigger first Provision (all Provisioning)")
			_, err := reconcileUntilStable(reconciler, nn)
			Expect(err).NotTo(HaveOccurred())

			By("Overriding mock state to: DB Ready, Cache Provisioning")
			app := &platformv1alpha1.Application{}
			Expect(k8sClient.Get(ctx, nn, app)).To(Succeed())
			mock.SetState(app, &provider.ResourceState{
				Database: &provider.DatabaseState{
					Phase:    provider.ResourceReady,
					Endpoint: "db.mock.local",
					Port:     5432,
					Message:  "Database is ready",
				},
				Cache: &provider.CacheState{
					Phase:   provider.ResourceProvisioning,
					Message: "Cache is being provisioned...",
				},
			})

			By("Reconciling again to pick up the mixed state")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying database is Ready but cache is not")
			Expect(k8sClient.Get(ctx, nn, app)).To(Succeed())

			dbCond := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeDatabaseReady)
			Expect(dbCond).NotTo(BeNil())
			Expect(dbCond.Status).To(Equal(metav1.ConditionTrue))

			cacheCond := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeCacheReady)
			Expect(cacheCond).NotTo(BeNil())
			Expect(cacheCond.Status).To(Equal(metav1.ConditionFalse))

			By("Verifying overall Ready is False")
			readyCond := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))

			Expect(app.Status.Phase).To(Equal(platformv1alpha1.ApplicationProvisioning))
		})
	})

	// =========================================================================
	// TEST: INFRASTRUCTURE PROVISIONING FAILURE (InvalidConfigError)
	// =========================================================================

	Context("When provisioning fails with InvalidConfigError", func() {
		const appName = "test-infra-invalid"
		var nn types.NamespacedName

		BeforeEach(func() {
			app, namespacedName := createInfraApp(appName)
			nn = namespacedName
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx, nn, &platformv1alpha1.Application{})
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			cleanupApp(nn)
		})

		It("should set phase to Failed and not requeue", func() {
			reconciler, mock := createReconcilerWithMock()
			mock.ProvisionError = &provider.InvalidConfigError{
				Field:   "database.type",
				Value:   "postgres",
				Message: "CNPG operator CRD not found",
			}

			By("Reconciling")
			result, err := reconcileUntilStable(reconciler, nn)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no requeue (terminal failure)")
			Expect(result.RequeueAfter).To(BeZero())

			By("Verifying phase is Failed")
			var app platformv1alpha1.Application
			Expect(k8sClient.Get(ctx, nn, &app)).To(Succeed())
			Expect(app.Status.Phase).To(Equal(platformv1alpha1.ApplicationFailed))
		})
	})

	// =========================================================================
	// TEST: RETRYABLE ERROR
	// =========================================================================

	Context("When provisioning encounters a retryable error", func() {
		const appName = "test-infra-retry"
		var nn types.NamespacedName

		BeforeEach(func() {
			app, namespacedName := createInfraApp(appName)
			nn = namespacedName
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx, nn, &platformv1alpha1.Application{})
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			cleanupApp(nn)
		})

		It("should requeue with shorter interval", func() {
			reconciler, mock := createReconcilerWithMock()
			mock.ProvisionError = &provider.ProvisioningError{
				ResourceType: "database",
				Message:      "temporary connection issue",
			}

			By("Reconciling")
			result, err := reconcileUntilStable(reconciler, nn)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying requeue with error interval (10s)")
			Expect(result.RequeueAfter).To(Equal(10 * time.Second))
		})
	})

	// =========================================================================
	// TEST: DESTROY ON DELETE
	// =========================================================================

	Context("When deleting an Application with infrastructure", func() {
		const appName = "test-infra-destroy"
		var nn types.NamespacedName

		BeforeEach(func() {
			app, namespacedName := createInfraApp(appName)
			nn = namespacedName
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx, nn, &platformv1alpha1.Application{})
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			cleanupApp(nn)
		})

		It("should call Destroy before removing finalizer", func() {
			reconciler, mock := createReconcilerWithMock()

			By("Reconciling to add finalizer and provision")
			_, err := reconcileUntilStable(reconciler, nn)
			Expect(err).NotTo(HaveOccurred())

			By("Deleting the Application")
			var app platformv1alpha1.Application
			Expect(k8sClient.Get(ctx, nn, &app)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &app)).To(Succeed())

			By("Reconciling the deletion")
			// handleDeletion stamps the deletion-start annotation and runs provider
			// cleanup in the same pass, so a single reconcile finishes the flow.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Destroy was called")
			Expect(mock.DestroyCallCount()).To(BeNumerically(">=", 1))

			By("Verifying finalizer was removed (object can be deleted)")
			// The object should either be gone or have no finalizer
			getErr := k8sClient.Get(ctx, nn, &app)
			if getErr == nil {
				Expect(app.Finalizers).NotTo(ContainElement(applicationFinalizer))
			}
			// If NotFound, that's fine too — means deletion completed
		})
	})

	// =========================================================================
	// TEST: NO INFRASTRUCTURE REQUESTED
	// =========================================================================

	Context("When no infrastructure is requested", func() {
		const appName = "test-no-infra"
		var nn types.NamespacedName

		BeforeEach(func() {
			nn = types.NamespacedName{Name: appName, Namespace: "default"}
			app := &platformv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      appName,
					Namespace: "default",
				},
				Spec: platformv1alpha1.ApplicationSpec{
					Team:  "platform",
					Owner: "test@example.com",
					Tier:  platformv1alpha1.TierStandard,
					// No database, cache, queue, or storage
				},
			}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx, nn, &platformv1alpha1.Application{})
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			cleanupApp(nn)
		})

		It("should not call the provider and mark infra conditions as NotRequested", func() {
			reconciler, mock := createReconcilerWithMock()

			By("Reconciling")
			_, err := reconcileUntilStable(reconciler, nn)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying provider was NOT called")
			Expect(mock.ProvisionCallCount()).To(Equal(0))

			By("Verifying infra conditions show NotRequested")
			var app platformv1alpha1.Application
			Expect(k8sClient.Get(ctx, nn, &app)).To(Succeed())

			dbCond := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeDatabaseReady)
			Expect(dbCond).NotTo(BeNil())
			Expect(dbCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(dbCond.Reason).To(Equal("DatabaseNotRequested"))
		})
	})

	// =========================================================================
	// TEST: STATUS MAPPING PIPELINE
	// =========================================================================

	Context("When provider returns full ResourceState", func() {
		const appName = "test-status-mapping"
		var nn types.NamespacedName

		BeforeEach(func() {
			app, namespacedName := createInfraApp(appName)
			nn = namespacedName
			Expect(k8sClient.Create(ctx, app)).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx, nn, &platformv1alpha1.Application{})
			}, timeout, interval).Should(Succeed())
		})

		AfterEach(func() {
			cleanupApp(nn)
		})

		It("should populate status.infrastructure with connection details", func() {
			reconciler, _ := createReconcilerWithMock()

			// With ProvisionDelay=0 (default), the mock immediately transitions
			// to Ready state with deterministic values we can assert against.
			// MockProvider generates:
			//   Endpoint: "{appName}-db.mock.local"
			//   Port:     5432
			//   SecretRef: "{appName}-database-credentials"

			By("Reconciling")
			_, err := reconcileUntilStable(reconciler, nn)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying exact status.infrastructure fields")
			app := &platformv1alpha1.Application{}
			Expect(k8sClient.Get(ctx, nn, app)).To(Succeed())
			Expect(app.Status.Infrastructure).NotTo(BeNil())
			Expect(app.Status.Infrastructure.Database).NotTo(BeNil())
			Expect(app.Status.Infrastructure.Database.Endpoint).To(Equal("test-status-mapping-db.mock.local"))
			Expect(app.Status.Infrastructure.Database.Port).To(Equal(int32(5432)))
			Expect(app.Status.Infrastructure.Database.SecretRef).NotTo(BeNil())
			Expect(app.Status.Infrastructure.Database.SecretRef.Name).To(Equal("test-status-mapping-database-credentials"))
		})
	})
})
