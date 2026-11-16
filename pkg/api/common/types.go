// Package common holds the REST API DTOs that more than one resource uses.
//
// It imports nothing, so clients (cappctl, capp-mcp) can share these types
// without pulling in the server handlers and their Kubernetes/Gin
// dependencies. The resource packages under pkg/api alias them.
package common

// ResourceQuantities holds CPU and memory as Kubernetes quantity strings.
type ResourceQuantities struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// ResourceSpec describes the compute resources requested/limited for a container.
type ResourceSpec struct {
	Requests *ResourceQuantities `json:"requests,omitempty"`
	Limits   *ResourceQuantities `json:"limits,omitempty"`
}

// EnvVarSource selects a value from a Secret or ConfigMap key.
type EnvVarSource struct {
	SecretKeyRef    *KeySelector `json:"secretKeyRef,omitempty"`
	ConfigMapKeyRef *KeySelector `json:"configMapKeyRef,omitempty"`
}

// KeySelector identifies a key within a named Secret or ConfigMap.
type KeySelector struct {
	Name string `json:"name" binding:"required"`
	Key  string `json:"key"  binding:"required"`
}

// EnvVar represents a single container environment variable.
type EnvVar struct {
	Name      string        `json:"name"      binding:"required"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}
