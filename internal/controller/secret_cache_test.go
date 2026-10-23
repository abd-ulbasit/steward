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
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
	"github.com/abd-ulbasit/steward/internal/provider"
)

// =============================================================================
// TESTS: the Application Secret under the label-scoped Secret cache (envtest)
// =============================================================================
//
// These run reconcileSecret through the informer cache cmd/main.go builds
// (provider.CacheOptions/ClientOptions), because only a real label-selected
// informer shows what happens when the Application's own Secret loses the
// managed-by label and drops out of the cache.
// =============================================================================

var _ = Describe("Application Secret outside the label-scoped cache", func() {
	const (
		ns       = "secret-cache-test"
		timeout  = 10 * time.Second
		interval = 100 * time.Millisecond
	)

	var (
		cacheCancel context.CancelFunc
		cached      client.Client
	)

	BeforeEach(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		if err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}

		var cacheCtx context.Context
		cacheCtx, cacheCancel = context.WithCancel(ctx)
		cacheOpts := provider.CacheOptions()
		cacheOpts.Scheme = k8sClient.Scheme()
		informers, err := cache.New(cfg, cacheOpts)
		Expect(err).NotTo(HaveOccurred())
		go func() { _ = informers.Start(cacheCtx) }()
		Expect(informers.WaitForCacheSync(cacheCtx)).To(BeTrue())

		clientOpts := provider.ClientOptions()
		clientOpts.Scheme = k8sClient.Scheme()
		clientOpts.Cache.Reader = informers
		cached, err = client.New(cfg, clientOpts)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		cacheCancel()
	})

	newApp := func(name string) *platformv1alpha1.Application {
		replicas := int32(1)
		app := &platformv1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: platformv1alpha1.ApplicationSpec{
				Team:  "platform",
				Owner: "test@example.com",
				Tier:  platformv1alpha1.TierStandard,
				Workload: &platformv1alpha1.WorkloadSpec{
					Image:    "nginx:latest",
					Replicas: &replicas,
				},
			},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		return app
	}

	newReconciler := func(apiReader client.Reader) *ApplicationReconciler {
		return &ApplicationReconciler{
			Client:    cached,
			Scheme:    k8sClient.Scheme(),
			Recorder:  record.NewFakeRecorder(100),
			APIReader: apiReader,
		}
	}

	key := func(app *platformv1alpha1.Application) client.ObjectKey {
		return client.ObjectKey{Name: app.Name, Namespace: app.Namespace}
	}

	// dropFromCache removes every label from the Secret and waits until the
	// cached client no longer sees it.
	dropFromCache := func(app *platformv1alpha1.Application) {
		var secret corev1.Secret
		Expect(k8sClient.Get(ctx, key(app), &secret)).To(Succeed())
		secret.Labels = nil
		Expect(k8sClient.Update(ctx, &secret)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(cached.Get(ctx, key(app), &corev1.Secret{}))
		}, timeout, interval).Should(BeTrue())
	}

	It("puts the labels back on its own Secret after they were removed", func() {
		app := newApp("relabel")
		r := newReconciler(k8sClient)

		_, err := r.reconcileSecret(ctx, app, &driftTracker{})
		Expect(err).NotTo(HaveOccurred())
		dropFromCache(app)

		// Without the uncached read the Secret looks absent and the create
		// collides with it on every pass. This is the wedge being fixed.
		_, err = newReconciler(nil).reconcileSecret(ctx, app, &driftTracker{})
		Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), "err = %v", err)

		dt := &driftTracker{}
		_, err = r.reconcileSecret(ctx, app, dt)
		Expect(err).NotTo(HaveOccurred())
		Expect(dt.corrected()).To(HaveLen(1))

		var secret corev1.Secret
		Expect(k8sClient.Get(ctx, key(app), &secret)).To(Succeed())
		Expect(secret.Labels).To(HaveKeyWithValue(provider.ManagedByLabel, provider.ManagedByValue))
		Expect(metav1.IsControlledBy(&secret, app)).To(BeTrue())

		// Labelled again, so later passes are served from the cache.
		Eventually(func() error {
			return cached.Get(ctx, key(app), &corev1.Secret{})
		}, timeout, interval).Should(Succeed())
	})

	It("refuses an unlabelled Secret it does not control", func() {
		app := newApp("foreign")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: ns},
			Data:       map[string][]byte{"token": []byte("not-stewards")},
		})).To(Succeed())

		_, err := newReconciler(k8sClient).reconcileSecret(ctx, app, &driftTracker{})
		Expect(err).To(MatchError(errSecretNotOwned))

		var secret corev1.Secret
		Expect(k8sClient.Get(ctx, key(app), &secret)).To(Succeed())
		Expect(secret.OwnerReferences).To(BeEmpty())
		Expect(secret.Labels).To(BeEmpty())
		Expect(string(secret.Data["token"])).To(Equal("not-stewards"))
	})
})
