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

// SecretVolume mounts a Kubernetes Secret as a volume into the container.
type SecretVolume struct {
	Name       string `json:"name"       binding:"required"`
	SecretName string `json:"secretName" binding:"required"`
	MountPath  string `json:"mountPath"  binding:"required"`
}

// ConfigMapVolume mounts a Kubernetes ConfigMap as a volume into the container.
type ConfigMapVolume struct {
	Name          string `json:"name"          binding:"required"`
	ConfigMapName string `json:"configMapName" binding:"required"`
	MountPath     string `json:"mountPath"     binding:"required"`
}

// Warning is a non-fatal problem attached to a successful response.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DeleteResponse is returned instead of an empty response when a delete
// succeeded with warnings.
type DeleteResponse struct {
	Warnings []Warning `json:"warnings,omitempty"`
}
