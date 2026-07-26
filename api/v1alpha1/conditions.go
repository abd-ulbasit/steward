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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// =============================================================================
// STATUS CONDITIONS - HELPER UTILITIES
// =============================================================================
//
// WHY THESE HELPERS EXIST:
//   Kubernetes conditions are a powerful but verbose pattern. If every
//   controller re-implements condition logic differently, we get:
//     - Inconsistent reasons/messages across reconcilers
//     - LastTransitionTime bugs (updated every reconcile)
//     - Missed observedGeneration updates
//     - Hard-to-test condition transitions
//
// These helpers provide a single, consistent way to:
//   1. Create conditions with the correct fields populated
//   2. Update existing conditions safely (using meta.SetStatusCondition)
//   3. Check readiness without duplicating boilerplate
//
// HOW IT WORKS UNDER THE HOOD:
//   - We build a metav1.Condition with Type/Status/Reason/Message
//   - We set ObservedGeneration to reflect the current spec
//   - We call meta.SetStatusCondition, which handles:
//       * Insertion or update by condition Type
//       * lastTransitionTime updates ONLY when Status changes
//       * Stable ordering for the condition list
//
// ALTERNATIVES CONSIDERED:
//   ┌─────────────────────────────────────────────────────────────────────────┐
//   │ Approach                    │ Pros                     │ Cons           │
//   ├─────────────────────────────┼──────────────────────────┼────────────────┤
//   │ Manual slice management     │ Full control             │ Bug-prone,     │
//   │ (index/find/replace)        │                          │ repetitive     │
//   ├─────────────────────────────┼──────────────────────────┼────────────────┤
//   │ Controller-only helpers     │ Localized                │ Not reusable   │
//   │                             │                          │across API types│
//   ├─────────────────────────────┼──────────────────────────┼────────────────┤
//   │ [x] Shared API helpers      │ Consistent, testable     │ Slightly more  │
//   │ (this file)                 │                          │ abstraction    │
//   └─────────────────────────────┴──────────────────────────┴────────────────┘
//
// HOW REAL PLATFORMS DO IT:
//   - Crossplane: condition helpers in package "xpv1"
//   - Cluster API: conditions helpers in "util/conditions"
//   - cert-manager: condition builders + set helpers
//
// FAILURE MODES THIS PREVENTS:
//   1. lastTransitionTime updates on every reconcile → watch storms
//   2. observedGeneration never updated → users can't tell if status is stale
//   3. Multiple conditions with same Type → ambiguous status
//
// =============================================================================

// NewCondition builds a metav1.Condition with consistent defaults.
// ObservedGeneration should always be the current spec generation.
func NewCondition(conditionType string, status metav1.ConditionStatus, reason, message string, observedGeneration int64) metav1.Condition {
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: metav1.Now(),
	}
}

// SetCondition updates the condition list and returns true if anything changed.
//
// Change detection is based on the fields that represent semantic state:
//   - Status
//   - Reason
//   - Message
//   - ObservedGeneration
//
// We do NOT compare LastTransitionTime because it is expected to change only
// when Status changes, and meta.SetStatusCondition manages that internally.
func SetCondition(conditions *[]metav1.Condition, condition metav1.Condition) bool {
	existing := meta.FindStatusCondition(*conditions, condition.Type)
	if existing != nil {
		if existing.Status == condition.Status &&
			existing.Reason == condition.Reason &&
			existing.Message == condition.Message &&
			existing.ObservedGeneration == condition.ObservedGeneration {
			return false
		}
	}

	meta.SetStatusCondition(conditions, condition)
	return true
}

// GetCondition returns the current condition by type (or nil if not present).
func GetCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(conditions, conditionType)
}

// IsConditionTrue returns true if the condition exists and is True.
func IsConditionTrue(conditions []metav1.Condition, conditionType string) bool {
	condition := GetCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// IsReady is a convenience check for overall readiness.
func IsReady(conditions []metav1.Condition) bool {
	return IsConditionTrue(conditions, ConditionTypeReady)
}
