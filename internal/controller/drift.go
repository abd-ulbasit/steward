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
// DRIFT TRACKING (Milestone 9)
// =============================================================================
//
// A driftTracker accumulates the outcome of each child-resource apply during a
// single reconcile pass. It exists so the main Reconcile loop can answer one
// question after all children are reconciled: "did anything need correcting?"
//
// WHY REQUEST-SCOPED (not a field on ApplicationReconciler):
//   controller-runtime runs reconciles for different objects CONCURRENTLY on a
//   shared reconciler instance. Accumulating drift on `r` would be a data race.
//   A fresh tracker per Reconcile call is inherently goroutine-safe.
//
// HOW DRIFT IS DISTINGUISHED FROM EXPECTED CHANGE:
//   CreateOrUpdate returns Created/Updated both on first provisioning AND when
//   correcting an external edit. The discriminator is the SPEC GENERATION:
//
//     drift  ==  (a child was Created/Updated)  AND  (spec did NOT change)
//
//   If spec.Generation == status.ObservedGeneration, the desired state is the
//   same one we already reconciled — so any child that still needed work was
//   changed by something outside the operator. That's drift. On a real spec
//   edit, Generation > ObservedGeneration and Created/Updated is expected.
// =============================================================================

package controller

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/abd-ulbasit/goplatform/internal/provider"
)

// driftRecord is the outcome of reconciling one child resource.
type driftRecord struct {
	kind    string // e.g. "Deployment", "Service"
	drifted bool   // true only when a MEANINGFUL change was corrected
	detail  string // optional field-level note, e.g. "replicas 5->3" or "recreated"
}

// driftTracker accumulates driftRecords for one reconcile pass.
type driftTracker struct {
	records []driftRecord
}

// record appends a child apply outcome. drifted must reflect a meaningful change
// (a managed field actually differed, or the resource was recreated) — NOT a raw
// CreateOrUpdate "Updated", which fires on harmless server-side defaulting too.
func (d *driftTracker) record(kind string, drifted bool, detail string) {
	d.records = append(d.records, driftRecord{kind: kind, drifted: drifted, detail: detail})
}

// corrected returns the records that represent real drift.
func (d *driftTracker) corrected() []driftRecord {
	var out []driftRecord
	for _, r := range d.records {
		if r.drifted {
			out = append(out, r)
		}
	}
	return out
}

// recoveredOrChanged is a small helper for the simpler child types (ConfigMap,
// Secret, HPA, PDB) where the realistic drift scenario is deletion: a Created
// result means the resource was missing and we recreated it. We deliberately do
// NOT treat Updated as drift for these, because their CreateOrUpdate churns on
// server defaults — flagging it would be a false positive.
func recoveredOrChanged(op controllerutil.OperationResult) (bool, string) {
	if op == controllerutil.OperationResultCreated {
		return true, "recreated"
	}
	return false, ""
}

// driftCorrectionMessage renders a stable, human-readable summary of corrected
// child resources, e.g. "Deployment (replicas 5->3), Service".
func driftCorrectionMessage(records []driftRecord) string {
	parts := make([]string, 0, len(records))
	for _, r := range records {
		if r.detail != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", r.kind, r.detail))
		} else {
			parts = append(parts, r.kind)
		}
	}
	sort.Strings(parts) // deterministic ordering for stable events/conditions
	return strings.Join(parts, ", ")
}

// servicePortNumbers extracts a sorted list of Service port numbers, for
// order-insensitive comparison of "did the exposed ports change?".
func servicePortNumbers(ports []corev1.ServicePort) []int32 {
	out := make([]int32, 0, len(ports))
	for _, p := range ports {
		out = append(out, p.Port)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// equalInt32Slices reports whether two int32 slices hold the same elements in
// the same order (callers sort first for set semantics).
func equalInt32Slices(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// infraDriftMessage renders a stable summary of infrastructure drift items,
// e.g. "database/spec.instances desired=1 actual=3".
func infraDriftMessage(items []provider.DriftItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s/%s desired=%s actual=%s",
			it.ResourceType, it.Field, it.DesiredValue, it.ActualValue))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
