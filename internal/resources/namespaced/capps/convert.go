package capps

// convert.go is the single place where frontend DTOs are translated to and
// from Kubernetes Capp objects. All coupling to the Kubernetes type structure
// is contained here — changing the K8s schema only requires updating this file.

import (
	"fmt"

	"github.com/dana-team/capp-backend/internal/config"
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
//     the Kubernetes API server can apply CRD defaulting (e.g. scaleSpec.metric
//     defaults to "concurrency", state defaults to "enabled").
//   - resourceVersion is NOT set here; the update handler reads it from the
//     live object and sets it before calling Update.
func ToK8s(req CappRequest, existing *cappv1alpha1.Capp, namespace string, sizes config.CappSizes) (*cappv1alpha1.Capp, error) {

	capp := &cappv1alpha1.Capp{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rcs.dana.io/v1alpha1",
			Kind:       "Capp",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: cappv1alpha1.CappSpec{},
	}
	capp.Spec.ConfigurationSpec = knativev1.ConfigurationSpec{
		Template: knativev1.RevisionTemplateSpec{
			Spec: knativev1.RevisionSpec{
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{},
					},
				},
			},
		},
	}

	// Copy unmanaged fields from the existing object to preserve them across updates.
	if existing != nil {
		capp.Spec = *existing.Spec.DeepCopy()
	}

	capp.Spec.State = req.State

	capp.Spec.ScaleSpec = cappv1alpha1.ScaleSpec{
		Metric:            req.ScaleSpec.Metric,
		MinReplicas:       req.ScaleSpec.MinReplicas,
		ScaleDelaySeconds: req.ScaleSpec.ScaleDelaySeconds,
	}

	// Build the container spec.

	container := corev1.Container{}
	if len(capp.Spec.ConfigurationSpec.Template.Spec.Containers) > 0 {
		container = capp.Spec.ConfigurationSpec.Template.Spec.Containers[0]
	}
	container.Name = req.ContainerName
	container.Image = req.Image

	container.Env = nil
	for _, e := range req.Env {
		ev := corev1.EnvVar{Name: e.Name}
		if e.ValueFrom != nil {
			bothSet := e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.ConfigMapKeyRef != nil
			neitherSet := e.ValueFrom.SecretKeyRef == nil && e.ValueFrom.ConfigMapKeyRef == nil
			if bothSet || neitherSet {
				return nil, fmt.Errorf("env var %q: valueFrom must set exactly one of secretKeyRef or configMapKeyRef", e.Name)
			}
			ev.ValueFrom = &corev1.EnvVarSource{}
			if e.ValueFrom.SecretKeyRef != nil {
				ev.ValueFrom.SecretKeyRef = &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.ValueFrom.SecretKeyRef.Name},
					Key:                  e.ValueFrom.SecretKeyRef.Key,
				}
			} else {
				ev.ValueFrom.ConfigMapKeyRef = &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.ValueFrom.ConfigMapKeyRef.Name},
					Key:                  e.ValueFrom.ConfigMapKeyRef.Key,
				}
			}
		} else {
			ev.Value = e.Value
		}
		container.Env = append(container.Env, ev)
	}

	container.VolumeMounts = nil
	capp.Spec.ConfigurationSpec.Template.Spec.Volumes = nil

	// Validate and collect volume/mount names to catch duplicates early.
	volumeNames := make(map[string]struct{}, len(req.VolumeMounts)+len(req.SecretVolumes)+len(req.ConfigMapVolumes))
	for _, vm := range req.VolumeMounts {
		if _, exists := volumeNames[vm.Name]; exists {
			return nil, fmt.Errorf("duplicate volume mount name %q in volumeMounts", vm.Name)
		}
		volumeNames[vm.Name] = struct{}{}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      vm.Name,
			MountPath: vm.MountPath,
		})
	}

	// Secret volumes
	extraVolumes := make([]corev1.Volume, 0, len(req.SecretVolumes)+len(req.ConfigMapVolumes))
	for _, sv := range req.SecretVolumes {
		if _, exists := volumeNames[sv.Name]; exists {
			return nil, fmt.Errorf("duplicate volume name %q in secretVolumes", sv.Name)
		}
		volumeNames[sv.Name] = struct{}{}
		extraVolumes = append(extraVolumes, corev1.Volume{
			Name: sv.Name,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: sv.SecretName},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      sv.Name,
			MountPath: sv.MountPath,
		})
	}
	// ConfigMap volumes
	for _, cv := range req.ConfigMapVolumes {
		if _, exists := volumeNames[cv.Name]; exists {
			return nil, fmt.Errorf("duplicate volume name %q in configMapVolumes", cv.Name)
		}
		volumeNames[cv.Name] = struct{}{}
		extraVolumes = append(extraVolumes, corev1.Volume{
			Name: cv.Name,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cv.ConfigMapName},
				},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      cv.Name,
			MountPath: cv.MountPath,
		})
	}

	capp.Spec.ConfigurationSpec.Template.Spec.Volumes = extraVolumes
	capp.Spec.ConfigurationSpec.Template.Spec.Containers = []corev1.Container{container}

	// Route spec.
	capp.Spec.RouteSpec = cappv1alpha1.RouteSpec{}
	if req.RouteSpec != nil {
		capp.Spec.RouteSpec = cappv1alpha1.RouteSpec{
			Hostname:            req.RouteSpec.Hostname,
			TlsEnabled:          req.RouteSpec.TLSEnabled,
			RouteTimeoutSeconds: req.RouteSpec.RouteTimeoutSeconds,
		}
	}

	// Log spec.
	capp.Spec.LogSpec = cappv1alpha1.LogSpec{}
	if req.LogSpec != nil {
		capp.Spec.LogSpec = cappv1alpha1.LogSpec{
			Type:           cappv1alpha1.LogType(req.LogSpec.Type),
			Host:           req.LogSpec.Host,
			Index:          req.LogSpec.Index,
			User:           req.LogSpec.User,
			PasswordSecret: req.LogSpec.PasswordSecret,
		}
	}

	// NFS volumes.
	nfsVols := make([]cappv1alpha1.NFSVolume, 0, len(req.NFSVolumes))
	for _, v := range req.NFSVolumes {
		qty, err := resource.ParseQuantity(v.Capacity)
		if err != nil {
			return nil, fmt.Errorf("invalid NFS volume capacity %q: %w", v.Capacity, err)
		}
		nfsVols = append(nfsVols, cappv1alpha1.NFSVolume{
			Name:     v.Name,
			Server:   v.Server,
			Path:     v.Path,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: qty},
		})
	}
	capp.Spec.VolumesSpec = cappv1alpha1.VolumesSpec{NFSVolumes: nfsVols}

	// resources
	if req.Size != "" {
		var requests, limits config.ResourceQuantities
		switch req.Size {
		case CappSizeSmall:
			requests = sizes.Small.Requests
			limits = sizes.Small.Limits
		case CappSizeLarge:
			requests = sizes.Large.Requests
			limits = sizes.Large.Limits
		case CappSizeMedium:
			requests = sizes.Medium.Requests
			limits = sizes.Medium.Limits
		default:
			return nil, fmt.Errorf("invalid size %q: must be one of small, medium, or large", req.Size)
		}

		reqCPU, err := resource.ParseQuantity(requests.CPU)
		if err != nil {
			return nil, fmt.Errorf("size %q: invalid requests.cpu %q: %w", req.Size, requests.CPU, err)
		}
		reqMem, err := resource.ParseQuantity(requests.Memory)
		if err != nil {
			return nil, fmt.Errorf("size %q: invalid requests.memory %q: %w", req.Size, requests.Memory, err)
		}
		limCPU, err := resource.ParseQuantity(limits.CPU)
		if err != nil {
			return nil, fmt.Errorf("size %q: invalid limits.cpu %q: %w", req.Size, limits.CPU, err)
		}
		limMem, err := resource.ParseQuantity(limits.Memory)
		if err != nil {
			return nil, fmt.Errorf("size %q: invalid limits.memory %q: %w", req.Size, limits.Memory, err)
		}
		capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    reqCPU,
				corev1.ResourceMemory: reqMem,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    limCPU,
				corev1.ResourceMemory: limMem,
			},
		}

	}
	return capp, nil
}

// sizeFromResources reverse-maps container resource limits back to a t-shirt
// size label by comparing against the configured size definitions.
// Returns empty string when no size matches (e.g. custom resource limits).
func sizeFromResources(res corev1.ResourceRequirements, sizes config.CappSizes) string {
	type candidate struct {
		name   string
		limits config.ResourceQuantities
	}
	for _, c := range []candidate{
		{string(CappSizeSmall), sizes.Small.Limits},
		{string(CappSizeMedium), sizes.Medium.Limits},
		{string(CappSizeLarge), sizes.Large.Limits},
	} {
		cpu, errC := resource.ParseQuantity(c.limits.CPU)
		mem, errM := resource.ParseQuantity(c.limits.Memory)
		if errC != nil || errM != nil {
			continue
		}
		gotCPU := res.Limits[corev1.ResourceCPU]
		gotMem := res.Limits[corev1.ResourceMemory]
		if cpu.Cmp(gotCPU) == 0 && mem.Cmp(gotMem) == 0 {
			return c.name
		}
	}
	return ""
}

// FromK8s converts a live rcs.dana.io/v1alpha1.Capp into a CappResponse DTO.
// Status fields are included verbatim from the resource's Status sub-object.
func FromK8s(capp *cappv1alpha1.Capp, sizes config.CappSizes) CappResponse {
	resp := CappResponse{
		Name:            capp.Name,
		Namespace:       capp.Namespace,
		UID:             string(capp.UID),
		ResourceVersion: capp.ResourceVersion,
		Labels:          capp.Labels,
		Annotations:     filterAnnotations(capp.Annotations),

		ScaleSpec: ScaleSpec{
			Metric:            capp.Spec.ScaleSpec.Metric,
			MinReplicas:       capp.Spec.ScaleSpec.MinReplicas,
			ScaleDelaySeconds: capp.Spec.ScaleSpec.ScaleDelaySeconds,
		},
		State: capp.Spec.State,
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
		if s := sizeFromResources(c.Resources, sizes); s != "" {
			resp.Size = CappSize(s)
		}
		for _, e := range c.Env {
			ev := EnvVar{Name: e.Name}
			if e.ValueFrom != nil {
				src := &EnvVarSource{}
				if e.ValueFrom.SecretKeyRef != nil {
					src.SecretKeyRef = &KeySelector{
						Name: e.ValueFrom.SecretKeyRef.Name,
						Key:  e.ValueFrom.SecretKeyRef.Key,
					}
				}
				if e.ValueFrom.ConfigMapKeyRef != nil {
					src.ConfigMapKeyRef = &KeySelector{
						Name: e.ValueFrom.ConfigMapKeyRef.Name,
						Key:  e.ValueFrom.ConfigMapKeyRef.Key,
					}
				}
				ev.ValueFrom = src
			} else {
				ev.Value = e.Value
			}
			resp.Env = append(resp.Env, ev)
		}
		for _, vm := range c.VolumeMounts {
			resp.VolumeMounts = append(resp.VolumeMounts, VolumeMount{
				Name:      vm.Name,
				MountPath: vm.MountPath,
			})
		}
	}

	// Secret and ConfigMap volumes.
	podSpec := capp.Spec.ConfigurationSpec.Template.Spec
	mountByName := make(map[string]string)
	if len(containers) > 0 {
		for _, vm := range containers[0].VolumeMounts {
			mountByName[vm.Name] = vm.MountPath
		}
	}
	for _, vol := range podSpec.Volumes {
		mountPath, mounted := mountByName[vol.Name]
		if !mounted {
			continue
		}
		if vol.Secret != nil {
			resp.SecretVolumes = append(resp.SecretVolumes, SecretVolume{
				Name:       vol.Name,
				SecretName: vol.Secret.SecretName,
				MountPath:  mountPath,
			})
		} else if vol.ConfigMap != nil {
			resp.ConfigMapVolumes = append(resp.ConfigMapVolumes, ConfigMapVolume{
				Name:          vol.Name,
				ConfigMapName: vol.ConfigMap.Name,
				MountPath:     mountPath,
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
			Type:           string(ls.Type),
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

	// Status conditions.
	resp.Status = buildStatus(capp)

	return resp
}

// buildStatus flattens the multi-level Capp status into a single condition
// list that the frontend's Conditions table can render without traversing
// nested objects.
func buildStatus(capp *cappv1alpha1.Capp) CappStatusResponse {
	conditions := make([]ConditionResponse, 0, len(capp.Status.Conditions)+len(capp.Status.KnativeObjectStatus.Conditions)+len(capp.Status.LoggingStatus.Conditions)+len(capp.Status.RouteStatus.DomainMappingObjectStatus.Conditions))

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
