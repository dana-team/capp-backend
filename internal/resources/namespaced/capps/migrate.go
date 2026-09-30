package capps

import (
	"context"
	"fmt"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// migrationBypassAnnotation tells the operator webhook to skip the DNS
// hostname uniqueness check during migration.
var migrationBypassAnnotation = consts.CappAPIGroup + "/skip-dns-check"

// prepareCapp returns a deep copy of source suitable for creation on the target cluster.
// It strips cluster-specific metadata (UID, resourceVersion, creationTimestamp, status,
// managedFields, ownerReferences) and sets the namespace to targetNamespace.
// If the source Capp has a hostname set, the DNS bypass annotation is added.
func prepareCapp(source *cappv1alpha1.Capp, targetNamespace string) *cappv1alpha1.Capp {
	target := source.DeepCopy()
	stripMetadata(target, targetNamespace)
	target.Status = cappv1alpha1.CappStatus{}

	if source.Spec.RouteSpec.Hostname != "" {
		if target.Annotations == nil {
			target.Annotations = map[string]string{}
		}
		target.Annotations[migrationBypassAnnotation] = "true"
	}

	return target
}

// prepareDependentResource returns a deep copy of source with cluster-specific metadata
// stripped for creation on the target cluster. It handles any type that implements
// client.Object (Secret, ConfigMap, etc.).
func prepareDependentResource[T client.Object](source T, targetNamespace string) T {
	copied := source.DeepCopyObject()
	target, ok := copied.(T)
	if !ok {
		panic("DeepCopyObject returned unexpected type")
	}

	stripMetadata(target, targetNamespace)
	return target
}

// stripMetadata removes server-assigned metadata from a K8s object so it
// can be created on a different cluster or namespace.
func stripMetadata(obj client.Object, targetNamespace string) {
	obj.SetUID(types.UID(""))
	obj.SetResourceVersion("")
	obj.SetCreationTimestamp(metav1.Time{})
	obj.SetManagedFields(nil)
	obj.SetOwnerReferences(nil)
	obj.SetNamespace(targetNamespace)
	obj.SetGeneration(0)
	obj.SetGenerateName("")
}

// listManagedResources lists all Secrets and ConfigMaps managed by
// capp-backend in the given namespace.
func listManagedResources(ctx context.Context, k8sClient client.Client, namespace string) ([]corev1.Secret, []corev1.ConfigMap, error) {
	labelFilter := client.MatchingLabels{consts.ManagedLabelKey: consts.ManagedLabelValue}
	inNamespace := client.InNamespace(namespace)

	var secrets corev1.SecretList
	if err := k8sClient.List(ctx, &secrets, inNamespace, labelFilter); err != nil {
		return nil, nil, err
	}

	var configMaps corev1.ConfigMapList
	if err := k8sClient.List(ctx, &configMaps, inNamespace, labelFilter); err != nil {
		return nil, nil, err
	}

	return secrets.Items, configMaps.Items, nil
}

// cleanupResources deletes the given secrets and configmaps on a best-effort
// basis. Not-found errors are ignored; the first real error is returned.
func cleanupResources(ctx context.Context, k8sClient client.Client, namespace string, secrets []corev1.Secret, configMaps []corev1.ConfigMap) error {
	var cleanupErr error
	for i := range secrets {
		obj := &corev1.Secret{}
		obj.Name = secrets[i].Name
		obj.Namespace = namespace
		if err := k8sClient.Delete(ctx, obj); err != nil && !k8serrors.IsNotFound(err) && cleanupErr == nil {
			cleanupErr = err
		}
	}

	for i := range configMaps {
		obj := &corev1.ConfigMap{}
		obj.Name = configMaps[i].Name
		obj.Namespace = namespace
		if err := k8sClient.Delete(ctx, obj); err != nil && !k8serrors.IsNotFound(err) && cleanupErr == nil {
			cleanupErr = err
		}
	}

	return cleanupErr
}

// copyDependentResources checks that none of the source Secrets or ConfigMaps
// exist on the target, then creates them all. Returns a conflict error on the
// first collision without writing anything. If a create fails mid-batch,
// already-created resources are best-effort deleted before returning.
func copyDependentResources(ctx context.Context, targetClient client.Client, targetNamespace string, secrets []corev1.Secret, configMaps []corev1.ConfigMap) error {
	for i := range secrets {
		key := client.ObjectKey{Namespace: targetNamespace, Name: secrets[i].Name}
		if err := targetClient.Get(ctx, key, &corev1.Secret{}); err == nil {
			return apierrors.NewConflict(fmt.Sprintf("Secret %q already exists", secrets[i].Name))
		} else if !k8serrors.IsNotFound(err) {
			return err
		}
	}

	for i := range configMaps {
		key := client.ObjectKey{Namespace: targetNamespace, Name: configMaps[i].Name}
		if err := targetClient.Get(ctx, key, &corev1.ConfigMap{}); err == nil {
			return apierrors.NewConflict(fmt.Sprintf("ConfigMap %q already exists", configMaps[i].Name))
		} else if !k8serrors.IsNotFound(err) {
			return err
		}
	}

	var createdSecrets []corev1.Secret
	var createdConfigMaps []corev1.ConfigMap

	for i := range secrets {
		prepared := prepareDependentResource(&secrets[i], targetNamespace)
		if err := targetClient.Create(ctx, prepared); err != nil {
			_ = cleanupResources(ctx, targetClient, targetNamespace, createdSecrets, createdConfigMaps)
			return err
		}
		createdSecrets = append(createdSecrets, secrets[i])
	}

	for i := range configMaps {
		prepared := prepareDependentResource(&configMaps[i], targetNamespace)
		if err := targetClient.Create(ctx, prepared); err != nil {
			_ = cleanupResources(ctx, targetClient, targetNamespace, createdSecrets, createdConfigMaps)
			return err
		}
		createdConfigMaps = append(createdConfigMaps, configMaps[i])
	}

	return nil
}
