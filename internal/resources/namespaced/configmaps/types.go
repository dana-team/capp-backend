package configmaps

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
}

type ConfigMapListResponse struct {
	Items []ConfigMapResponse `json:"items"`
	Total int                 `json:"total"`
}
