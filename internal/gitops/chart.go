// Package gitops provides values generation and git push capabilities
// for publishing Capp resources to a dedicated git repository. ArgoCD can
// then sync from that repository using the shared capp-template Helm chart
// combined with per-Capp values files.
package gitops

import (
	"fmt"

	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// GenerateValues extracts the Capp's parameters into a values.yaml suitable
// for use with the helm/capp-template chart. The returned bytes are a YAML
// document matching the chart's values schema.
func GenerateValues(capp *cappv1alpha1.Capp) ([]byte, error) {
	vals := buildValuesMap(capp)

	out, err := yaml.Marshal(vals)
	if err != nil {
		return nil, fmt.Errorf("gitops: failed to marshal values to YAML: %w", err)
	}
	return out, nil
}

func buildValuesMap(capp *cappv1alpha1.Capp) map[string]any {
	vals := map[string]any{
		"name":      capp.Name,
		"namespace": capp.Namespace,
	}

	if len(capp.Labels) > 0 {
		vals["labels"] = capp.Labels
	}
	if filtered := k8s.FilterAnnotations(capp.Annotations); len(filtered) > 0 {
		vals["annotations"] = filtered
	}

	if capp.Spec.ScaleMetric != "" {
		vals["scaleMetric"] = capp.Spec.ScaleMetric
	}
	if capp.Spec.State != "" {
		vals["state"] = capp.Spec.State
	}
	if capp.Spec.MinReplicas != 0 {
		vals["minReplicas"] = capp.Spec.MinReplicas
	}

	extractContainerFields(capp, vals)
	extractRouteSpec(capp, vals)
	extractLogSpec(capp, vals)
	extractNFSVolumes(capp, vals)
	extractSources(capp, vals)

	return vals
}

func extractContainerFields(capp *cappv1alpha1.Capp, vals map[string]any) {
	containers := capp.Spec.ConfigurationSpec.Template.Spec.Containers
	if len(containers) == 0 {
		return
	}
	c := containers[0]
	vals["image"] = c.Image
	if c.Name != "" {
		vals["containerName"] = c.Name
	}
	if len(c.Env) > 0 {
		vals["env"] = convertEnv(c.Env)
	}
	if len(c.VolumeMounts) > 0 {
		vals["volumeMounts"] = convertVolumeMounts(c.VolumeMounts)
	}
}

func extractRouteSpec(capp *cappv1alpha1.Capp, vals map[string]any) {
	rs := capp.Spec.RouteSpec
	if rs.Hostname == "" && !rs.TlsEnabled && rs.RouteTimeoutSeconds == nil {
		return
	}
	route := map[string]any{}
	if rs.Hostname != "" {
		route["hostname"] = rs.Hostname
	}
	if rs.TlsEnabled {
		route["tlsEnabled"] = rs.TlsEnabled
	}
	if rs.RouteTimeoutSeconds != nil {
		route["routeTimeoutSeconds"] = *rs.RouteTimeoutSeconds
	}
	vals["routeSpec"] = route
}

func extractLogSpec(capp *cappv1alpha1.Capp, vals map[string]any) {
	ls := capp.Spec.LogSpec
	if ls.Type == "" {
		return
	}
	logSpec := map[string]any{
		"type": string(ls.Type),
		"host": ls.Host,
		"user": ls.User,
	}
	if ls.Index != "" {
		logSpec["index"] = ls.Index
	}
	if ls.PasswordSecret != "" {
		logSpec["passwordSecret"] = ls.PasswordSecret
	}
	vals["logSpec"] = logSpec
}

func extractNFSVolumes(capp *cappv1alpha1.Capp, vals map[string]any) {
	nfs := capp.Spec.VolumesSpec.NFSVolumes
	if len(nfs) == 0 {
		return
	}
	vols := make([]map[string]any, 0, len(nfs))
	for _, v := range nfs {
		vol := map[string]any{
			"name":   v.Name,
			"server": v.Server,
			"path":   v.Path,
		}
		if q, ok := v.Capacity[corev1.ResourceStorage]; ok {
			vol["capacity"] = q.String()
		}
		vols = append(vols, vol)
	}
	vals["nfsVolumes"] = vols
}

func extractSources(capp *cappv1alpha1.Capp, vals map[string]any) {
	sources := capp.Spec.Sources
	if len(sources) == 0 {
		return
	}
	srcs := make([]map[string]any, 0, len(sources))
	for _, s := range sources {
		src := map[string]any{
			"name":       s.Name,
			"scalarType": s.ScalarType,
		}
		if len(s.ScalarMetadata) > 0 {
			src["scalarMetadata"] = s.ScalarMetadata
		}
		if s.MinReplicas != nil {
			src["minReplicas"] = *s.MinReplicas
		}
		if s.MaxReplicas != nil {
			src["maxReplicas"] = *s.MaxReplicas
		}
		srcs = append(srcs, src)
	}
	vals["sources"] = srcs
}

func convertEnv(envVars []corev1.EnvVar) []map[string]string {
	out := make([]map[string]string, 0, len(envVars))
	for _, e := range envVars {
		out = append(out, map[string]string{"name": e.Name, "value": e.Value})
	}
	return out
}

func convertVolumeMounts(mounts []corev1.VolumeMount) []map[string]string {
	out := make([]map[string]string, 0, len(mounts))
	for _, vm := range mounts {
		out = append(out, map[string]string{"name": vm.Name, "mountPath": vm.MountPath})
	}
	return out
}

