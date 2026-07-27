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

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// =============================================================================
// REGRESSION: THE OPERATOR MUST NOT FIGHT ITS OWN HPA
// =============================================================================
//
// spec.workload.replicas and an HPA are two writers of the same field. Once an
// HPA exists for a Deployment, the HPA is the owner of spec.replicas: it writes
// through the scale subresource whenever load changes. If the operator also
// writes that field from spec.workload.replicas on every pass, the two fight:
//
//	HPA scales 1 -> 3  ->  Deployment update event  ->  Application re-enqueued
//	  ->  operator writes replicas back to 1  ->  HPA scales 1 -> 3  ->  ...
//
// Worse, the operator reported the HPA's own scale-up as EXTERNAL DRIFT
// ("DriftCorrected ... Deployment (replicas 3->1)"), so the condition that is
// supposed to mean "somebody edited our children behind our back" fired
// continuously on a perfectly healthy autoscaled app.
//
// envtest runs the apiserver only, with no kube-controller-manager, so no real
// HPA controller is present. The test writes spec.replicas directly, which is
// exactly the mutation the HPA's scale write produces on the stored object.
// =============================================================================

var _ = Describe("Application Controller - HPA-owned replicas", func() {

	const (
		timeout      = 10 * time.Second
		interval     = 250 * time.Millisecond
		resourceName = "test-app-hpa-scale"
		// What the HPA scales the Deployment up to. Above spec.workload.replicas
		// (1) and equal to the HPA's own minReplicas floor.
		hpaScaledTo = int32(3)
	)

	var (
		nn         types.NamespacedName
		recorder   *record.FakeRecorder
		reconciler *ApplicationReconciler
	)

	// drainEvents empties the FakeRecorder channel and returns what it held, so
	// a later assertion sees only the events of the reconcile under test.
	drainEvents := func() []string {
		var events []string
		for {
			select {
			case e := <-recorder.Events:
				events = append(events, e)
			default:
				return events
			}
		}
	}

	BeforeEach(func() {
		nn = types.NamespacedName{Name: resourceName, Namespace: "default"}

		replicas := int32(1)
		minReplicas := hpaScaledTo
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
				// The README's headline sample: HorizontalPodAutoscaler (3-12).
				Scaling: &platformv1alpha1.ScalingSpec{
					MinReplicas: &minReplicas,
					MaxReplicas: 12,
					Metrics: []platformv1alpha1.ScalingMetric{
						{Type: "cpu", Target: 70},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		recorder = record.NewFakeRecorder(100)
		reconciler = &ApplicationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: recorder,
		}

		// Converge: add the finalizer, create the children, and record
		// status.observedGeneration so later passes count as "spec unchanged".
		for i := 0; i < 3; i++ {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}

		Eventually(func() error {
			return k8sClient.Get(ctx, nn, &autoscalingv2.HorizontalPodAutoscaler{})
		}, timeout, interval).Should(Succeed())
	})

	AfterEach(func() {
		app := &platformv1alpha1.Application{}
		if err := k8sClient.Get(ctx, nn, app); err == nil {
			app.Finalizers = nil
			_ = k8sClient.Update(ctx, app)
			_ = k8sClient.Delete(ctx, app)
		}
		for _, obj := range []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace}},
			&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace}},
		} {
			_ = k8sClient.Delete(ctx, obj)
		}
	})

	It("does not revert a scale-up performed by the HPA", func() {
		By("confirming the reconciler has observed the current spec")
		app := &platformv1alpha1.Application{}
		Expect(k8sClient.Get(ctx, nn, app)).To(Succeed())
		Expect(app.Status.ObservedGeneration).To(Equal(app.Generation))

		By("letting the HPA scale the Deployment to its minReplicas floor")
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		scaled := hpaScaledTo
		deployment.Spec.Replicas = &scaled
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

		drainEvents()

		By("reconciling, as the owned-Deployment watch makes the operator do")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the operator left the HPA's replica count alone")
		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		Expect(deployment.Spec.Replicas).NotTo(BeNil())
		Expect(*deployment.Spec.Replicas).To(Equal(hpaScaledTo),
			"the operator overwrote the HPA's scale decision with spec.workload.replicas")

		By("verifying the HPA's own action was not reported as external drift")
		events := drainEvents()
		for _, e := range events {
			Expect(e).NotTo(ContainSubstring("DriftCorrected"),
				"an HPA scale event was mistaken for an external edit")
		}

		Expect(k8sClient.Get(ctx, nn, app)).To(Succeed())
		drift := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeDriftDetected)
		Expect(drift).NotTo(BeNil())
		Expect(drift.Status).To(Equal(metav1.ConditionFalse),
			"DriftDetected was raised for the HPA's own scale decision")
	})

	It("stays quiet across repeated reconciles after an HPA scale-up", func() {
		// The loop is what makes this severe: a single corrected pass would be a
		// cosmetic bug, but the write re-enqueues the Application through the
		// owned-Deployment watch, so it never settles. Reconciling several times
		// in a row must be a no-op on the replica count.
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		scaled := hpaScaledTo
		deployment.Spec.Replicas = &scaled
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

		for i := 0; i < 3; i++ {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
			Expect(*deployment.Spec.Replicas).To(Equal(hpaScaledTo))
		}
	})

	It("still corrects an image edit while the HPA owns the replica count", func() {
		// Skipping spec.replicas must not turn the Deployment into an
		// unmanaged object: every other field we own is still enforced.
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		scaled := hpaScaledTo
		deployment.Spec.Replicas = &scaled
		deployment.Spec.Template.Spec.Containers[0].Image = "nginx:tampered"
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

		drainEvents()

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:latest"))
		Expect(*deployment.Spec.Replicas).To(Equal(hpaScaledTo))

		app := &platformv1alpha1.Application{}
		Expect(k8sClient.Get(ctx, nn, app)).To(Succeed())
		drift := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeDriftDetected)
		Expect(drift).NotTo(BeNil())
		Expect(drift.Status).To(Equal(metav1.ConditionTrue))
		Expect(drift.Message).To(ContainSubstring("image changed"))
		Expect(drift.Message).NotTo(ContainSubstring("replicas"))
	})

	It("takes the replica count back in the pass that removes the scaling block", func() {
		// Ownership has to hand back cleanly, and in one pass. reconcileHPA
		// deletes our HPA further down the same reconcile, so the deployment step
		// must already ignore that doomed HPA rather than defer to it and leave
		// the workload parked at the autoscaler's last count for a pass.
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		scaled := hpaScaledTo
		deployment.Spec.Replicas = &scaled
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

		By("removing spec.scaling")
		app := &platformv1alpha1.Application{}
		Expect(k8sClient.Get(ctx, nn, app)).To(Succeed())
		app.Spec.Scaling = nil
		Expect(k8sClient.Update(ctx, app)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the HPA is gone and spec.workload.replicas applies again")
		Expect(k8sClient.Get(ctx, nn, &autoscalingv2.HorizontalPodAutoscaler{})).NotTo(Succeed())
		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))
	})
})

// =============================================================================
// THE OTHER HALF: NOBODY ELSE OWNS THE REPLICA COUNT
// =============================================================================
//
// Yielding spec.replicas is conditional. With no autoscaler in the picture the
// operator is still the owner of the field and a hand edit is still drift, so
// these specs pin the boundary from both sides: a third-party autoscaler (KEDA
// materializes its ScaledObject as a plain HPA that we do not own) is respected,
// and a bare `kubectl scale` is not.
// =============================================================================

var _ = Describe("Application Controller - replica ownership boundary", func() {

	const (
		timeout      = 10 * time.Second
		interval     = 250 * time.Millisecond
		resourceName = "test-app-no-scaling"
	)

	var (
		nn         types.NamespacedName
		recorder   *record.FakeRecorder
		reconciler *ApplicationReconciler
	)

	BeforeEach(func() {
		nn = types.NamespacedName{Name: resourceName, Namespace: "default"}

		replicas := int32(2)
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
				},
				// No Scaling block: the operator owns spec.replicas.
			},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		recorder = record.NewFakeRecorder(100)
		reconciler = &ApplicationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: recorder,
		}
		for i := 0; i < 3; i++ {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}
		Eventually(func() error {
			return k8sClient.Get(ctx, nn, &appsv1.Deployment{})
		}, timeout, interval).Should(Succeed())
	})

	AfterEach(func() {
		app := &platformv1alpha1.Application{}
		if err := k8sClient.Get(ctx, nn, app); err == nil {
			app.Finalizers = nil
			_ = k8sClient.Update(ctx, app)
			_ = k8sClient.Delete(ctx, app)
		}
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
		})
		_ = k8sClient.Delete(ctx, &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "keda-hpa-" + resourceName, Namespace: nn.Namespace},
		})
	})

	It("restores the replica count after a manual scale when no HPA exists", func() {
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))

		By("scaling the Deployment by hand, as `kubectl scale` would")
		hand := int32(7)
		deployment.Spec.Replicas = &hand
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))

		app := &platformv1alpha1.Application{}
		Expect(k8sClient.Get(ctx, nn, app)).To(Succeed())
		drift := meta.FindStatusCondition(app.Status.Conditions, platformv1alpha1.ConditionTypeDriftDetected)
		Expect(drift).NotTo(BeNil())
		Expect(drift.Status).To(Equal(metav1.ConditionTrue))
		Expect(drift.Message).To(ContainSubstring("replicas 7->2"))
	})

	It("yields to a third-party HPA that targets the same Deployment", func() {
		By("standing up an HPA we do not own, the way KEDA does")
		kedaMin := int32(4)
		kedaHPA := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "keda-hpa-" + resourceName,
				Namespace: nn.Namespace,
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       resourceName,
				},
				MinReplicas: &kedaMin,
				MaxReplicas: 20,
			},
		}
		Expect(k8sClient.Create(ctx, kedaHPA)).To(Succeed())

		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		scaled := int32(6)
		deployment.Spec.Replicas = &scaled
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, deployment)).To(Succeed())
		Expect(*deployment.Spec.Replicas).To(Equal(int32(6)),
			"the operator overwrote a third-party autoscaler's scale decision")
	})
})
