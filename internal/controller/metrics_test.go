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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// =============================================================================
// TESTS: Custom Prometheus Metrics
// =============================================================================

var _ = Describe("Controller Metrics", func() {
	Context("reconcileDuration", func() {
		It("records reconcile duration without panic", func() {
			// Histograms don't support ToFloat64 on individual label sets.
			// We verify the metric is registered and can be observed.
			Expect(func() {
				recordReconcileDuration("test-app", "default", "success", 0.5)
			}).NotTo(Panic())
		})

		It("records multiple observations", func() {
			Expect(func() {
				recordReconcileDuration("multi-app", "ns1", "success", 0.1)
				recordReconcileDuration("multi-app", "ns1", "error", 1.5)
				recordReconcileDuration("multi-app", "ns1", "not_found", 0.01)
			}).NotTo(Panic())
		})
	})

	Context("applicationPhaseGauge", func() {
		It("sets phase to 1 for current phase", func() {
			setApplicationPhase("test-app", "default", "Ready", "standard", "platform")

			val := testutil.ToFloat64(applicationPhaseGauge.WithLabelValues("test-app", "default", "Ready", "standard", "platform"))
			Expect(val).To(Equal(float64(1)))
		})

		It("clears previous phase when setting new phase", func() {
			setApplicationPhase("test-app2", "default", "Provisioning", "standard", "platform")
			setApplicationPhase("test-app2", "default", "Ready", "standard", "platform")

			// Previous phase should be deleted
			provVal := testutil.ToFloat64(applicationPhaseGauge.WithLabelValues("test-app2", "default", "Provisioning", "standard", "platform"))
			Expect(provVal).To(Equal(float64(0)))

			// Current phase should be 1
			readyVal := testutil.ToFloat64(applicationPhaseGauge.WithLabelValues("test-app2", "default", "Ready", "standard", "platform"))
			Expect(readyVal).To(Equal(float64(1)))
		})
	})

	Context("deleteApplicationMetrics", func() {
		It("removes all phase metrics for an application", func() {
			setApplicationPhase("delete-me", "default", "Ready", "critical", "payments")
			deleteApplicationMetrics("delete-me", "default", "critical", "payments")

			val := testutil.ToFloat64(applicationPhaseGauge.WithLabelValues("delete-me", "default", "Ready", "critical", "payments"))
			Expect(val).To(Equal(float64(0)))
		})
	})

	Context("reconcileErrorsTotal", func() {
		It("increments error counter", func() {
			before := testutil.ToFloat64(reconcileErrorsTotal.WithLabelValues("err-app", "default", "deployment"))
			incrementReconcileErrors("err-app", "default", "deployment")
			after := testutil.ToFloat64(reconcileErrorsTotal.WithLabelValues("err-app", "default", "deployment"))
			Expect(after).To(Equal(before + 1))
		})
	})

	Context("managedResourcesGauge", func() {
		It("sets managed resource count", func() {
			setManagedResources("default", "Deployment", 5)
			val := testutil.ToFloat64(managedResourcesGauge.WithLabelValues("default", "Deployment"))
			Expect(val).To(Equal(float64(5)))
		})

		It("updates managed resource count", func() {
			setManagedResources("production", "Service", 3)
			setManagedResources("production", "Service", 7)
			val := testutil.ToFloat64(managedResourcesGauge.WithLabelValues("production", "Service"))
			Expect(val).To(Equal(float64(7)))
		})
	})

	Context("applicationTotal", func() {
		It("sets the Application total for a tier", func() {
			setApplicationTotal("metrics-ns", "critical", 2)
			val := testutil.ToFloat64(applicationTotal.WithLabelValues("metrics-ns", "critical"))
			Expect(val).To(Equal(float64(2)))
		})

		It("resets a tier to zero so emptied tiers do not stay stuck", func() {
			setApplicationTotal("metrics-ns2", "standard", 4)
			setApplicationTotal("metrics-ns2", "standard", 0)
			val := testutil.ToFloat64(applicationTotal.WithLabelValues("metrics-ns2", "standard"))
			Expect(val).To(Equal(float64(0)))
		})
	})
})
