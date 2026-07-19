package secrets

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
}

type SecretListResponse struct {
	Items []SecretResponse `json:"items"`
	Total int              `json:"total"`
}
