package capps

import (
	"fmt"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// helmValues mirrors the values.yaml schema of the helm/capp-template chart.
// It is used to serialize a live Capp into a per-capp values file that
// overrides the chart defaults for ArgoCD deployment.
type helmValues struct {
	Name          string            `json:"name"`
	Namespace     string            `json:"namespace"`
	Image         string            `json:"image"`
	ContainerName string            `json:"containerName,omitempty"`
	ScaleMetric   string            `json:"scaleMetric,omitempty"`
	State         string            `json:"state,omitempty"`
	MinReplicas   int               `json:"minReplicas"`
	Env           []helmEnvVar      `json:"env,omitempty"`
	VolumeMounts  []helmVolumeMount `json:"volumeMounts,omitempty"`
	RouteSpec     *helmRouteSpec    `json:"routeSpec,omitempty"`
	LogSpec       *helmLogSpec      `json:"logSpec,omitempty"`
	NFSVolumes    []helmNFSVolume   `json:"nfsVolumes,omitempty"`
	Sources       []helmKedaSource  `json:"sources,omitempty"`
}

type helmEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type helmVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type helmRouteSpec struct {
	Hostname            string `json:"hostname,omitempty"`
	TLSEnabled          bool   `json:"tlsEnabled"`
	RouteTimeoutSeconds *int64 `json:"routeTimeoutSeconds,omitempty"`
}

type helmLogSpec struct {
	Type           string `json:"type"`
	Host           string `json:"host"`
	Index          string `json:"index,omitempty"`
	User           string `json:"user"`
	PasswordSecret string `json:"passwordSecret"`
}

type helmNFSVolume struct {
	Name     string `json:"name"`
	Server   string `json:"server"`
	Path     string `json:"path"`
	Capacity string `json:"capacity"`
}

type helmKedaSource struct {
	Name           string            `json:"name"`
	ScalarType     string            `json:"scalarType"`
	ScalarMetadata map[string]string `json:"scalarMetadata,omitempty"`
	MinReplicas    *int32            `json:"minReplicas,omitempty"`
	MaxReplicas    *int32            `json:"maxReplicas,omitempty"`
}

// GenerateValues converts a live Capp into a YAML-encoded values file
// suitable for the helm/capp-template chart.
func GenerateValues(capp *cappv1alpha1.Capp) ([]byte, error) {
	vals := helmValues{
		Name:        capp.Name,
		Namespace:   capp.Namespace,
		ScaleMetric: capp.Spec.ScaleMetric,
		State:       capp.Spec.State,
		MinReplicas: capp.Spec.MinReplicas,
	}

	containers := capp.Spec.ConfigurationSpec.Template.Spec.Containers
	if len(containers) > 0 {
		c := containers[0]
		vals.Image = c.Image
		vals.ContainerName = c.Name
		for _, e := range c.Env {
			vals.Env = append(vals.Env, helmEnvVar{Name: e.Name, Value: e.Value})
		}
		for _, vm := range c.VolumeMounts {
			vals.VolumeMounts = append(vals.VolumeMounts, helmVolumeMount{
				Name: vm.Name, MountPath: vm.MountPath,
			})
		}
	}

	if rs := capp.Spec.RouteSpec; rs.Hostname != "" || rs.TlsEnabled || rs.RouteTimeoutSeconds != nil {
		vals.RouteSpec = &helmRouteSpec{
			Hostname:            rs.Hostname,
			TLSEnabled:          rs.TlsEnabled,
			RouteTimeoutSeconds: rs.RouteTimeoutSeconds,
		}
	}

	if ls := capp.Spec.LogSpec; ls.Type != "" {
		vals.LogSpec = &helmLogSpec{
			Type:           string(ls.Type),
			Host:           ls.Host,
			Index:          ls.Index,
			User:           ls.User,
			PasswordSecret: ls.PasswordSecret,
		}
	}

	for _, v := range capp.Spec.VolumesSpec.NFSVolumes {
		capacity := ""
		if q, ok := v.Capacity[corev1.ResourceStorage]; ok {
			capacity = q.String()
		}
		vals.NFSVolumes = append(vals.NFSVolumes, helmNFSVolume{
			Name: v.Name, Server: v.Server, Path: v.Path, Capacity: capacity,
		})
	}

	for _, s := range capp.Spec.Sources {
		vals.Sources = append(vals.Sources, helmKedaSource{
			Name:           s.Name,
			ScalarType:     s.ScalarType,
			ScalarMetadata: s.ScalarMetadata,
			MinReplicas:    s.MinReplicas,
			MaxReplicas:    s.MaxReplicas,
		})
	}

	out, err := yaml.Marshal(vals)
	if err != nil {
		return nil, fmt.Errorf("marshal values: %w", err)
	}
	return out, nil
}
