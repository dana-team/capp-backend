// Package capps implements the Capp resource handler.
//
// It exposes full CRUD for the rcs.dana.io/v1alpha1/Capp custom resource:
//
//	GET    /api/v1/clusters/:cluster/capps                               (all namespaces)
//	GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps
//	GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
//	POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps
//	PUT    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
//	DELETE /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
//
// The handler translates between the frontend-facing DTOs defined in this file
// and the Kubernetes Capp type via convert.go. All K8s API interaction happens
// through the per-request scoped client injected by the cluster middleware.
package capps

// ── Request / Response DTOs ───────────────────────────────────────────────────
// These types are the public API contract between the backend and the frontend.
// They are intentionally simpler than the raw Kubernetes Capp spec so the
// frontend does not need to understand K8s conventions.

// EnvVar represents a single container environment variable.
type EnvVar struct {
	Name  string `json:"name"  binding:"required"`
	Value string `json:"value"`
}

// VolumeMount maps a volume into a container at a path.
type VolumeMount struct {
	Name      string `json:"name"      binding:"required"`
	MountPath string `json:"mountPath" binding:"required"`
}

// RouteSpec configures the external HTTP route for a Capp.
type RouteSpec struct {
	// Hostname is a custom DNS name for the Capp. Optional.
	Hostname string `json:"hostname,omitempty"`

	// TLSEnabled determines whether TLS is enabled for the route.
	TLSEnabled bool `json:"tlsEnabled,omitempty"`

	// RouteTimeoutSeconds is the maximum request duration. Optional.
	RouteTimeoutSeconds *int64 `json:"routeTimeoutSeconds,omitempty"`
}

// LogSpec configures Elasticsearch log shipping for a Capp.
type LogSpec struct {
	// Type is currently always "elastic".
	Type           string `json:"type"`
	Host           string `json:"host"`
	Index          string `json:"index"`
	User           string `json:"user"`
	PasswordSecret string `json:"passwordSecret"`
}

// NFSVolume describes one NFS volume to be mounted into the Capp containers.
type NFSVolume struct {
	Name     string `json:"name"     binding:"required"`
	Server   string `json:"server"   binding:"required"`
	Path     string `json:"path"     binding:"required"`
	Capacity string `json:"capacity" binding:"required"` // e.g. "10Gi"
}

// KedaSource describes a KEDA external scaler trigger source.
type KedaSource struct {
	Name           string            `json:"name"           binding:"required"`
	ScalarType     string            `json:"scalarType"     binding:"required"`
	ScalarMetadata map[string]string `json:"scalarMetadata,omitempty"`
	MinReplicas    *int32            `json:"minReplicas,omitempty"`
	MaxReplicas    *int32            `json:"maxReplicas,omitempty"`
}

// CappRequest is the request body accepted by POST (create) and PUT (update).
// Required fields are validated by Gin's binding:"required" tags.
type CappRequest struct {
	// Name is the Kubernetes resource name. Required for create.
	// For update it is taken from the URL parameter.
	Name      string `json:"name"      binding:"required"`
	Namespace string `json:"namespace" binding:"required"`

	// ScaleMetric defines the autoscaling metric.
	// One of: concurrency, cpu, memory, rps, external. Default: concurrency.
	ScaleMetric string `json:"scaleMetric,omitempty"`

	// State is enabled or disabled. Default: enabled.
	State string `json:"state,omitempty"`

	// MinReplicas sets the minimum replica count. Default: 0.
	MinReplicas int `json:"minReplicas,omitempty"`

	// Image is the container image reference. Required.
	Image string `json:"image" binding:"required"`

	// ContainerName is the optional container name.
	ContainerName string `json:"containerName,omitempty"`

	// Env is the list of environment variables.
	Env []EnvVar `json:"env,omitempty"`

	// VolumeMounts maps volumes into the container.
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`

	// RouteSpec configures external routing. Optional.
	RouteSpec *RouteSpec `json:"routeSpec,omitempty"`

	// LogSpec configures log shipping. Optional.
	LogSpec *LogSpec `json:"logSpec,omitempty"`

	// NFSVolumes lists NFS volumes to provision. Optional.
	NFSVolumes []NFSVolume `json:"nfsVolumes,omitempty"`

	// Sources lists KEDA trigger sources. Optional.
	Sources []KedaSource `json:"sources,omitempty"`
}

// ── Response types ────────────────────────────────────────────────────────────

// ConditionResponse is a single status condition flattened for the UI.
type ConditionResponse struct {
	// Source identifies which sub-system produced this condition
	// (e.g. "knative", "logging", "route.certificate").
	Source             string `json:"source"`
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

// ApplicationLinksResponse holds the cluster console and site link.
type ApplicationLinksResponse struct {
	Site        string `json:"site,omitempty"`
	ConsoleLink string `json:"consoleLink,omitempty"`
}

// StateStatusResponse reflects the Capp's current enabled/disabled state.
type StateStatusResponse struct {
	State      string `json:"state,omitempty"`
	LastChange string `json:"lastChange,omitempty"`
}

// CappStatusResponse is the status sub-object in a CappResponse.
// It exposes a flattened view of the condition tree that the frontend's
// Conditions table renders.
type CappStatusResponse struct {
	Conditions       []ConditionResponse      `json:"conditions"`
	ApplicationLinks ApplicationLinksResponse `json:"applicationLinks"`
	StateStatus      StateStatusResponse      `json:"stateStatus"`
}

// CappResponse is returned by all read and write endpoints.
type CappResponse struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	CreatedAt       string            `json:"createdAt,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`

	ScaleMetric   string `json:"scaleMetric,omitempty"`
	State         string `json:"state,omitempty"`
	MinReplicas   int    `json:"minReplicas"`
	Image         string `json:"image,omitempty"`
	ContainerName string `json:"containerName,omitempty"`

	Env          []EnvVar      `json:"env,omitempty"`
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`
	RouteSpec    *RouteSpec    `json:"routeSpec,omitempty"`
	LogSpec      *LogSpec      `json:"logSpec,omitempty"`
	NFSVolumes   []NFSVolume   `json:"nfsVolumes,omitempty"`
	Sources      []KedaSource  `json:"sources,omitempty"`

	Status CappStatusResponse `json:"status"`
}

// CappListResponse is the envelope returned by the list endpoints.
type CappListResponse struct {
	Items []CappResponse `json:"items"`
	Total int            `json:"total"`
}
