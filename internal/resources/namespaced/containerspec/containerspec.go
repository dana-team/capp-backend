// Package containerspec converts the container-level DTOs that several
// resources share (environment variables, CPU and memory) to and from their
// Kubernetes types, so a fix to the conversion reaches every resource.
package containerspec

import (
	"fmt"

	"github.com/dana-team/capp-backend/internal/config"
	api "github.com/dana-team/capp-backend/pkg/api/common"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// EnvVarsToK8s converts API env vars to container env vars. Each variable
// needs a name, and a valueFrom must set exactly one of secretKeyRef or
// configMapKeyRef with both its name and key.
//
// Gin does not validate binding tags inside a slice, so the required fields
// are checked here rather than left to the API server, which accepts some of
// them and then fails to start the pod.
func EnvVarsToK8s(vars []api.EnvVar) ([]corev1.EnvVar, error) {
	var out []corev1.EnvVar
	for i, e := range vars {
		if e.Name == "" {
			return nil, fmt.Errorf("env[%d]: name is required", i)
		}
		ev := corev1.EnvVar{Name: e.Name, Value: e.Value}
		if e.ValueFrom != nil {
			source, err := envVarSourceToK8s(e.Name, e.ValueFrom)
			if err != nil {
				return nil, err
			}
			ev.Value = ""
			ev.ValueFrom = source
		}
		out = append(out, ev)
	}
	return out, nil
}

func envVarSourceToK8s(varName string, src *api.EnvVarSource) (*corev1.EnvVarSource, error) {
	switch {
	case (src.SecretKeyRef == nil) == (src.ConfigMapKeyRef == nil):
		return nil, fmt.Errorf("env var %q: valueFrom must set exactly one of secretKeyRef or configMapKeyRef", varName)
	case src.SecretKeyRef != nil:
		if src.SecretKeyRef.Name == "" || src.SecretKeyRef.Key == "" {
			return nil, fmt.Errorf("env var %q: secretKeyRef needs both name and key", varName)
		}
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: src.SecretKeyRef.Name},
			Key:                  src.SecretKeyRef.Key,
		}}, nil
	default:
		if src.ConfigMapKeyRef.Name == "" || src.ConfigMapKeyRef.Key == "" {
			return nil, fmt.Errorf("env var %q: configMapKeyRef needs both name and key", varName)
		}
		return &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: src.ConfigMapKeyRef.Name},
			Key:                  src.ConfigMapKeyRef.Key,
		}}, nil
	}
}

// EnvVarsFromK8s converts K8s EnvVars into DTO EnvVars.
//
// A variable the DTO cannot express (see Representable) is left out, so the
// result can always be sent back through EnvVarsToK8s. MergeEnvVars keeps
// those variables on the object when the list is replaced.
func EnvVarsFromK8s(envs []corev1.EnvVar) []api.EnvVar {
	result := make([]api.EnvVar, 0, len(envs))
	for _, e := range envs {
		if !Representable(e) {
			continue
		}
		ev := api.EnvVar{Name: e.Name}
		if e.ValueFrom != nil {
			ev.ValueFrom = envVarSourceFromK8s(e.ValueFrom)
		} else {
			ev.Value = e.Value
		}
		result = append(result, ev)
	}
	return result
}

// Representable reports whether the DTO can express the variable: a literal
// value, a Secret key or a ConfigMap key. A fieldRef or resourceFieldRef set
// outside the API cannot be expressed and does not survive a round trip.
func Representable(e corev1.EnvVar) bool {
	return e.ValueFrom == nil || e.ValueFrom.SecretKeyRef != nil || e.ValueFrom.ConfigMapKeyRef != nil
}

// MergeEnvVars returns the env list to store when a request replaces the
// list of a container that already has one: the requested variables, then
// every existing variable the DTO cannot express, unless the request sets a
// variable of the same name. Without it a full-replace update would silently
// delete variables the client was never shown.
func MergeEnvVars(requested, existing []corev1.EnvVar) []corev1.EnvVar {
	names := make(map[string]bool, len(requested))
	for _, e := range requested {
		names[e.Name] = true
	}
	merged := requested
	for _, e := range existing {
		if !Representable(e) && !names[e.Name] {
			merged = append(merged, e)
		}
	}
	return merged
}

func envVarSourceFromK8s(src *corev1.EnvVarSource) *api.EnvVarSource {
	out := &api.EnvVarSource{}
	if src.SecretKeyRef != nil {
		out.SecretKeyRef = &api.KeySelector{Name: src.SecretKeyRef.Name, Key: src.SecretKeyRef.Key}
	}
	if src.ConfigMapKeyRef != nil {
		out.ConfigMapKeyRef = &api.KeySelector{Name: src.ConfigMapKeyRef.Name, Key: src.ConfigMapKeyRef.Key}
	}
	return out
}

// ParseResourceQuantities parses config-level quantities into a K8s
// ResourceRequirements. All four quantities are required.
func ParseResourceQuantities(requests, limits config.ResourceQuantities) (corev1.ResourceRequirements, error) {
	reqCPU, err := resource.ParseQuantity(requests.CPU)
	if err != nil {
		return corev1.ResourceRequirements{}, fmt.Errorf("invalid requests.cpu %q: %w", requests.CPU, err)
	}
	reqMem, err := resource.ParseQuantity(requests.Memory)
	if err != nil {
		return corev1.ResourceRequirements{}, fmt.Errorf("invalid requests.memory %q: %w", requests.Memory, err)
	}
	limCPU, err := resource.ParseQuantity(limits.CPU)
	if err != nil {
		return corev1.ResourceRequirements{}, fmt.Errorf("invalid limits.cpu %q: %w", limits.CPU, err)
	}
	limMem, err := resource.ParseQuantity(limits.Memory)
	if err != nil {
		return corev1.ResourceRequirements{}, fmt.Errorf("invalid limits.memory %q: %w", limits.Memory, err)
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    reqCPU,
			corev1.ResourceMemory: reqMem,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    limCPU,
			corev1.ResourceMemory: limMem,
		},
	}, nil
}

// ParseResourceList parses CPU and memory quantities into a ResourceList.
// An empty quantity is left out.
func ParseResourceList(cpu, memory, label string) (corev1.ResourceList, error) {
	list := make(corev1.ResourceList)
	if cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return nil, fmt.Errorf("invalid %s.cpu %q: %w", label, cpu, err)
		}
		list[corev1.ResourceCPU] = q
	}
	if memory != "" {
		q, err := resource.ParseQuantity(memory)
		if err != nil {
			return nil, fmt.Errorf("invalid %s.memory %q: %w", label, memory, err)
		}
		list[corev1.ResourceMemory] = q
	}
	return list, nil
}

// ParseCustomResources converts a user-supplied ResourceSpec into K8s ResourceRequirements.
func ParseCustomResources(spec *api.ResourceSpec) (corev1.ResourceRequirements, error) {
	var res corev1.ResourceRequirements
	if spec.Requests != nil {
		list, err := ParseResourceList(spec.Requests.CPU, spec.Requests.Memory, "requests")
		if err != nil {
			return res, err
		}
		res.Requests = list
	}
	if spec.Limits != nil {
		list, err := ParseResourceList(spec.Limits.CPU, spec.Limits.Memory, "limits")
		if err != nil {
			return res, err
		}
		res.Limits = list
	}
	return res, nil
}

// ResourceSpecFromK8s converts K8s ResourceRequirements into a DTO ResourceSpec.
// Returns nil when the container has no resources set.
func ResourceSpecFromK8s(res corev1.ResourceRequirements) *api.ResourceSpec {
	if len(res.Requests) == 0 && len(res.Limits) == 0 {
		return nil
	}
	spec := &api.ResourceSpec{}
	if len(res.Requests) > 0 {
		rq := &api.ResourceQuantities{}
		if cpu, ok := res.Requests[corev1.ResourceCPU]; ok {
			rq.CPU = cpu.String()
		}
		if mem, ok := res.Requests[corev1.ResourceMemory]; ok {
			rq.Memory = mem.String()
		}
		spec.Requests = rq
	}
	if len(res.Limits) > 0 {
		lq := &api.ResourceQuantities{}
		if cpu, ok := res.Limits[corev1.ResourceCPU]; ok {
			lq.CPU = cpu.String()
		}
		if mem, ok := res.Limits[corev1.ResourceMemory]; ok {
			lq.Memory = mem.String()
		}
		spec.Limits = lq
	}
	return spec
}
