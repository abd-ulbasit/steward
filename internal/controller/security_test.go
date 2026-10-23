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
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// =============================================================================
// UNIT: SECURITY SETTINGS OF THE GENERATED DEPLOYMENT
// =============================================================================

var _ = Describe("workload security defaults", func() {
	build := func(security *platformv1alpha1.WorkloadSecuritySpec) *appsv1.Deployment {
		app := &platformv1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: platformv1alpha1.ApplicationSpec{
				Team:  "platform",
				Owner: "platform@example.com",
				Workload: &platformv1alpha1.WorkloadSpec{
					// The examples ship root images; the defaults must not
					// stop them from starting.
					Image:    "nginx:latest",
					Security: security,
				},
			},
		}
		r := &ApplicationReconciler{}
		return r.buildDeployment(app)
	}

	It("applies the baseline settings when no security block is given", func() {
		d := build(nil)
		pod := d.Spec.Template.Spec
		c := pod.Containers[0]

		Expect(pod.SecurityContext).NotTo(BeNil())
		Expect(pod.SecurityContext.SeccompProfile).To(Equal(
			&corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}))
		Expect(pod.AutomountServiceAccountToken).To(Equal(ptr.To(false)))
		Expect(c.SecurityContext).NotTo(BeNil())
		Expect(c.SecurityContext.AllowPrivilegeEscalation).To(Equal(ptr.To(false)))

		// Nothing that depends on how the image was built.
		Expect(pod.SecurityContext.RunAsNonRoot).To(BeNil())
		Expect(pod.SecurityContext.RunAsUser).To(BeNil())
		Expect(c.SecurityContext.Capabilities).To(BeNil())
		Expect(c.SecurityContext.ReadOnlyRootFilesystem).To(BeNil())
	})

	It("adds runAsNonRoot and drops all capabilities under restricted", func() {
		d := build(&platformv1alpha1.WorkloadSecuritySpec{
			Profile:                platformv1alpha1.SecurityProfileRestricted,
			RunAsUser:              ptr.To[int64](101),
			ReadOnlyRootFilesystem: ptr.To(true),
		})
		pod := d.Spec.Template.Spec
		c := pod.Containers[0]

		Expect(pod.SecurityContext.RunAsNonRoot).To(Equal(ptr.To(true)))
		Expect(pod.SecurityContext.RunAsUser).To(Equal(ptr.To[int64](101)))
		Expect(pod.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
		Expect(c.SecurityContext.Capabilities).To(Equal(&corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}))
		Expect(c.SecurityContext.AllowPrivilegeEscalation).To(Equal(ptr.To(false)))
		Expect(c.SecurityContext.ReadOnlyRootFilesystem).To(Equal(ptr.To(true)))
	})

	It("honors the baseline opt-outs", func() {
		d := build(&platformv1alpha1.WorkloadSecuritySpec{
			AllowPrivilegeEscalation:     ptr.To(true),
			AutomountServiceAccountToken: ptr.To(true),
		})
		pod := d.Spec.Template.Spec

		Expect(pod.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(Equal(ptr.To(true)))
		Expect(pod.AutomountServiceAccountToken).To(Equal(ptr.To(true)))
		Expect(pod.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
	})

	It("keeps privilege escalation off under restricted even if the webhook was bypassed", func() {
		d := build(&platformv1alpha1.WorkloadSecuritySpec{
			Profile:                  platformv1alpha1.SecurityProfileRestricted,
			AllowPrivilegeEscalation: ptr.To(true),
		})
		Expect(d.Spec.Template.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(Equal(ptr.To(false)))
	})
})
