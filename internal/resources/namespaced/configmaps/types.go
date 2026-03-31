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
