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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// =============================================================================
// UNIT: WATCH PREDICATE AND INITIAL REPLICA COUNT
// =============================================================================

var _ = Describe("isScaleOnlyUpdate", func() {

	// baseDeployment returns a Deployment with the fields a scale write leaves
	// untouched already populated, so each spec only varies what it is about.
	baseDeployment := func(replicas int32) *appsv1.Deployment {
		r := replicas
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "app",
				Namespace:       "default",
				Generation:      4,
				ResourceVersion: "100",
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &r,
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
					},
				},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1},
		}
	}

	It("filters an update whose only change is the replica count", func() {
		oldDeploy := baseDeployment(1)
		newDeploy := baseDeployment(3)
		// What the apiserver rewrites on any write.
		newDeploy.Generation = 5
		newDeploy.ResourceVersion = "101"

		Expect(isScaleOnlyUpdate(oldDeploy, newDeploy)).To(BeTrue())
	})

	It("keeps an update that also moves readiness", func() {
		oldDeploy := baseDeployment(1)
		newDeploy := baseDeployment(3)
		newDeploy.Status.ReadyReplicas = 3
		newDeploy.Status.AvailableReplicas = 3

		Expect(isScaleOnlyUpdate(oldDeploy, newDeploy)).To(BeFalse())
	})

	It("keeps an update that changes the pod template", func() {
		oldDeploy := baseDeployment(1)
		newDeploy := baseDeployment(3)
		newDeploy.Spec.Template.Spec.Containers[0].Image = "nginx:tampered"

		Expect(isScaleOnlyUpdate(oldDeploy, newDeploy)).To(BeFalse())
	})

	It("keeps an update that leaves the replica count alone", func() {
		oldDeploy := baseDeployment(3)
		newDeploy := baseDeployment(3)
		newDeploy.Status.ReadyReplicas = 3

		Expect(isScaleOnlyUpdate(oldDeploy, newDeploy)).To(BeFalse())
	})

	It("routes through the builder predicate", func() {
		oldDeploy := baseDeployment(1)
		newDeploy := baseDeployment(3)

		Expect(ignoreScaleOnlyUpdates().Update(event.UpdateEvent{
			ObjectOld: oldDeploy,
			ObjectNew: newDeploy,
		})).To(BeFalse())

		// A non-Deployment object must never be filtered.
		Expect(ignoreScaleOnlyUpdates().Update(event.UpdateEvent{
			ObjectOld: &corev1.Service{},
			ObjectNew: &corev1.Service{},
		})).To(BeTrue())
	})
})

var _ = Describe("initialReplicas", func() {

	appWith := func(replicas *int32, scaling *platformv1alpha1.ScalingSpec) *platformv1alpha1.Application {
		return &platformv1alpha1.Application{
			Spec: platformv1alpha1.ApplicationSpec{
				Workload: &platformv1alpha1.WorkloadSpec{Image: "nginx:latest", Replicas: replicas},
				Scaling:  scaling,
			},
		}
	}
	ptr := func(v int32) *int32 { return &v }

	It("uses spec.workload.replicas when nothing is autoscaling", func() {
		Expect(initialReplicas(appWith(ptr(4), nil))).To(Equal(int32(4)))
	})

	It("defaults to one when replicas is unset", func() {
		Expect(initialReplicas(appWith(nil, nil))).To(Equal(int32(1)))
	})

	It("starts at the autoscaling floor rather than below it", func() {
		scaling := &platformv1alpha1.ScalingSpec{MinReplicas: ptr(3), MaxReplicas: 12}
		Expect(initialReplicas(appWith(ptr(1), scaling))).To(Equal(int32(3)))
	})

	It("never starts above the autoscaling ceiling", func() {
		scaling := &platformv1alpha1.ScalingSpec{MinReplicas: ptr(1), MaxReplicas: 5}
		Expect(initialReplicas(appWith(ptr(9), scaling))).To(Equal(int32(5)))
	})

	It("leaves a count already inside the range alone", func() {
		scaling := &platformv1alpha1.ScalingSpec{MinReplicas: ptr(2), MaxReplicas: 8}
		Expect(initialReplicas(appWith(ptr(4), scaling))).To(Equal(int32(4)))
	})
})
