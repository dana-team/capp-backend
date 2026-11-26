// Package configmaps holds the ConfigMap request/response DTOs of the REST API.
//
// It imports only other DTO packages, so clients (cappctl, capp-mcp) can share these types
// without pulling in the server handler and its Kubernetes/Gin dependencies.
// The handler package internal/resources/namespaced/configmaps aliases them.
package configmaps

import (
	"github.com/dana-team/capp-backend/pkg/api/common"
	"github.com/dana-team/capp-backend/pkg/api/drift"
)

type ConfigMapRequest struct {
	// Name is the Kubernetes config map name.
	Name string `json:"name" binding:"required"`

	// Data is the key-value data stored in the config map.
	Data map[string]string `json:"data"`
}

type ConfigMapUpdateRequest struct {
	// Data is the key-value data stored in the config map.
	Data map[string]string `json:"data"`
}

type ConfigMapResponse struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	CreatedAt       string            `json:"createdAt,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	Data            map[string]string `json:"data"`

	// Warnings reports side effects that failed although the request succeeded.
	Warnings []common.Warning `json:"warnings,omitempty"`

	// Drift compares the ConfigMap with its recorded desired state. It is omitted
	// when desired state is disabled.
	Drift *drift.Drift `json:"drift,omitempty"`
}

type ConfigMapListResponse struct {
	Items []ConfigMapResponse `json:"items"`
	Total int                 `json:"total"`

	// Missing lists ConfigMaps that have a recorded desired state but no longer
	// exist. They are not counted in Total.
	Missing []drift.Missing `json:"missing,omitempty"`
}

// ConfigMapNameListResponse is the response body for the names-only list endpoint.
type ConfigMapNameListResponse struct {
	Items []string `json:"items"`
	Total int      `json:"total"`
}
