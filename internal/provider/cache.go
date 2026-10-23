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

package provider

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// =============================================================================
// MANAGER CACHE SCOPE
// =============================================================================
//
// controller-runtime starts one cluster-wide informer per type the manager
// reads or watches. Owns(&corev1.Secret{}) alone therefore copies every Secret
// in the cluster into this process: other teams' TLS keys, ServiceAccount
// tokens, Helm release payloads. A memory dump or a compromised pod would
// expose all of them, and on a large cluster the 128Mi limit is at risk.
//
// The operator only ever reads Secrets it created, and those carry
// app.kubernetes.io/managed-by=steward, so the Secret informer is restricted
// to that label. A Secret without the label is invisible to cached reads;
// getSecret falls back to an uncached read for the one case that matters
// (credential Secrets written by releases that did not label them).
//
// Pods are read in exactly one place (redisPodsReady, a label-selected List
// in one namespace). Serving that from the cache would need a cluster-wide
// Pod informer and the watch verb, so Pod reads bypass the cache and the
// RBAC stays get/list.
//
// This narrows what sits in memory. It does not narrow the RBAC: the
// ClusterRole still allows reading every Secret. See
// docs/DESIGN-DECISIONS.md section 7 for that trade-off.
// =============================================================================

// ManagedByLabel and ManagedByValue mark every object steward creates.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "steward"
)

// ManagedSecretSelector selects the Secrets steward creates.
func ManagedSecretSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{ManagedByLabel: ManagedByValue})
}

// CacheOptions returns the manager cache configuration.
func CacheOptions() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Label: ManagedSecretSelector()},
		},
	}
}

// ClientOptions returns the manager client configuration.
func ClientOptions() client.Options {
	return client.Options{
		Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Pod{}},
		},
	}
}
