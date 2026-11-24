// Package secrets holds the Secret request/response DTOs of the REST API.
//
// It imports only other DTO packages, so clients (cappctl, capp-mcp) can share these types
// without pulling in the server handler and its Kubernetes/Gin dependencies.
// The handler package internal/resources/namespaced/secrets aliases them.
package secrets

import (
	"github.com/dana-team/capp-backend/pkg/api/common"
	"github.com/dana-team/capp-backend/pkg/api/drift"
)

type SecretRequest struct {
	// Name is the Kubernetes secret name.
	Name string `json:"name" binding:"required"`
	// Type is the Kubernetes secret type (e.g. "Opaque", "kubernetes.io/tls").
	// Defaults to "Opaque" if not provided.
	Type string `json:"type,omitempty"`
	// Data is the key-value data stored in the secret (plain-text values; the API server handles base64 encoding).
	Data map[string]string `json:"data,omitempty"`
}

type SecretUpdateRequest struct {
	// Data is the key-value data stored in the secret.
	Data map[string]string `json:"data,omitempty"`
}

type SecretResponse struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Type            string            `json:"type"`
	CreatedAt       string            `json:"createdAt,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	Data            map[string]string `json:"data,omitempty"`

	// Warnings reports side effects that failed although the request succeeded.
	Warnings []common.Warning `json:"warnings,omitempty"`

	// Drift compares the Secret with its recorded desired state. It is omitted
	// when desired state is disabled.
	Drift *drift.Drift `json:"drift,omitempty"`
}

type SecretListResponse struct {
	Items []SecretResponse `json:"items"`
	Total int              `json:"total"`

	// Missing lists Secrets that have a recorded desired state but no longer
	// exist. They are not counted in Total.
	Missing []drift.Missing `json:"missing,omitempty"`
}

// SecretNameListResponse returns a list of secret names without exposing secret data.
type SecretNameListResponse struct {
	Items []string `json:"items"`
	Total int      `json:"total"`
}
