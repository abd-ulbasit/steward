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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// =============================================================================
// WORKLOAD SECURITY DEFAULTS
// =============================================================================
//
// The operator's own pod runs under the restricted Pod Security Standard, but
// the Deployments it generated for tenants carried no security settings at
// all: root-capable containers, the default seccomp profile (Unconfined on most
// kubelets), and the namespace's default ServiceAccount token mounted into
// every pod.
//
// The defaults below are the subset that does not depend on how the image was
// built, so every image that ran before still runs (nginx:latest and
// postgres:16-alpine, which start as root, included):
//
//	seccompProfile: RuntimeDefault       the container runtime's syscall filter
//	allowPrivilegeEscalation: false      setuid binaries cannot gain privileges
//	automountServiceAccountToken: false  no API token in a pod that never uses it
//
// runAsNonRoot and dropping all capabilities are what the restricted standard
// adds, and they break images that start as root, so they are opt-in through
// spec.workload.security.profile: restricted. See WorkloadSecuritySpec for the
// opt-outs.
// =============================================================================

// workloadSecurity returns the pod security context, the container security
// context and the automountServiceAccountToken value for the Deployment.
func workloadSecurity(app *platformv1alpha1.Application) (*corev1.PodSecurityContext, *corev1.SecurityContext, *bool) {
	var spec platformv1alpha1.WorkloadSecuritySpec
	if app.Spec.Workload != nil && app.Spec.Workload.Security != nil {
		spec = *app.Spec.Workload.Security
	}
	restricted := spec.Profile == platformv1alpha1.SecurityProfileRestricted

	pod := &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		RunAsUser:      spec.RunAsUser,
		RunAsGroup:     spec.RunAsGroup,
		FSGroup:        spec.FSGroup,
	}
	container := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   spec.ReadOnlyRootFilesystem,
	}

	if restricted {
		pod.RunAsNonRoot = ptr.To(true)
		container.Capabilities = &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		}
	} else if spec.AllowPrivilegeEscalation != nil && *spec.AllowPrivilegeEscalation {
		// Opt-out. Ignored under restricted, where the webhook rejects it; the
		// controller enforces the same rule in case webhooks are disabled.
		container.AllowPrivilegeEscalation = ptr.To(true)
	}

	automount := ptr.To(false)
	if spec.AutomountServiceAccountToken != nil && *spec.AutomountServiceAccountToken {
		automount = ptr.To(true)
	}

	return pod, container, automount
}
