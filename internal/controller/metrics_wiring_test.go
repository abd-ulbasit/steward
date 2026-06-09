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
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/abd-ulbasit/goplatform/api/v1alpha1"
	"github.com/abd-ulbasit/goplatform/internal/provider"
)

// =============================================================================
// TESTS: Aggregate Metric Wiring (envtest)
// =============================================================================
//
// The unit tests in metrics_test.go exercise the metric *setters* directly.
// They prove the gauges accept values — but they would still pass even if the
// reconcile loop never called them. That is the exact "tested but not
// integrated" trap that left managedResourcesGauge and applicationTotal stuck
// at zero in a live cluster.
//
// This spec closes that gap end-to-end: it creates a real Application through
// the API server (envtest), drives the reconcile loop, and asserts the
// aggregate gauges reflect actual cluster state. We isolate the assertions in a
// dedicated namespace so concurrent specs (which use "default", "production",
// etc.) cannot perturb the counts.
// =============================================================================

var _ = Describe("Aggregate metric wiring", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
		ns       = "metrics-wiring"
		appName  = "metric-app"
	)

	nn := types.NamespacedName{Name: appName, Namespace: ns}

	// newReconciler builds a reconciler wired to a MockProvider. The provider is
	// never exercised here (the test Application requests no infrastructure), but
	// wiring it keeps the reconciler shape identical to production.
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

	// driveReconcile runs the loop a few times. The first pass adds the finalizer
	// and requeues; later passes create the child resources and recompute the
	// gauges. Reconcile is idempotent, so extra passes are harmless — we avoid
	// inspecting the deprecated Result.Requeue field and just run enough passes.
	driveReconcile := func(r *ApplicationReconciler) {
		for i := 0; i < 3; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	BeforeEach(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
		// Namespaces are not deleted between specs in envtest; tolerate re-create.
		if err != nil {
			Expect(err).To(MatchError(ContainSubstring("already exists")))
		}
	})

	AfterEach(func() {
		app := &platformv1alpha1.Application{}
		if err := k8sClient.Get(ctx, nn, app); err == nil {
			app.Finalizers = nil
			_ = k8sClient.Update(ctx, app)
			_ = k8sClient.Delete(ctx, app)
		}
	})

	It("populates managed-resource and Application-total gauges through the reconcile loop", func() {
		reconciler := newReconciler()

		replicas := int32(1)
		app := &platformv1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: ns},
			Spec: platformv1alpha1.ApplicationSpec{
				Team:  "platform",
				Owner: "test@example.com",
				Tier:  platformv1alpha1.TierCritical,
				Workload: &platformv1alpha1.WorkloadSpec{
					Image:    "nginx:latest",
					Replicas: &replicas,
					// A named port makes the controller emit a Service too, so we can
					// assert both managed-resource kinds.
					Ports: []platformv1alpha1.ContainerPort{
						{Name: "http", ContainerPort: 8080},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		// The Deployment is created on the same reconcile pass that recomputes the
		// gauges, so the count is eventually 1. We re-reconcile inside Eventually
		// to ride out the finalizer-add requeue and any creation latency.
		Eventually(func() float64 {
			driveReconcile(reconciler)
			return testutil.ToFloat64(managedResourcesGauge.WithLabelValues(ns, "Deployment"))
		}, timeout, interval).Should(Equal(float64(1)), "Deployment gauge should reflect the one managed Deployment")

		Expect(testutil.ToFloat64(managedResourcesGauge.WithLabelValues(ns, "Service"))).
			To(Equal(float64(1)), "Service gauge should reflect the one managed Service")

		// Exactly one critical Application; the other tiers must be explicitly zero
		// (proving the "don't leave emptied tiers stuck" reset logic runs).
		Expect(testutil.ToFloat64(applicationTotal.WithLabelValues(ns, "critical"))).To(Equal(float64(1)))
		Expect(testutil.ToFloat64(applicationTotal.WithLabelValues(ns, "standard"))).To(Equal(float64(0)))
		Expect(testutil.ToFloat64(applicationTotal.WithLabelValues(ns, "development"))).To(Equal(float64(0)))
	})
})
