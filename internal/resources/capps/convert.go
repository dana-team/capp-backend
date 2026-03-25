package capps

// convert.go is the single place where frontend DTOs are translated to and
// from Kubernetes Capp objects. All coupling to the Kubernetes type structure
// is contained here — changing the K8s schema only requires updating this file.

import (
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	knativev1 "knative.dev/serving/pkg/apis/serving/v1"
)

// ToK8s converts a CappRequest into a rcs.dana.io/v1alpha1 Capp resource
// suitable for creating or replacing via the Kubernetes API.
//
// Design notes:
//   - Fields that the frontend did not populate are omitted from the spec so
//     the Kubernetes API server can apply CRD defaulting (e.g. scaleMetric
//     defaults to "concurrency", state defaults to "enabled").
//   - resourceVersion is NOT set here; the update handler reads it from the
//     live object and sets it before calling Update.
func ToK8s(req CappRequest, namespace string) *cappv1alpha1.Capp {
	capp := &cappv1alpha1.Capp{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rcs.dana.io/v1alpha1",
			Kind:       "Capp",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: cappv1alpha1.CappSpec{
			ScaleMetric: req.ScaleMetric,
			State:       req.State,
			MinReplicas: req.MinReplicas,
		},
	}

	// Build the container spec.
	container := corev1.Container{
		Name:  req.ContainerName,
		Image: req.Image,
	}
	for _, e := range req.Env {
		container.Env = append(container.Env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	for _, vm := range req.VolumeMounts {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      vm.Name,
			MountPath: vm.MountPath,
		})
	}

	capp.Spec.ConfigurationSpec = knativev1.ConfigurationSpec{
		Template: knativev1.RevisionTemplateSpec{
			Spec: knativev1.RevisionSpec{
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{container},
				},
			},
		},
	}

	// Route spec.
	if req.RouteSpec != nil {
		capp.Spec.RouteSpec = cappv1alpha1.RouteSpec{
			Hostname:            req.RouteSpec.Hostname,
			TlsEnabled:          req.RouteSpec.TLSEnabled,
			RouteTimeoutSeconds: req.RouteSpec.RouteTimeoutSeconds,
		}
	}

	// Log spec.
	if req.LogSpec != nil {
		capp.Spec.LogSpec = cappv1alpha1.LogSpec{
			Type:           req.LogSpec.Type,
			Host:           req.LogSpec.Host,
			Index:          req.LogSpec.Index,
			User:           req.LogSpec.User,
			PasswordSecret: req.LogSpec.PasswordSecret,
		}
	}

	// NFS volumes.
	if len(req.NFSVolumes) > 0 {
		nfsVols := make([]cappv1alpha1.NFSVolume, 0, len(req.NFSVolumes))
		for _, v := range req.NFSVolumes {
			qty := resource.MustParse(v.Capacity)
			nfsVols = append(nfsVols, cappv1alpha1.NFSVolume{
				Name:     v.Name,
				Server:   v.Server,
				Path:     v.Path,
				Capacity: corev1.ResourceList{corev1.ResourceStorage: qty},
			})
		}
		capp.Spec.VolumesSpec = cappv1alpha1.VolumesSpec{NFSVolumes: nfsVols}
	}

	// KEDA sources.
	if len(req.Sources) > 0 {
		sources := make([]cappv1alpha1.KedaSource, 0, len(req.Sources))
		for _, s := range req.Sources {
			sources = append(sources, cappv1alpha1.KedaSource{
				Name:           s.Name,
				ScalarType:     s.ScalarType,
				ScalarMetadata: s.ScalarMetadata,
				MinReplicas:    s.MinReplicas,
				MaxReplicas:    s.MaxReplicas,
			})
		}
		capp.Spec.Sources = sources
	}

	return capp
}

// FromK8s converts a live rcs.dana.io/v1alpha1.Capp into a CappResponse DTO.
// Status fields are included verbatim from the resource's Status sub-object.
func FromK8s(capp *cappv1alpha1.Capp) CappResponse {
	resp := CappResponse{
		Name:            capp.Name,
		Namespace:       capp.Namespace,
		UID:             string(capp.UID),
		ResourceVersion: capp.ResourceVersion,
		Labels:          capp.Labels,
		Annotations:     filterAnnotations(capp.Annotations),

		ScaleMetric: capp.Spec.ScaleMetric,
		State:       capp.Spec.State,
		MinReplicas: capp.Spec.MinReplicas,
	}

	if !capp.CreationTimestamp.IsZero() {
		resp.CreatedAt = capp.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z")
	}

	// Extract the first container's details.
	containers := capp.Spec.ConfigurationSpec.Template.Spec.Containers
	if len(containers) > 0 {
		c := containers[0]
		resp.Image = c.Image
		resp.ContainerName = c.Name
		for _, e := range c.Env {
			resp.Env = append(resp.Env, EnvVar{Name: e.Name, Value: e.Value})
		}
		for _, vm := range c.VolumeMounts {
			resp.VolumeMounts = append(resp.VolumeMounts, VolumeMount{
				Name:      vm.Name,
				MountPath: vm.MountPath,
			})
		}
	}

	// Route spec.
	if rs := capp.Spec.RouteSpec; rs.Hostname != "" || rs.TlsEnabled || rs.RouteTimeoutSeconds != nil {
		resp.RouteSpec = &RouteSpec{
			Hostname:            rs.Hostname,
			TLSEnabled:          rs.TlsEnabled,
			RouteTimeoutSeconds: rs.RouteTimeoutSeconds,
		}
	}

	// Log spec.
	if ls := capp.Spec.LogSpec; ls.Type != "" {
		resp.LogSpec = &LogSpec{
			Type:           ls.Type,
			Host:           ls.Host,
			Index:          ls.Index,
			User:           ls.User,
			PasswordSecret: ls.PasswordSecret,
		}
	}

	// NFS volumes.
	for _, v := range capp.Spec.VolumesSpec.NFSVolumes {
		capacity := ""
		if q, ok := v.Capacity[corev1.ResourceStorage]; ok {
			capacity = q.String()
		}
		resp.NFSVolumes = append(resp.NFSVolumes, NFSVolume{
			Name:     v.Name,
			Server:   v.Server,
			Path:     v.Path,
			Capacity: capacity,
		})
	}

	// KEDA sources.
	for _, s := range capp.Spec.Sources {
		resp.Sources = append(resp.Sources, KedaSource{
			Name:           s.Name,
			ScalarType:     s.ScalarType,
			ScalarMetadata: s.ScalarMetadata,
			MinReplicas:    s.MinReplicas,
			MaxReplicas:    s.MaxReplicas,
		})
	}

	// Status conditions.
	resp.Status = buildStatus(capp)

	return resp
}

// buildStatus flattens the multi-level Capp status into a single condition
// list that the frontend's Conditions table can render without traversing
// nested objects.
func buildStatus(capp *cappv1alpha1.Capp) CappStatusResponse {
	var conditions []ConditionResponse

	// Top-level conditions (from the Capp controller).
	for _, c := range capp.Status.Conditions {
		conditions = append(conditions, ConditionResponse{
			Source:             "capp",
			Type:               c.Type,
			Status:             string(c.Status),
			LastTransitionTime: c.LastTransitionTime.UTC().Format("2006-01-02T15:04:05Z"),
			Reason:             c.Reason,
			Message:            c.Message,
		})
	}

	// Knative service conditions.
	for _, c := range capp.Status.KnativeObjectStatus.Conditions {
		conditions = append(conditions, ConditionResponse{
			Source:             "knative",
			Type:               string(c.Type),
			Status:             string(c.Status),
			LastTransitionTime: c.LastTransitionTime.Inner.UTC().Format("2006-01-02T15:04:05Z"),
			Reason:             c.Reason,
			Message:            c.Message,
		})
	}

	// Logging conditions.
	for _, c := range capp.Status.LoggingStatus.Conditions {
		conditions = append(conditions, ConditionResponse{
			Source:             "logging",
			Type:               c.Type,
			Status:             string(c.Status),
			LastTransitionTime: c.LastTransitionTime.UTC().Format("2006-01-02T15:04:05Z"),
			Reason:             c.Reason,
			Message:            c.Message,
		})
	}

	// Route conditions.
	for _, c := range capp.Status.RouteStatus.DomainMappingObjectStatus.Conditions {
		conditions = append(conditions, ConditionResponse{
			Source:             "route.domainmapping",
			Type:               string(c.Type),
			Status:             string(c.Status),
			LastTransitionTime: c.LastTransitionTime.Inner.UTC().Format("2006-01-02T15:04:05Z"),
			Reason:             c.Reason,
			Message:            c.Message,
		})
	}

	stateStatus := StateStatusResponse{State: capp.Status.StateStatus.State}
	if !capp.Status.StateStatus.LastChange.IsZero() {
		stateStatus.LastChange = capp.Status.StateStatus.LastChange.UTC().Format("2006-01-02T15:04:05Z")
	}

	return CappStatusResponse{
		Conditions: conditions,
		ApplicationLinks: ApplicationLinksResponse{
			Site:        capp.Status.ApplicationLinks.Site,
			ConsoleLink: capp.Status.ApplicationLinks.ConsoleLink,
		},
		StateStatus: stateStatus,
	}
}

// filterAnnotations removes internal Kubernetes annotations (kubectl.kubernetes.io/*)
// from the response to avoid leaking operational metadata to the frontend.
func filterAnnotations(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		// Keep only non-internal annotations.
		if len(k) < 20 || k[:20] != "kubectl.kubernetes.i" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
