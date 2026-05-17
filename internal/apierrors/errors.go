// Package apierrors defines the canonical error types used throughout the
// capp-backend HTTP layer.
//
// Every handler calls Respond(c, err) instead of writing JSON directly.
// Respond translates Go errors — including Kubernetes API errors — into a
// consistent JSON envelope that the frontend can reliably parse.
//
// Error envelope shape:
//
//	{
//	  "error": {
//	    "code":    "NOT_FOUND",
//	    "message": "capp \"my-app\" not found in namespace \"default\"",
//	    "status":  404,
//	    "details": {}
//	  }
//	}
package apierrors

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

// Error code constants. These are the values that appear in the "code" field
// of the JSON error envelope and are stable across releases. Frontend code
// should switch on these strings rather than HTTP status codes.
const (
	CodeNotFound         = "NOT_FOUND"
	CodeConflict         = "CONFLICT"
	CodeForbidden        = "FORBIDDEN"
	CodeUnauthorized     = "UNAUTHORIZED"
	CodeBadRequest       = "BAD_REQUEST"
	CodeInternal         = "INTERNAL_ERROR"
	CodeClusterNotFound  = "CLUSTER_NOT_FOUND"
	CodeClusterUnhealthy = "CLUSTER_UNHEALTHY"
	CodeNotSupported     = "NOT_SUPPORTED"
	CodeNamespaceDenied  = "NAMESPACE_DENIED"
)

// APIError is the canonical error type for all HTTP responses. It implements
// the error interface so it can be passed through normal Go error handling
// paths and detected with errors.As.
type APIError struct {
	// Code is a stable, machine-readable error identifier (see the Code* constants).
	Code string `json:"code"`

	// Message is a human-readable description safe to display in the UI.
	Message string `json:"message"`

	// Status is the HTTP status code associated with this error.
	Status int `json:"status"`

	// Details holds optional structured context. May be nil.
	Details map[string]any `json:"details,omitempty"`

	// cause holds the original error for server-side logging. It is never
	// serialized to JSON or exposed to clients.
	cause error `json:"-"`
}

// Unwrap returns the underlying cause so errors.Is/As can traverse the chain.
func (e *APIError) Unwrap() error {
	return e.cause
}

// Error implements the error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// ── Constructors ──────────────────────────────────────────────────────────────

// NewNotFound returns a 404 error for a named resource.
func NewNotFound(resource, name string) *APIError {
	return &APIError{
		Code:    CodeNotFound,
		Message: fmt.Sprintf("%s %q not found", resource, name),
		Status:  http.StatusNotFound,
	}
}

// NewConflict returns a 409 error when a resource already exists.
func NewConflict(resource, name string) *APIError {
	return &APIError{
		Code:    CodeConflict,
		Message: fmt.Sprintf("%s %q already exists", resource, name),
		Status:  http.StatusConflict,
	}
}

// NewForbidden returns a 403 error.
func NewForbidden(msg string) *APIError {
	return &APIError{Code: CodeForbidden, Message: msg, Status: http.StatusForbidden}
}

// NewUnauthorized returns a 401 error.
func NewUnauthorized(msg string) *APIError {
	return &APIError{Code: CodeUnauthorized, Message: msg, Status: http.StatusUnauthorized}
}

// NewBadRequest returns a 400 error with the provided message.
func NewBadRequest(msg string) *APIError {
	return &APIError{Code: CodeBadRequest, Message: msg, Status: http.StatusBadRequest}
}

// NewInternal returns a generic 500 response. The original error is NOT
// included in the client-facing message to prevent leaking internal
// details (cluster URLs, service-account paths, etc.). Callers that need
// to preserve the original error for server-side logging should use
// Respond, which attaches the original error to the Gin context.
func NewInternal(err error) *APIError {
	return &APIError{
		Code:    CodeInternal,
		Message: "internal server error",
		Status:  http.StatusInternalServerError,
		cause:   err,
	}
}

// NewClusterNotFound returns a 404 error for an unknown cluster name.
func NewClusterNotFound(name string) *APIError {
	return &APIError{
		Code:    CodeClusterNotFound,
		Message: fmt.Sprintf("cluster %q is not configured", name),
		Status:  http.StatusNotFound,
	}
}

// NewClusterUnhealthy returns a 503 error when a cluster is reachable in config
// but failed its last health check.
func NewClusterUnhealthy(name string) *APIError {
	return &APIError{
		Code:    CodeClusterUnhealthy,
		Message: fmt.Sprintf("cluster %q is currently unavailable", name),
		Status:  http.StatusServiceUnavailable,
	}
}

// NewNotSupported returns a 501 error for operations not implemented in the
// current configuration (e.g. Login in passthrough mode).
func NewNotSupported(operation string) *APIError {
	return &APIError{
		Code:    CodeNotSupported,
		Message: fmt.Sprintf("operation %q is not supported in the current auth mode", operation),
		Status:  http.StatusNotImplemented,
	}
}

// NewNamespaceDenied returns a 403 error when a namespace is outside the
// allowedNamespaces list for a cluster.
func NewNamespaceDenied(namespace, cluster string) *APIError {
	return &APIError{
		Code:    CodeNamespaceDenied,
		Message: fmt.Sprintf("namespace %q is not accessible on cluster %q", namespace, cluster),
		Status:  http.StatusForbidden,
	}
}

// ── Response helpers ──────────────────────────────────────────────────────────

// Respond translates err into the canonical JSON error envelope and writes it
// to the Gin context. Translation priority:
//  1. *APIError — written as-is.
//  2. Kubernetes API errors — mapped to the nearest APIError.
//  3. Anything else — wrapped as a 500 Internal Error.
//
// The original error is attached to the Gin context (via c.Error) so the
// logging middleware can record it server-side without leaking it to the
// client. Respond calls c.Abort() so no further handlers are executed.
func Respond(c *gin.Context, err error) {
	// Attach the original error for server-side logging.
	_ = c.Error(err)

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		c.AbortWithStatusJSON(apiErr.Status, gin.H{"error": apiErr})
		return
	}

	// Translate well-known Kubernetes API status errors.
	if translated := translateK8sError(err); translated != nil {
		c.AbortWithStatusJSON(translated.Status, gin.H{"error": translated})
		return
	}

	// Fallback: 500.
	internal := NewInternal(err)
	c.AbortWithStatusJSON(internal.Status, gin.H{"error": internal})
}

// translateK8sError maps a Kubernetes API error to an APIError with a safe,
// generic client-facing message. The original error is preserved as the cause
// for server-side logging. Returns nil if err is not a Kubernetes API error.
func translateK8sError(err error) *APIError {
	switch {
	case k8serrors.IsNotFound(err):
		return &APIError{Code: CodeNotFound, Message: k8sSafeMessage(err, "resource not found"), Status: http.StatusNotFound, cause: err}
	case k8serrors.IsAlreadyExists(err):
		return &APIError{Code: CodeConflict, Message: k8sSafeMessage(err, "resource already exists"), Status: http.StatusConflict, cause: err}
	case k8serrors.IsForbidden(err):
		return &APIError{Code: CodeForbidden, Message: "access denied", Status: http.StatusForbidden, cause: err}
	case k8serrors.IsUnauthorized(err):
		return &APIError{Code: CodeUnauthorized, Message: "unauthorized", Status: http.StatusUnauthorized, cause: err}
	case k8serrors.IsInvalid(err):
		return &APIError{Code: CodeBadRequest, Message: "request validation failed", Status: http.StatusBadRequest, cause: err}
	case k8serrors.IsServiceUnavailable(err):
		return &APIError{Code: CodeInternal, Message: "service temporarily unavailable", Status: http.StatusServiceUnavailable, cause: err}
	case k8serrors.IsTimeout(err), k8serrors.IsServerTimeout(err):
		return &APIError{Code: CodeInternal, Message: "request timed out", Status: http.StatusGatewayTimeout, cause: err}
	default:
		if k8serrors.IsInternalError(err) {
			return &APIError{Code: CodeInternal, Message: "internal server error", Status: http.StatusInternalServerError, cause: err}
		}
		return nil
	}
}

// k8sSafeMessage extracts the resource name from a K8s StatusError (if
// available) and builds a safe client-facing message. Falls back to
// fallback when structured details are absent.
func k8sSafeMessage(err error, fallback string) string {
	var statusErr *k8serrors.StatusError
	if !errors.As(err, &statusErr) {
		return fallback
	}
	details := statusErr.Status().Details
	if details == nil || details.Name == "" {
		return fallback
	}
	return fmt.Sprintf("%s: %q", fallback, details.Name)
}

// RespondOK writes a 200 response with data as the JSON body.
func RespondOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, data)
}

// RespondCreated writes a 201 response with data as the JSON body.
func RespondCreated(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, data)
}

// RespondNoContent writes a 204 response with no body.
func RespondNoContent(c *gin.Context) {
	c.AbortWithStatus(http.StatusNoContent)
}
