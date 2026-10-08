package capps

import (
	"context"
	"errors"
	"fmt"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// migrateTarget holds the resolved target cluster state needed by migration
// handlers. Returned by resolveTarget after all pre-flight checks pass.
type migrateTarget struct {
	meta         cluster.ClusterMeta
	targetClient client.Client
}

// resolveTarget validates the migration target and returns a ready-to-use
// migrateTarget. On failure it writes the error response to c and returns
// nil, false.
func (h *Handler) resolveTarget(c *gin.Context, targetCluster, targetNamespace, sourceNamespace string) (*migrateTarget, bool) {
	meta, err := extractClusterMeta(c)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return nil, false
	}

	if targetCluster == meta.Name && targetNamespace == sourceNamespace {
		apierrors.Respond(c, apierrors.NewBadRequest("cannot migrate to the same cluster and namespace"))
		return nil, false
	}

	credVal, exists := c.Get(string(middleware.CredentialKey))
	if !exists {
		apierrors.Respond(c, apierrors.NewInternal(fmt.Errorf("credential not found in context")))
		return nil, false
	}
	cred, ok := credVal.(auth.ClusterCredential)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(fmt.Errorf("credential has unexpected type in context")))
		return nil, false
	}

	targetCC, err := h.clusterMgr.Get(targetCluster)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) {
			apierrors.Respond(c, apierrors.NewClusterNotFound(targetCluster))
			return nil, false
		}
		apierrors.Respond(c, apierrors.NewInternal(err))
		return nil, false
	}
	if !targetCC.IsHealthy() {
		apierrors.Respond(c, apierrors.NewClusterUnhealthy(targetCluster))
		return nil, false
	}

	if !h.clusterMgr.IsNamespaceAllowed(targetCC, targetNamespace) {
		apierrors.Respond(c, apierrors.NewNamespaceDenied(targetNamespace, targetCluster))
		return nil, false
	}

	targetClient, err := h.clusterMgr.ClientFor(targetCC, cred)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(fmt.Errorf("build target client: %w", err)))
		return nil, false
	}

	ctx := c.Request.Context()

	var targetNS corev1.Namespace
	if err := targetClient.Get(ctx, client.ObjectKey{Name: targetNamespace}, &targetNS); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Namespace", targetNamespace))
			return nil, false
		}
		apierrors.Respond(c, err)
		return nil, false
	}

	return &migrateTarget{meta: meta, targetClient: targetClient}, true
}

// migrationBypassAnnotation tells the operator webhook to skip the DNS
// hostname uniqueness check during migration.
var migrationBypassAnnotation = consts.CappAPIGroup + "/skip-dns-check"

// deleteMigratedSource deletes the source Capp and removes the bypass
// annotation from the target if present. Returns (true, nil) on full success,
// (false, err) if the source delete fails, or (true, err) if only the
// annotation patch fails.
func deleteMigratedSource(ctx context.Context, sourceClient, targetClient client.Client, sourceCapp, targetCapp *cappv1alpha1.Capp) (bool, error) {
	if err := sourceClient.Delete(ctx, sourceCapp); err != nil {
		return false, err
	}

	if _, hasBypass := targetCapp.Annotations[migrationBypassAnnotation]; hasBypass {
		patch := client.MergeFrom(targetCapp.DeepCopy())
		delete(targetCapp.Annotations, migrationBypassAnnotation)
		if err := targetClient.Patch(ctx, targetCapp, patch); err != nil {
			return true, fmt.Errorf("remove bypass annotation: %w", err)
		}
	}

	return true, nil
}

// prepareCapp returns a deep copy of source suitable for creation on the target cluster.
// It strips cluster-specific metadata (UID, resourceVersion, creationTimestamp, status,
// managedFields, ownerReferences) and sets the namespace to targetNamespace.
func prepareCapp(source *cappv1alpha1.Capp, targetNamespace, targetHostname string) *cappv1alpha1.Capp {
	target := source.DeepCopy()
	stripMetadata(target, targetNamespace)
	target.Status = cappv1alpha1.CappStatus{}

	if targetHostname != "" {
		target.Spec.RouteSpec.Hostname = targetHostname
		return target
	}

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

// validateHostnameMap checks that every hostnameMap key matches a Capp in the
// list and that each Capp's hostname is valid per resolveTargetHostname. On
// failure it returns a 400 error; on success it returns nil.
func validateHostnameMap(capps []cappv1alpha1.Capp, hostnameMap map[string]string, deleteSource bool) error {
	names := make(map[string]struct{}, len(capps))
	for i := range capps {
		names[capps[i].Name] = struct{}{}
	}

	for key := range hostnameMap {
		if _, ok := names[key]; !ok {
			return apierrors.NewBadRequest(fmt.Sprintf("hostnameMap contains unknown Capp name: %q", key))
		}
	}

	for i := range capps {
		targetHostname := hostnameMap[capps[i].Name]
		if _, err := resolveTargetHostname(capps[i].Spec.RouteSpec.Hostname, targetHostname, deleteSource); err != nil {
			return apierrors.NewBadRequest(fmt.Sprintf("Capp %q: %s", capps[i].Name, err.Error()))
		}
	}

	return nil
}

// resolveTargetHostname validates the targetHostname field against the source
// Capp's hostname and the deleteSource flag. It returns the effective hostname
// to pass to prepareCapp, or an error if the combination is invalid.
func resolveTargetHostname(sourceHostname, targetHostname string, deleteSource bool) (string, error) {
	if targetHostname != "" && sourceHostname == "" {
		return "", apierrors.NewBadRequest("targetHostname cannot be set when the source Capp has no hostname")
	}

	if sourceHostname != "" && !deleteSource {
		if targetHostname == "" {
			return "", apierrors.NewBadRequest("targetHostname is required when copying a Capp with a custom hostname")
		}
		if targetHostname == sourceHostname {
			return "", apierrors.NewBadRequest("targetHostname must differ from the source hostname")
		}
	}

	if deleteSource && targetHostname == sourceHostname {
		return "", nil
	}

	return targetHostname, nil
}
