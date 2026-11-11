// Package boundcapps finds the Capps that reference a Secret or ConfigMap and
// restarts the ones that opted in to reload on change.
//
// A Capp opts in with the annotation consts.BoundResourceRolloutAnnotation set
// to "true". A restart records the resource's resourceVersion in
// consts.BoundResourceVersionsAnnotation on the Capp. That key is deliberately
// outside the rcs.dana.io/ prefix: the operator copies only non-rcs.dana.io
// Capp annotations into the Knative revision template, so changing it is what
// makes Knative create a new revision.
//
// Keying restarts on resourceVersion rather than on a timestamp or a data diff
// makes them exact and idempotent: every update has a new resourceVersion, so
// two updates within the same second each restart the Capp, and retrying a
// PUT whose restarts partly failed restarts only the Capps still behind.
package boundcapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Kind is the kind of resource a Capp can be bound to.
type Kind string

const (
	KindSecret    Kind = "Secret"
	KindConfigMap Kind = "ConfigMap"
)

// Response is returned by GET .../secrets/:name/bound and .../configmaps/:name/bound.
type Response struct {
	// BoundCapps lists every Capp in the namespace that references the resource,
	// from its pod template or (Secrets only) its log or Kafka source settings.
	BoundCapps []string `json:"boundCapps"`
	// ToReload lists the subset of BoundCapps that restart when the resource
	// changes: opted-in Capps whose pod template references it.
	ToReload []string `json:"toReload"`
}

// Find lists the Capps in namespace that reference the resource of the given
// kind and name.
//
// Secrets referenced only by spec.logSpec.passwordSecret or a Kafka source's
// secretRef are bound (so deleting them is refused) but never reloaded: the
// operator reads those Secrets for log shipping and the KafkaSource, not the
// Capp's pods, so restarting the pods would not pick up the change.
func Find(ctx context.Context, k8sClient client.Client, namespace string, kind Kind, name string) (Response, error) {
	cappList := &cappv1alpha1.CappList{}
	if err := k8sClient.List(ctx, cappList, client.InNamespace(namespace)); err != nil {
		return Response{}, err
	}

	resp := Response{BoundCapps: []string{}, ToReload: []string{}}
	for i := range cappList.Items {
		capp := &cappList.Items[i]
		inPods := references(&capp.Spec.ConfigurationSpec.Template.Spec.PodSpec, kind, name)
		if !inPods && !specReferences(&capp.Spec, kind, name) {
			continue
		}
		resp.BoundCapps = append(resp.BoundCapps, capp.Name)
		if inPods && ReloadEnabled(capp.Annotations) {
			resp.ToReload = append(resp.ToReload, capp.Name)
		}
	}
	return resp, nil
}

// ReloadEnabled reports whether the Capp annotations opt in to reload on change.
func ReloadEnabled(annotations map[string]string) bool {
	return annotations[consts.BoundResourceRolloutAnnotation] == consts.BoundResourceRolloutEnabled
}

// TriggerRollouts restarts each named Capp that has not yet been restarted for
// resourceVersion of the given resource. Capps that no longer opt in, or that
// already record this resourceVersion, are left unchanged. All failures are
// returned joined; calling it again retries only the Capps that failed.
func TriggerRollouts(ctx context.Context, k8sClient client.Client, namespace string, cappNames []string, kind Kind, resourceName, resourceVersion string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	key := versionKey(kind, resourceName)

	var errs []error
	for _, cappName := range cappNames {
		if err := triggerRollout(ctx, k8sClient, namespace, cappName, key, resourceVersion, now); err != nil {
			errs = append(errs, fmt.Errorf("capp %s: %w", cappName, err))
		}
	}
	return errors.Join(errs...)
}

func triggerRollout(ctx context.Context, k8sClient client.Client, namespace, cappName, key, resourceVersion, now string) error {
	// The versions annotation is read-modify-write, so use an optimistic Update
	// and retry on conflict: two resources restarting the same Capp at once
	// must not drop each other's entries.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		capp := &cappv1alpha1.Capp{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cappName}, capp); err != nil {
			return err
		}
		if !ReloadEnabled(capp.Annotations) {
			return nil
		}

		versions := parseVersions(capp.Annotations[consts.BoundResourceVersionsAnnotation])
		if versions[key] == resourceVersion {
			return nil
		}
		versions[key] = resourceVersion
		pruneVersions(versions, &capp.Spec.ConfigurationSpec.Template.Spec.PodSpec)

		encoded, err := json.Marshal(versions) // map keys are sorted, so the value is stable
		if err != nil {
			return err
		}
		capp.Annotations[consts.BoundResourceVersionsAnnotation] = string(encoded)
		capp.Annotations[consts.RestartedAtAnnotation] = now
		capp.Annotations[consts.RestartReasonAnnotation] = key
		return k8sClient.Update(ctx, capp)
	})
}

func versionKey(kind Kind, name string) string { return string(kind) + "/" + name }

// parseVersions decodes the versions annotation; a missing or malformed value
// yields an empty map, which at worst causes one extra restart.
func parseVersions(raw string) map[string]string {
	versions := map[string]string{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &versions); err != nil {
			return map[string]string{}
		}
	}
	return versions
}

// pruneVersions drops entries for resources the pod spec no longer references,
// so the annotation does not grow as references are removed.
func pruneVersions(versions map[string]string, spec *corev1.PodSpec) {
	for key := range versions {
		kind, name, ok := strings.Cut(key, "/")
		if !ok || !references(spec, Kind(kind), name) {
			delete(versions, key)
		}
	}
}

// specReferences reports whether Capp settings outside the pod template use the
// resource: the log shipping password Secret and Kafka source credentials.
func specReferences(spec *cappv1alpha1.CappSpec, kind Kind, name string) bool {
	if kind != KindSecret {
		return false
	}
	if spec.LogSpec.PasswordSecret == name {
		return true
	}
	for _, src := range spec.EventSourcesSpec.Sources {
		if src.KafkaSourceConfiguration != nil && src.KafkaSourceConfiguration.SecretRef.Name == name {
			return true
		}
	}
	return false
}

// references reports whether any container or volume in the pod spec uses the resource.
func references(spec *corev1.PodSpec, kind Kind, name string) bool {
	if kind == KindSecret {
		for _, ps := range spec.ImagePullSecrets {
			if ps.Name == name {
				return true
			}
		}
	}

	containers := make([]corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	containers = append(containers, spec.InitContainers...)
	containers = append(containers, spec.Containers...)
	for i := range containers {
		if containerReferences(&containers[i], kind, name) {
			return true
		}
	}

	for i := range spec.Volumes {
		if volumeReferences(&spec.Volumes[i].VolumeSource, kind, name) {
			return true
		}
	}
	return false
}

func containerReferences(c *corev1.Container, kind Kind, name string) bool {
	for _, ef := range c.EnvFrom {
		switch kind {
		case KindSecret:
			if ef.SecretRef != nil && ef.SecretRef.Name == name {
				return true
			}
		case KindConfigMap:
			if ef.ConfigMapRef != nil && ef.ConfigMapRef.Name == name {
				return true
			}
		}
	}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			continue
		}
		switch kind {
		case KindSecret:
			if e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == name {
				return true
			}
		case KindConfigMap:
			if e.ValueFrom.ConfigMapKeyRef != nil && e.ValueFrom.ConfigMapKeyRef.Name == name {
				return true
			}
		}
	}
	return false
}

func volumeReferences(v *corev1.VolumeSource, kind Kind, name string) bool {
	switch kind {
	case KindSecret:
		if v.Secret != nil && v.Secret.SecretName == name {
			return true
		}
	case KindConfigMap:
		if v.ConfigMap != nil && v.ConfigMap.Name == name {
			return true
		}
	}
	if v.Projected == nil {
		return false
	}
	for _, src := range v.Projected.Sources {
		switch kind {
		case KindSecret:
			if src.Secret != nil && src.Secret.Name == name {
				return true
			}
		case KindConfigMap:
			if src.ConfigMap != nil && src.ConfigMap.Name == name {
				return true
			}
		}
	}
	return false
}
