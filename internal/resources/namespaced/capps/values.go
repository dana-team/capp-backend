package capps

import (
	"fmt"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// syncValues is the schema written to per-capp values files in the GitOps
// repository. It embeds CappSpec directly so the Helm values always match the
// upstream CRD definition without maintaining parallel structs.
type syncValues struct {
	Name      string                `json:"name"`
	Namespace string                `json:"namespace"`
	Spec      cappv1alpha1.CappSpec `json:"spec"`
}

// GenerateValues converts a live Capp into a YAML-encoded values file
// suitable for the helm/capp-template chart.
func GenerateValues(capp *cappv1alpha1.Capp) ([]byte, error) {
	vals := syncValues{
		Name:      capp.Name,
		Namespace: capp.Namespace,
		Spec:      capp.Spec,
	}
	out, err := yaml.Marshal(vals)
	if err != nil {
		return nil, fmt.Errorf("marshal values: %w", err)
	}
	return out, nil
}
