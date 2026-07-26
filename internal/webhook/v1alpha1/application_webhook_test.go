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

// =============================================================================
// WEBHOOK UNIT TESTS
// =============================================================================
//
// These tests validate the webhook logic DIRECTLY on the structs — no webhook
// server, no envtest, no API server needed. This is the advantage of the
// kubebuilder v4 CustomValidator/CustomDefaulter pattern: the logic is just
// Go methods that can be tested in isolation.
//
// TEST ORGANIZATION:
//   - Defaulter tests: verify mutation logic (labels, version defaults, backup)
//   - Validator create tests: verify cross-field rules on creation
//   - Validator update tests: verify immutability checks + shared rules on update
//
// =============================================================================

package v1alpha1

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/abd-ulbasit/steward/api/v1alpha1"
)

// helper to create a minimal valid Application for testing.
// Tests override specific fields as needed.
func newTestApplication(name string) *platformv1alpha1.Application {
	return &platformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: platformv1alpha1.ApplicationSpec{
			Team:  "test-team",
			Owner: "test@example.com",
			Tier:  platformv1alpha1.TierStandard,
		},
	}
}

// int32Ptr returns a pointer to the given int32 value.
func int32Ptr(i int32) *int32 { return &i }

var _ = Describe("Application Webhook", func() {
	var (
		obj       *platformv1alpha1.Application
		oldObj    *platformv1alpha1.Application
		validator ApplicationCustomValidator
		defaulter ApplicationCustomDefaulter
		ctx       context.Context
	)

	BeforeEach(func() {
		obj = newTestApplication("test-app")
		oldObj = newTestApplication("test-app")
		validator = ApplicationCustomValidator{}
		defaulter = ApplicationCustomDefaulter{}
		ctx = context.TODO()
	})

	// =========================================================================
	// DEFAULTER TESTS
	// =========================================================================

	Context("When creating Application under Defaulting Webhook", func() {

		// -----------------------------------------------------------------
		// LABEL INJECTION
		// -----------------------------------------------------------------

		It("Should set managed-by label", func() {
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "steward"))
		})

		It("Should set team label from spec.team", func() {
			obj.Spec.Team = "payments"
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Labels).To(HaveKeyWithValue("platform.steward.sh/team", "payments"))
		})

		It("Should preserve existing labels while adding new ones", func() {
			obj.Labels = map[string]string{
				"custom-label": "custom-value",
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Labels).To(HaveKeyWithValue("custom-label", "custom-value"))
			Expect(obj.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "steward"))
			Expect(obj.Labels).To(HaveKeyWithValue("platform.steward.sh/team", "test-team"))
		})

		It("Should not overwrite existing managed-by label", func() {
			obj.Labels = map[string]string{
				"app.kubernetes.io/managed-by": "flux",
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Labels["app.kubernetes.io/managed-by"]).To(Equal("flux"))
		})

		// -----------------------------------------------------------------
		// DATABASE VERSION DEFAULTS
		// -----------------------------------------------------------------

		It("Should set postgres version to 16 when not specified", func() {
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type: platformv1alpha1.DatabasePostgres,
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Database.Version).To(Equal("16"))
		})

		It("Should set mysql version to 8 when not specified", func() {
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type: platformv1alpha1.DatabaseMySQL,
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Database.Version).To(Equal("8"))
		})

		It("Should not override explicitly set database version", func() {
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "15",
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Database.Version).To(Equal("15"))
		})

		// -----------------------------------------------------------------
		// BACKUP DEFAULTS FOR CRITICAL TIER
		// -----------------------------------------------------------------

		It("Should set backup defaults for critical tier with database", func() {
			obj.Spec.Tier = platformv1alpha1.TierCritical
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:             platformv1alpha1.DatabasePostgres,
				Version:          "16",
				HighAvailability: true,
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Database.Backup).NotTo(BeNil())
			Expect(*obj.Spec.Database.Backup.Enabled).To(BeTrue())
			Expect(*obj.Spec.Database.Backup.RetentionDays).To(Equal(int32(14)))
		})

		It("Should not set backup for standard tier", func() {
			obj.Spec.Tier = platformv1alpha1.TierStandard
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "16",
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Database.Backup).To(BeNil())
		})

		It("Should not override explicitly set backup config", func() {
			obj.Spec.Tier = platformv1alpha1.TierCritical
			enabled := true
			retention := int32(7)
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:             platformv1alpha1.DatabasePostgres,
				Version:          "16",
				HighAvailability: true,
				Backup: &platformv1alpha1.BackupSpec{
					Enabled:       &enabled,
					RetentionDays: &retention,
				},
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(*obj.Spec.Database.Backup.RetentionDays).To(Equal(int32(7)))
		})

		// -----------------------------------------------------------------
		// IDEMPOTENCY
		// -----------------------------------------------------------------

		It("Should be idempotent when called twice", func() {
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type: platformv1alpha1.DatabasePostgres,
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			// Save state after first call
			version1 := obj.Spec.Database.Version
			labels1 := make(map[string]string)
			for k, v := range obj.Labels {
				labels1[k] = v
			}

			// Call again
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Database.Version).To(Equal(version1))
			Expect(obj.Labels).To(Equal(labels1))
		})
	})

	// =========================================================================
	// VALIDATOR CREATE TESTS
	// =========================================================================

	Context("When validating Application on creation", func() {

		// -----------------------------------------------------------------
		// HAPPY PATH
		// -----------------------------------------------------------------

		It("Should accept a valid Application with no infrastructure", func() {
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should accept a valid Application with database", func() {
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "16",
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		// -----------------------------------------------------------------
		// CRITICAL TIER HA CHECKS
		// -----------------------------------------------------------------

		It("Should reject critical tier with non-HA database", func() {
			obj.Spec.Tier = platformv1alpha1.TierCritical
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:             platformv1alpha1.DatabasePostgres,
				Version:          "16",
				HighAvailability: false,
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("highAvailability"))
		})

		It("Should reject critical tier with non-HA cache", func() {
			obj.Spec.Tier = platformv1alpha1.TierCritical
			obj.Spec.Cache = &platformv1alpha1.CacheSpec{
				Type:             platformv1alpha1.CacheRedis,
				HighAvailability: false,
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("highAvailability"))
		})

		It("Should accept critical tier without database (nothing to validate)", func() {
			obj.Spec.Tier = platformv1alpha1.TierCritical
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should accept critical tier with HA database", func() {
			obj.Spec.Tier = platformv1alpha1.TierCritical
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:             platformv1alpha1.DatabasePostgres,
				Version:          "16",
				HighAvailability: true,
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		// -----------------------------------------------------------------
		// DATABASE VERSION RANGE
		// -----------------------------------------------------------------

		It("Should accept all valid PostgreSQL versions (13-17)", func() {
			for _, v := range []string{"13", "14", "15", "16", "17"} {
				obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
					Type:    platformv1alpha1.DatabasePostgres,
					Version: v,
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred(), "version %s should be valid", v)
			}
		})

		It("Should reject PostgreSQL version below 13", func() {
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "12",
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("between 13 and 17"))
		})

		It("Should reject PostgreSQL version above 17", func() {
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "18",
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("between 13 and 17"))
		})

		It("Should accept valid MySQL versions (5, 8)", func() {
			for _, v := range []string{"5", "8"} {
				obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
					Type:    platformv1alpha1.DatabaseMySQL,
					Version: v,
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred(), "version %s should be valid", v)
			}
		})

		It("Should reject invalid MySQL version", func() {
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabaseMySQL,
				Version: "6",
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("5 (for 5.7.x) or 8"))
		})

		// -----------------------------------------------------------------
		// SCALING VALIDATION
		// -----------------------------------------------------------------

		It("Should reject minReplicas greater than maxReplicas", func() {
			obj.Spec.Scaling = &platformv1alpha1.ScalingSpec{
				MinReplicas: int32Ptr(5),
				MaxReplicas: 3,
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("minReplicas"))
		})

		It("Should accept minReplicas equal to maxReplicas", func() {
			obj.Spec.Scaling = &platformv1alpha1.ScalingSpec{
				MinReplicas: int32Ptr(3),
				MaxReplicas: 3,
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should accept scaling without minReplicas set", func() {
			obj.Spec.Scaling = &platformv1alpha1.ScalingSpec{
				MaxReplicas: 5,
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		// -----------------------------------------------------------------
		// MULTIPLE ERRORS RETURNED
		// -----------------------------------------------------------------

		It("Should return multiple validation errors at once", func() {
			obj.Spec.Tier = platformv1alpha1.TierCritical
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:             platformv1alpha1.DatabasePostgres,
				Version:          "12",
				HighAvailability: false,
			}
			obj.Spec.Scaling = &platformv1alpha1.ScalingSpec{
				MinReplicas: int32Ptr(5),
				MaxReplicas: 3,
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			// Should contain errors for: HA, version range, and scaling
			Expect(err.Error()).To(ContainSubstring("highAvailability"))
			Expect(err.Error()).To(ContainSubstring("between 13 and 17"))
			Expect(err.Error()).To(ContainSubstring("minReplicas"))
		})
	})

	// =========================================================================
	// VALIDATOR UPDATE TESTS
	// =========================================================================

	Context("When validating Application on update", func() {

		// -----------------------------------------------------------------
		// IMMUTABLE FIELD CHECKS
		// -----------------------------------------------------------------

		It("Should reject database type change", func() {
			oldObj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "16",
			}
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabaseMySQL,
				Version: "8",
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
			Expect(err.Error()).To(ContainSubstring("database"))
		})

		It("Should reject queue type change", func() {
			oldObj.Spec.Queue = &platformv1alpha1.QueueSpec{
				Type: platformv1alpha1.QueueRabbitMQ,
			}
			obj.Spec.Queue = &platformv1alpha1.QueueSpec{
				Type: platformv1alpha1.QueueKafka,
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
			Expect(err.Error()).To(ContainSubstring("queue"))
		})

		It("Should reject cache type change", func() {
			oldObj.Spec.Cache = &platformv1alpha1.CacheSpec{
				Type: platformv1alpha1.CacheRedis,
			}
			obj.Spec.Cache = &platformv1alpha1.CacheSpec{
				Type: platformv1alpha1.CacheMemcached,
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
			Expect(err.Error()).To(ContainSubstring("cache"))
		})

		// -----------------------------------------------------------------
		// ALLOWED INFRASTRUCTURE CHANGES
		// -----------------------------------------------------------------

		It("Should allow adding database (old had none)", func() {
			oldObj.Spec.Database = nil
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "16",
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should allow removing database", func() {
			oldObj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "16",
			}
			obj.Spec.Database = nil
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should allow changing database size (mutable field)", func() {
			oldObj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "16",
				Size:    platformv1alpha1.SizeSmall,
			}
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "16",
				Size:    platformv1alpha1.SizeMedium,
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		// -----------------------------------------------------------------
		// UPDATE ALSO RUNS CREATE VALIDATIONS
		// -----------------------------------------------------------------

		It("Should also run create validations on update", func() {
			// Critical tier + no HA should fail even on update
			obj.Spec.Tier = platformv1alpha1.TierCritical
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:             platformv1alpha1.DatabasePostgres,
				Version:          "16",
				HighAvailability: false,
			}
			oldObj.Spec.Tier = platformv1alpha1.TierCritical
			oldObj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:             platformv1alpha1.DatabasePostgres,
				Version:          "16",
				HighAvailability: false,
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("highAvailability"))
		})

		// -----------------------------------------------------------------
		// COMBINED IMMUTABILITY + VALIDATION ERRORS
		// -----------------------------------------------------------------

		It("Should return both immutability and validation errors", func() {
			oldObj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabasePostgres,
				Version: "16",
			}
			// Change type (immutability violation) AND use invalid version (validation)
			obj.Spec.Database = &platformv1alpha1.DatabaseSpec{
				Type:    platformv1alpha1.DatabaseMySQL,
				Version: "6",
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
			Expect(err.Error()).To(ContainSubstring("MySQL version"))
		})
	})

	// =========================================================================
	// VALIDATOR DELETE TESTS
	// =========================================================================

	Context("When validating Application on deletion", func() {
		It("Should always allow deletion", func() {
			_, err := validator.ValidateDelete(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
