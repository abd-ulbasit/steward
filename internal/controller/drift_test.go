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

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
	"github.com/abd-ulbasit/goplatform/internal/provider"
)

// =============================================================================
// TESTS: Drift Detection & Self-Healing (Milestone 9, envtest)
// =============================================================================
//
// These prove the reconcile loop restores child resources after external
// tampering and flags it via the DriftDetected condition. They reconcile to
// steady state FIRST (ObservedGeneration == Generation) so the drift signal —
// "a child changed while the spec did not" — is exercised authentically.
// =============================================================================

var _ = Describe("Drift detection and self-healing", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
		ns       = "drift-test"
	)

	newReconciler := func() *ApplicationReconciler {
		factory := provider.NewFactory()
		factory.SetProvider(provider.NewMockProvider(nil))
		return &ApplicationReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			Recorder:        record.NewFakeRecorder(100),
			ProviderFactory: factory,
		}
	}

	// reconcileToSteadyState drives the loop until the controller has fully
	// reconciled the current generation (ObservedGeneration == Generation). After
	// this point, any further child create/update means external drift.
	reconcileToSteadyState := func(r *ApplicationReconciler, nn types.NamespacedName) {
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(err).NotTo(HaveOccurred())
			var got platformv1alpha1.Application
			g.Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
			g.Expect(got.Generation).To(BeNumerically(">", 0))
			g.Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		}, timeout, interval).Should(Succeed())
	}

	newApp := func(name string) (*platformv1alpha1.Application, types.NamespacedName) {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		replicas := int32(1)
		app := &platformv1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: platformv1alpha1.ApplicationSpec{
				Team:  "platform",
				Owner: "test@example.com",
				Tier:  platformv1alpha1.TierStandard,
				Workload: &platformv1alpha1.WorkloadSpec{
					Image:    "nginx:latest",
					Replicas: &replicas,
					Ports: []platformv1alpha1.ContainerPort{
						{Name: "http", ContainerPort: 8080},
					},
				},
			},
		}
		return app, nn
	}

	BeforeEach(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		if err != nil {
			Expect(err).To(MatchError(ContainSubstring("already exists")))
		}
	})

	It("corrects an externally modified Deployment and flags DriftDetected with field-level detail", func() {
		reconciler := newReconciler()
		app, nn := newApp("drift-modify")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		reconcileToSteadyState(reconciler, nn)

		// Simulate an operator scaling the Deployment by hand.
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, nn, &dep)).To(Succeed())
		tampered := int32(99)
		dep.Spec.Replicas = &tampered
		Expect(k8sClient.Update(ctx, &dep)).To(Succeed())

		// A single reconcile (spec unchanged) should restore desired state...
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, &dep)).To(Succeed())
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)), "drifted replicas should be restored")

		// ...and flag DriftDetected=True with a field-level message.
		var got platformv1alpha1.Application
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, platformv1alpha1.ConditionTypeDriftDetected)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring("replicas 99->1"))

		// The next clean reconcile should clear the condition back to False.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		cond = meta.FindStatusCondition(got.Status.Conditions, platformv1alpha1.ConditionTypeDriftDetected)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	})

	It("recreates a deleted child Service", func() {
		reconciler := newReconciler()
		app, nn := newApp("drift-delete")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		reconcileToSteadyState(reconciler, nn)

		// Service exists, then gets deleted out-of-band.
		var svc corev1.Service
		Expect(k8sClient.Get(ctx, nn, &svc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &svc)).To(Succeed())
		Eventually(func() bool {
			return k8sClient.Get(ctx, nn, &corev1.Service{}) != nil
		}, timeout, interval).Should(BeTrue(), "service should be gone before recovery")

		// Reconcile should recreate it.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			return k8sClient.Get(ctx, nn, &corev1.Service{})
		}, timeout, interval).Should(Succeed(), "service should be recreated")
	})

	AfterEach(func() {
		for _, name := range []string{"drift-modify", "drift-delete"} {
			app := &platformv1alpha1.Application{}
			nn := types.NamespacedName{Name: name, Namespace: ns}
			if err := k8sClient.Get(ctx, nn, app); err == nil {
				app.Finalizers = nil
				_ = k8sClient.Update(ctx, app)
				_ = k8sClient.Delete(ctx, app)
			}
		}
	})
})
