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
	"errors"
	"fmt"
)

// =============================================================================
// INFRASTRUCTURE PROVIDER ERRORS
// =============================================================================
//
// WHY TYPED ERRORS:
//   Different errors require different handling strategies:
//     - NotReady: Requeue and wait (provisioning in progress)
//     - QuotaExceeded: Alert, don't retry automatically
//     - NotFound: Normal for new resources, unexpected for existing
//     - InvalidConfig: User error, surface in status
//
//   Typed errors let the controller make intelligent decisions:
//
//   switch {
//   case provider.IsNotReady(err):
//       return ctrl.Result{RequeueAfter: 30*time.Second}, nil  // Wait and retry
//   case provider.IsQuotaExceeded(err):
//       recorder.Event(app, "Warning", "QuotaExceeded", err.Error())
//       return ctrl.Result{}, nil  // Don't retry - needs human intervention
//   case provider.IsInvalidConfig(err):
//       // Update status with error, don't requeue
//       return ctrl.Result{}, nil
//   default:
//       return ctrl.Result{}, err  // Unknown error, let controller-runtime handle
//   }
//
// HOW CROSSPLANE DOES IT:
//   - Uses wrapped errors with resource.ExternalObservation
//   - Has ResourceExists, ResourceUpToDate flags
//   - Errors bubble up to status conditions
//
// HOW AWS SDK DOES IT:
//   - Typed errors like ErrCodeDBInstanceNotFoundFault
//   - Allows precise error handling
//   - We follow this pattern
//
// =============================================================================

// ProviderError is the base interface for all provider errors.
// Implementations embed this to add specific error types.
type ProviderError interface {
	error

	// ErrorCode returns a machine-readable error code
	ErrorCode() string

	// IsRetryable returns true if the operation should be retried
	IsRetryable() bool

	// Resource returns the resource type that failed (database, cache, etc.)
	Resource() string
}

// =============================================================================
// ERROR TYPES
// =============================================================================

// NotReadyError indicates the resource is still being provisioned.
// The controller should requeue and check again later.
//
// Example: RDS instance is in "creating" state
type NotReadyError struct {
	ResourceType string
	ResourceID   string
	CurrentPhase ResourcePhase
	Message      string
}

func (e *NotReadyError) Error() string {
	return fmt.Sprintf("%s %s is not ready (phase: %s): %s",
		e.ResourceType, e.ResourceID, e.CurrentPhase, e.Message)
}

func (e *NotReadyError) ErrorCode() string {
	return "NotReady"
}

func (e *NotReadyError) IsRetryable() bool {
	return true
}

func (e *NotReadyError) Resource() string {
	return e.ResourceType
}

// NotFoundError indicates the resource doesn't exist.
// This is normal for new resources, but unexpected if we expected it to exist.
//
// Example: Terraform state exists but RDS instance was deleted externally
type NotFoundError struct {
	ResourceType string
	ResourceID   string
	Message      string
}

func (e *NotFoundError) Error() string {
	if e.ResourceID != "" {
		return fmt.Sprintf("%s %s not found: %s", e.ResourceType, e.ResourceID, e.Message)
	}
	return fmt.Sprintf("%s not found: %s", e.ResourceType, e.Message)
}

func (e *NotFoundError) ErrorCode() string {
	return "NotFound"
}

func (e *NotFoundError) IsRetryable() bool {
	return true // Might appear on retry (eventual consistency)
}

func (e *NotFoundError) Resource() string {
	return e.ResourceType
}

// QuotaExceededError indicates a cloud provider quota/limit was hit.
// This requires human intervention (request quota increase).
//
// Example: "Maximum number of DB instances (20) exceeded"
type QuotaExceededError struct {
	ResourceType string
	QuotaName    string
	CurrentUsage int
	Limit        int
	Message      string
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("%s quota exceeded for %s (current: %d, limit: %d): %s",
		e.ResourceType, e.QuotaName, e.CurrentUsage, e.Limit, e.Message)
}

func (e *QuotaExceededError) ErrorCode() string {
	return "QuotaExceeded"
}

func (e *QuotaExceededError) IsRetryable() bool {
	return false // Needs human intervention
}

func (e *QuotaExceededError) Resource() string {
	return e.ResourceType
}

// InvalidConfigError indicates the Application spec has invalid configuration.
// The user needs to fix the spec.
//
// Example: Unsupported database version, invalid size, etc.
type InvalidConfigError struct {
	ResourceType string
	Field        string
	Value        string
	Message      string
}

func (e *InvalidConfigError) Error() string {
	return fmt.Sprintf("invalid configuration for %s: %s=%s: %s",
		e.ResourceType, e.Field, e.Value, e.Message)
}

func (e *InvalidConfigError) ErrorCode() string {
	return "InvalidConfig"
}

func (e *InvalidConfigError) IsRetryable() bool {
	return false // User must fix the spec
}

func (e *InvalidConfigError) Resource() string {
	return e.ResourceType
}

// ProvisioningError indicates a failure during resource provisioning.
// This might be transient (retry) or permanent (investigate).
//
// Example: Terraform apply failed, AWS API error
type ProvisioningError struct {
	ResourceType string
	ResourceID   string
	Operation    string // "create", "update", "delete"
	Cause        error
	Message      string
}

func (e *ProvisioningError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("failed to %s %s %s: %s: %v",
			e.Operation, e.ResourceType, e.ResourceID, e.Message, e.Cause)
	}
	return fmt.Sprintf("failed to %s %s %s: %s",
		e.Operation, e.ResourceType, e.ResourceID, e.Message)
}

func (e *ProvisioningError) ErrorCode() string {
	return "ProvisioningFailed"
}

func (e *ProvisioningError) IsRetryable() bool {
	return true // Transient failures should be retried
}

func (e *ProvisioningError) Resource() string {
	return e.ResourceType
}

func (e *ProvisioningError) Unwrap() error {
	return e.Cause
}

// TimeoutError indicates an operation took too long.
// Common for Terraform operations that can take 10+ minutes.
//
// Example: RDS instance creation exceeded 30 minute timeout
type TimeoutError struct {
	ResourceType string
	ResourceID   string
	Operation    string
	Duration     string
	Message      string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s %s %s timed out after %s: %s",
		e.Operation, e.ResourceType, e.ResourceID, e.Duration, e.Message)
}

func (e *TimeoutError) ErrorCode() string {
	return "Timeout"
}

func (e *TimeoutError) IsRetryable() bool {
	return true // Might succeed on retry (operation might complete in background)
}

func (e *TimeoutError) Resource() string {
	return e.ResourceType
}

// AuthenticationError indicates a failure to authenticate with the cloud provider.
// This is typically a configuration issue.
//
// Example: Invalid AWS credentials, expired token
type AuthenticationError struct {
	Provider string
	Message  string
	Cause    error
}

func (e *AuthenticationError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("authentication failed for provider %s: %s: %v",
			e.Provider, e.Message, e.Cause)
	}
	return fmt.Sprintf("authentication failed for provider %s: %s",
		e.Provider, e.Message)
}

func (e *AuthenticationError) ErrorCode() string {
	return "AuthenticationFailed"
}

func (e *AuthenticationError) IsRetryable() bool {
	return false // Config issue, needs human intervention
}

func (e *AuthenticationError) Resource() string {
	return "provider"
}

func (e *AuthenticationError) Unwrap() error {
	return e.Cause
}

// ProviderNotConfiguredError indicates the requested provider is not configured.
type ProviderNotConfiguredError struct {
	Provider ProviderType
	Message  string
}

func (e *ProviderNotConfiguredError) Error() string {
	return fmt.Sprintf("provider %s is not configured: %s", e.Provider, e.Message)
}

func (e *ProviderNotConfiguredError) ErrorCode() string {
	return "ProviderNotConfigured"
}

func (e *ProviderNotConfiguredError) IsRetryable() bool {
	return false
}

func (e *ProviderNotConfiguredError) Resource() string {
	return "provider"
}

// =============================================================================
// ERROR CHECKING HELPERS
// =============================================================================
//
// These functions check if an error is of a specific type.
// They use errors.As which handles wrapped errors correctly.
//
// USAGE:
//
//   if provider.IsNotReady(err) {
//       // Requeue and wait
//   }
//
// =============================================================================

// IsNotReady returns true if the error indicates a resource is not ready.
func IsNotReady(err error) bool {
	var e *NotReadyError
	return errors.As(err, &e)
}

// IsNotFound returns true if the error indicates a resource was not found.
func IsNotFound(err error) bool {
	var e *NotFoundError
	return errors.As(err, &e)
}

// IsQuotaExceeded returns true if the error indicates a quota was exceeded.
func IsQuotaExceeded(err error) bool {
	var e *QuotaExceededError
	return errors.As(err, &e)
}

// IsInvalidConfig returns true if the error indicates invalid configuration.
func IsInvalidConfig(err error) bool {
	var e *InvalidConfigError
	return errors.As(err, &e)
}

// IsProvisioningError returns true if the error is a general provisioning failure.
func IsProvisioningError(err error) bool {
	var e *ProvisioningError
	return errors.As(err, &e)
}

// IsTimeout returns true if the error indicates an operation timed out.
func IsTimeout(err error) bool {
	var e *TimeoutError
	return errors.As(err, &e)
}

// IsAuthenticationError returns true if the error is an authentication failure.
func IsAuthenticationError(err error) bool {
	var e *AuthenticationError
	return errors.As(err, &e)
}

// IsProviderNotConfigured returns true if the provider is not configured.
func IsProviderNotConfigured(err error) bool {
	var e *ProviderNotConfiguredError
	return errors.As(err, &e)
}

// IsRetryable returns true if the error is potentially transient and retry might help.
func IsRetryable(err error) bool {
	var pe ProviderError
	if errors.As(err, &pe) {
		return pe.IsRetryable()
	}
	// Unknown errors are treated as retryable (conservative)
	return true
}

// GetErrorCode extracts the error code from a ProviderError.
// Returns empty string if not a ProviderError.
func GetErrorCode(err error) string {
	var pe ProviderError
	if errors.As(err, &pe) {
		return pe.ErrorCode()
	}
	return ""
}

// GetResourceType extracts the resource type from a ProviderError.
// Returns empty string if not a ProviderError.
func GetResourceType(err error) string {
	var pe ProviderError
	if errors.As(err, &pe) {
		return pe.Resource()
	}
	return ""
}
