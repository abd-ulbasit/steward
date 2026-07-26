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
// CONDITION HELPER TESTS
// =============================================================================
//
// These helpers sit under the reconciler's status-write path, and two of their
// properties are load-bearing for the whole controller:
//
//   1. SetCondition must report "no change" when nothing semantic changed.
//      Reconcile skips the Status().Update() call when the status is unchanged.
//      If SetCondition reported a change on every pass, the 5-minute resync
//      would write status forever, and every write is a watch event for anyone
//      watching Applications.
//
//   2. LastTransitionTime must move only when Status flips. A timestamp that
//      ticks on every reconcile makes the object different every pass, which
//      defeats (1) even if Reason and Message are stable.
//
// Both are properties of the *contract*, not of any single caller, so they are
// tested here rather than through the controller.
// =============================================================================

package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewConditionPopulatesAllFields(t *testing.T) {
	before := metav1.Now()
	c := NewCondition(ConditionTypeReady, metav1.ConditionTrue, "AllReady", "everything is up", 7)

	if c.Type != ConditionTypeReady {
		t.Errorf("Type = %q, want %q", c.Type, ConditionTypeReady)
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Status = %q, want %q", c.Status, metav1.ConditionTrue)
	}
	if c.Reason != "AllReady" {
		t.Errorf("Reason = %q, want %q", c.Reason, "AllReady")
	}
	if c.Message != "everything is up" {
		t.Errorf("Message = %q, want %q", c.Message, "everything is up")
	}
	// ObservedGeneration is what lets a user tell "status reflects my latest
	// edit" from "status is stale". A zero value here would silently break that.
	if c.ObservedGeneration != 7 {
		t.Errorf("ObservedGeneration = %d, want 7", c.ObservedGeneration)
	}
	if c.LastTransitionTime.Before(&before) {
		t.Errorf("LastTransitionTime = %v, want >= %v", c.LastTransitionTime, before)
	}
}

func TestSetConditionChangeDetection(t *testing.T) {
	base := NewCondition(ConditionTypeDatabaseReady, metav1.ConditionFalse, "Provisioning", "creating cluster", 1)

	tests := []struct {
		name       string
		next       metav1.Condition
		wantChange bool
	}{
		{
			name:       "identical condition is not a change",
			next:       NewCondition(ConditionTypeDatabaseReady, metav1.ConditionFalse, "Provisioning", "creating cluster", 1),
			wantChange: false,
		},
		{
			name:       "status flip is a change",
			next:       NewCondition(ConditionTypeDatabaseReady, metav1.ConditionTrue, "Provisioning", "creating cluster", 1),
			wantChange: true,
		},
		{
			name:       "reason change is a change",
			next:       NewCondition(ConditionTypeDatabaseReady, metav1.ConditionFalse, "WaitingForOperator", "creating cluster", 1),
			wantChange: true,
		},
		{
			name:       "message change is a change",
			next:       NewCondition(ConditionTypeDatabaseReady, metav1.ConditionFalse, "Provisioning", "waiting for 3 instances", 1),
			wantChange: true,
		},
		{
			// A spec edit bumps Generation. Even with an otherwise identical
			// condition, status must be rewritten so observedGeneration catches up.
			name:       "observedGeneration bump is a change",
			next:       NewCondition(ConditionTypeDatabaseReady, metav1.ConditionFalse, "Provisioning", "creating cluster", 2),
			wantChange: true,
		},
		{
			// LastTransitionTime is managed by meta.SetStatusCondition, so a
			// newer timestamp on an otherwise identical condition must NOT count.
			// This is the property that keeps the 5-minute resync silent.
			name: "newer timestamp alone is not a change",
			next: func() metav1.Condition {
				c := NewCondition(ConditionTypeDatabaseReady, metav1.ConditionFalse, "Provisioning", "creating cluster", 1)
				c.LastTransitionTime = metav1.NewTime(time.Now().Add(time.Hour))
				return c
			}(),
			wantChange: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conditions := []metav1.Condition{}
			if changed := SetCondition(&conditions, base); !changed {
				t.Fatal("SetCondition on an empty list returned false, want true")
			}

			if got := SetCondition(&conditions, tt.next); got != tt.wantChange {
				t.Errorf("SetCondition changed = %v, want %v", got, tt.wantChange)
			}
			if len(conditions) != 1 {
				t.Errorf("len(conditions) = %d, want 1 (SetCondition must update in place, not append)", len(conditions))
			}
		})
	}
}

func TestSetConditionLastTransitionTimeOnlyMovesOnStatusFlip(t *testing.T) {
	var conditions []metav1.Condition
	SetCondition(&conditions, NewCondition(ConditionTypeReady, metav1.ConditionFalse, "Provisioning", "starting", 1))
	first := GetCondition(conditions, ConditionTypeReady).LastTransitionTime

	// Same status, new message: the transition time must be preserved, otherwise
	// every reconcile produces a byte-different status object.
	time.Sleep(2 * time.Millisecond)
	SetCondition(&conditions, NewCondition(ConditionTypeReady, metav1.ConditionFalse, "Provisioning", "still starting", 1))
	if got := GetCondition(conditions, ConditionTypeReady).LastTransitionTime; !got.Equal(&first) {
		t.Errorf("LastTransitionTime moved on a message-only update: %v -> %v", first, got)
	}

	// Status flip: now it must move.
	time.Sleep(2 * time.Millisecond)
	SetCondition(&conditions, NewCondition(ConditionTypeReady, metav1.ConditionTrue, "AllReady", "up", 1))
	if got := GetCondition(conditions, ConditionTypeReady).LastTransitionTime; got.Equal(&first) {
		t.Errorf("LastTransitionTime did not move on a status flip (still %v)", first)
	}
}

func TestConditionLookupHelpers(t *testing.T) {
	var conditions []metav1.Condition

	if GetCondition(conditions, ConditionTypeReady) != nil {
		t.Error("GetCondition on an empty list returned non-nil")
	}
	if IsConditionTrue(conditions, ConditionTypeReady) {
		t.Error("IsConditionTrue on an empty list returned true")
	}
	if IsReady(conditions) {
		t.Error("IsReady on an empty list returned true; a missing Ready condition must not read as ready")
	}

	SetCondition(&conditions, NewCondition(ConditionTypeReady, metav1.ConditionFalse, "Provisioning", "starting", 1))
	if IsReady(conditions) {
		t.Error("IsReady returned true for Ready=False")
	}

	// ConditionUnknown is the state during provisioning of an optional
	// component; it must not be treated as ready.
	SetCondition(&conditions, NewCondition(ConditionTypeCacheReady, metav1.ConditionUnknown, "Pending", "not yet observed", 1))
	if IsConditionTrue(conditions, ConditionTypeCacheReady) {
		t.Error("IsConditionTrue returned true for ConditionUnknown")
	}

	SetCondition(&conditions, NewCondition(ConditionTypeReady, metav1.ConditionTrue, "AllReady", "up", 1))
	if !IsReady(conditions) {
		t.Error("IsReady returned false for Ready=True")
	}
	if got := len(conditions); got != 2 {
		t.Errorf("len(conditions) = %d, want 2", got)
	}
}
