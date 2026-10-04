package capps

import (
	"context"
	"fmt"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	knativev1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPrepareCapp(t *testing.T) {
	const hostname = "app.example.com"

	source := &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "my-app",
			Namespace:       "source-ns",
			UID:             types.UID("abc-123"),
			ResourceVersion: "999",
			Generation:      5,
			CreationTimestamp: metav1.Time{
				Time: metav1.Now().Time,
			},
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "controller"},
			},
			OwnerReferences: []metav1.OwnerReference{
				{Name: "parent", UID: "owner-uid"},
			},
			Labels:      map[string]string{"app": "test"},
			Annotations: map[string]string{"existing": "value"},
		},
		Spec: cappv1alpha1.CappSpec{
			ConfigurationSpec: knativev1.ConfigurationSpec{
				Template: knativev1.RevisionTemplateSpec{
					Spec: knativev1.RevisionSpec{
						PodSpec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "nginx:latest"},
							},
						},
					},
				},
			},
		},
		Status: cappv1alpha1.CappStatus{
			StateStatus: cappv1alpha1.StateStatus{State: "enabled"},
		},
	}

	t.Run("strips cluster-specific metadata", func(t *testing.T) {
		result := prepareCapp(source, "target-ns", "")

		assert.Equal(t, types.UID(""), result.UID)
		assert.Empty(t, result.ResourceVersion)
		assert.True(t, result.CreationTimestamp.IsZero())
		assert.Nil(t, result.ManagedFields)
		assert.Nil(t, result.OwnerReferences)
		assert.Equal(t, cappv1alpha1.CappStatus{}, result.Status)
		assert.Zero(t, result.Generation)
		assert.Empty(t, result.GenerateName)
	})

	t.Run("sets target namespace", func(t *testing.T) {
		result := prepareCapp(source, "other-ns", "")
		assert.Equal(t, "other-ns", result.Namespace)
	})

	t.Run("preserves name labels and spec", func(t *testing.T) {
		result := prepareCapp(source, "target-ns", "")

		assert.Equal(t, "my-app", result.Name)
		assert.Equal(t, map[string]string{"app": "test"}, result.Labels)
		assert.Equal(t, "nginx:latest", result.Spec.ConfigurationSpec.Template.Spec.PodSpec.Containers[0].Image)
	})

	t.Run("does not mutate source", func(t *testing.T) {
		original := source.DeepCopy()
		_ = prepareCapp(source, "target-ns", "")

		assert.Equal(t, original.UID, source.UID)
		assert.Equal(t, original.Namespace, source.Namespace)
		assert.Equal(t, original.ResourceVersion, source.ResourceVersion)
	})

	t.Run("adds bypass annotation when hostname is set", func(t *testing.T) {
		withHostname := source.DeepCopy()
		withHostname.Spec.RouteSpec.Hostname = hostname

		result := prepareCapp(withHostname, "target-ns", "")

		require.Contains(t, result.Annotations, migrationBypassAnnotation)
		assert.Equal(t, "true", result.Annotations[migrationBypassAnnotation])
		assert.Equal(t, "value", result.Annotations["existing"])
	})

	t.Run("no bypass annotation when hostname is empty", func(t *testing.T) {
		result := prepareCapp(source, "target-ns", "")
		assert.NotContains(t, result.Annotations, migrationBypassAnnotation)
	})

	t.Run("adds bypass annotation when source has nil annotations", func(t *testing.T) {
		withHostname := source.DeepCopy()
		withHostname.Spec.RouteSpec.Hostname = hostname
		withHostname.Annotations = nil

		result := prepareCapp(withHostname, "target-ns", "")

		require.NotNil(t, result.Annotations)
		assert.Equal(t, "true", result.Annotations[migrationBypassAnnotation])
	})

	t.Run("overrides hostname and skips bypass when targetHostname set", func(t *testing.T) {
		withHostname := source.DeepCopy()
		withHostname.Spec.RouteSpec.Hostname = hostname

		result := prepareCapp(withHostname, "target-ns", "new.example.com")

		assert.Equal(t, "new.example.com", result.Spec.RouteSpec.Hostname)
		assert.NotContains(t, result.Annotations, migrationBypassAnnotation)
	})
}

func TestPrepareDependentResource(t *testing.T) {
	t.Run("strips metadata from Secret", func(t *testing.T) {
		source := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "my-secret",
				Namespace:       "source-ns",
				UID:             types.UID("sec-uid"),
				ResourceVersion: "42",
				Generation:      3,
				CreationTimestamp: metav1.Time{
					Time: metav1.Now().Time,
				},
				ManagedFields: []metav1.ManagedFieldsEntry{
					{Manager: "controller"},
				},
				OwnerReferences: []metav1.OwnerReference{
					{Name: "parent"},
				},
				Labels: map[string]string{"team": "a"},
			},
			Data: map[string][]byte{"password": []byte("secret")},
		}

		result := prepareDependentResource(source, "target-ns")

		assert.Equal(t, "my-secret", result.Name)
		assert.Equal(t, "target-ns", result.Namespace)
		assert.Equal(t, types.UID(""), result.UID)
		assert.Empty(t, result.ResourceVersion)
		assert.True(t, result.CreationTimestamp.IsZero())
		assert.Nil(t, result.ManagedFields)
		assert.Nil(t, result.OwnerReferences)
		assert.Zero(t, result.Generation)
		assert.Equal(t, map[string]string{"team": "a"}, result.Labels)
		assert.Equal(t, []byte("secret"), result.Data["password"])
	})

	t.Run("strips metadata from ConfigMap", func(t *testing.T) {
		source := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "my-cm",
				Namespace:       "source-ns",
				UID:             types.UID("cm-uid"),
				ResourceVersion: "10",
				Generation:      2,
				CreationTimestamp: metav1.Time{
					Time: metav1.Now().Time,
				},
				ManagedFields: []metav1.ManagedFieldsEntry{
					{Manager: "kubectl"},
				},
				OwnerReferences: []metav1.OwnerReference{
					{Name: "owner"},
				},
			},
			Data: map[string]string{"key": "value"},
		}

		result := prepareDependentResource(source, "target-ns")

		assert.Equal(t, "my-cm", result.Name)
		assert.Equal(t, "target-ns", result.Namespace)
		assert.Equal(t, types.UID(""), result.UID)
		assert.Empty(t, result.ResourceVersion)
		assert.True(t, result.CreationTimestamp.IsZero())
		assert.Nil(t, result.ManagedFields)
		assert.Nil(t, result.OwnerReferences)
		assert.Zero(t, result.Generation)
		assert.Equal(t, "value", result.Data["key"])
	})

	t.Run("does not mutate source", func(t *testing.T) {
		source := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "keep",
				Namespace:       "source-ns",
				UID:             types.UID("orig"),
				ResourceVersion: "7",
			},
		}
		original := source.DeepCopy()
		_ = prepareDependentResource(source, "target-ns")

		assert.Equal(t, original.UID, source.UID)
		assert.Equal(t, original.Namespace, source.Namespace)
	})
}

var managedLabels = map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue}

func TestListManagedResources(t *testing.T) {
	t.Run("returns managed secrets and configmaps", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns1", Labels: managedLabels},
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "ns1", Labels: managedLabels},
		}
		k8sClient := testutil.FakeClient(t, secret, cm)

		secrets, configMaps, err := listManagedResources(context.Background(), k8sClient, "ns1")
		require.NoError(t, err)
		assert.Len(t, secrets, 1)
		assert.Equal(t, "s1", secrets[0].Name)
		assert.Len(t, configMaps, 1)
		assert.Equal(t, "cm1", configMaps[0].Name)
	})

	t.Run("excludes unlabeled resources", func(t *testing.T) {
		managed := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "managed", Namespace: "ns1", Labels: managedLabels},
		}
		unlabeled := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "unlabeled", Namespace: "ns1"},
		}
		k8sClient := testutil.FakeClient(t, managed, unlabeled)

		secrets, _, err := listManagedResources(context.Background(), k8sClient, "ns1")
		require.NoError(t, err)
		assert.Len(t, secrets, 1)
		assert.Equal(t, "managed", secrets[0].Name)
	})

	t.Run("excludes resources from other namespaces", func(t *testing.T) {
		inScope := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "source", Labels: managedLabels},
		}
		outOfScope := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: "other", Labels: managedLabels},
		}
		k8sClient := testutil.FakeClient(t, inScope, outOfScope)

		secrets, _, err := listManagedResources(context.Background(), k8sClient, "source")
		require.NoError(t, err)
		assert.Len(t, secrets, 1)
		assert.Equal(t, "s1", secrets[0].Name)
	})

	t.Run("returns empty slices when no managed resources exist", func(t *testing.T) {
		k8sClient := testutil.FakeClient(t)

		secrets, configMaps, err := listManagedResources(context.Background(), k8sClient, "empty-ns")
		require.NoError(t, err)
		assert.Empty(t, secrets)
		assert.Empty(t, configMaps)
	})
}

func TestCopyDependentResources(t *testing.T) {
	t.Run("copies secrets and configmaps to target", func(t *testing.T) {
		secrets := []corev1.Secret{
			{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "src", UID: "uid1", ResourceVersion: "1", Labels: managedLabels}},
		}
		configMaps := []corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "src", UID: "uid2", ResourceVersion: "2", Labels: managedLabels}},
		}
		targetClient := testutil.FakeClient(t)

		err := copyDependentResources(context.Background(), targetClient, "target-ns", secrets, configMaps)
		require.NoError(t, err)

		var s corev1.Secret
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "s1"}, &s))
		assert.Equal(t, "target-ns", s.Namespace)

		var cm corev1.ConfigMap
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "cm1"}, &cm))
		assert.Equal(t, "target-ns", cm.Namespace)
	})

	t.Run("returns conflict on existing secret", func(t *testing.T) {
		existing := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "collision", Namespace: "target-ns"},
		}
		targetClient := testutil.FakeClient(t, existing)

		secrets := []corev1.Secret{
			{ObjectMeta: metav1.ObjectMeta{Name: "collision", Namespace: "src"}},
		}
		err := copyDependentResources(context.Background(), targetClient, "target-ns", secrets, nil)
		require.Error(t, err)

		var apiErr *apierrors.APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, apierrors.CodeConflict, apiErr.Code)
		assert.Contains(t, apiErr.Message, "collision")
	})

	t.Run("returns conflict on existing configmap", func(t *testing.T) {
		existing := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "collision", Namespace: "dest"},
		}
		targetClient := testutil.FakeClient(t, existing)

		configMaps := []corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: "collision", Namespace: "src"}},
		}
		err := copyDependentResources(context.Background(), targetClient, "dest", nil, configMaps)
		require.Error(t, err)

		var apiErr *apierrors.APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, apierrors.CodeConflict, apiErr.Code)
		assert.Contains(t, apiErr.Message, "collision")
	})

	t.Run("no writes when collision detected", func(t *testing.T) {
		existing := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: "target-ns"},
		}
		targetClient := testutil.FakeClient(t, existing)

		secrets := []corev1.Secret{
			{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "src", Labels: managedLabels}},
			{ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: "src", Labels: managedLabels}},
		}
		err := copyDependentResources(context.Background(), targetClient, "target-ns", secrets, nil)
		require.Error(t, err)

		// s1 should not have been created since collision check runs before writes
		var s corev1.Secret
		getErr := targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "s1"}, &s)
		assert.Error(t, getErr)
	})

	t.Run("succeeds with empty resource lists", func(t *testing.T) {
		targetClient := testutil.FakeClient(t)
		err := copyDependentResources(context.Background(), targetClient, "target-ns", nil, nil)
		require.NoError(t, err)
	})

	t.Run("rolls back created secrets on later secret create failure", func(t *testing.T) {
		var createCount int
		targetClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					createCount++
					if createCount == 2 {
						return fmt.Errorf("transient API error")
					}
				}
				return c.Create(ctx, obj, opts...)
			},
		})

		secrets := []corev1.Secret{
			{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "src", Labels: managedLabels}},
			{ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: "src", Labels: managedLabels}},
		}
		err := copyDependentResources(context.Background(), targetClient, "target-ns", secrets, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "transient API error")

		var s corev1.Secret
		getErr := targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "s1"}, &s)
		assert.Error(t, getErr, "s1 should have been cleaned up")
	})

	t.Run("rolls back secrets on configmap create failure", func(t *testing.T) {
		targetClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return fmt.Errorf("configmap create failed")
				}
				return c.Create(ctx, obj, opts...)
			},
		})

		secrets := []corev1.Secret{
			{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "src", Labels: managedLabels}},
		}
		configMaps := []corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "src", Labels: managedLabels}},
		}
		err := copyDependentResources(context.Background(), targetClient, "target-ns", secrets, configMaps)
		require.Error(t, err)

		var s corev1.Secret
		getErr := targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "s1"}, &s)
		assert.Error(t, getErr, "s1 should have been cleaned up")
	})

	t.Run("returns original error when cleanup also fails", func(t *testing.T) {
		var createCount int
		targetClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					createCount++
					if createCount == 2 {
						return fmt.Errorf("create failed")
					}
				}
				return c.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return fmt.Errorf("delete also failed")
				}
				return c.Delete(ctx, obj, opts...)
			},
		})

		secrets := []corev1.Secret{
			{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "src", Labels: managedLabels}},
			{ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: "src", Labels: managedLabels}},
		}
		err := copyDependentResources(context.Background(), targetClient, "target-ns", secrets, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create failed", "original error must be returned, not cleanup error")
	})
}

func TestResolveTargetHostname(t *testing.T) {
	tests := []struct {
		name           string
		sourceHostname string
		targetHostname string
		deleteSource   bool
		want           string
		wantErr        string
	}{
		{
			name:           "copy without hostname succeeds",
			sourceHostname: "", targetHostname: "", deleteSource: false,
			want: "",
		},
		{
			name:           "copy with hostname requires targetHostname",
			sourceHostname: "app.example.com", targetHostname: "", deleteSource: false,
			wantErr: "targetHostname is required",
		},
		{
			name:           "copy with same targetHostname rejected",
			sourceHostname: "app.example.com", targetHostname: "app.example.com", deleteSource: false,
			wantErr: "targetHostname must differ",
		},
		{
			name:           "copy with different targetHostname succeeds",
			sourceHostname: "app.example.com", targetHostname: "new.example.com", deleteSource: false,
			want: "new.example.com",
		},
		{
			name:           "rejects targetHostname when source has no hostname",
			sourceHostname: "", targetHostname: "new.example.com", deleteSource: false,
			wantErr: "targetHostname cannot be set",
		},
		{
			name:           "move with hostname without targetHostname uses bypass",
			sourceHostname: "app.example.com", targetHostname: "", deleteSource: true,
			want: "",
		},
		{
			name:           "move with same targetHostname treated as absent",
			sourceHostname: "app.example.com", targetHostname: "app.example.com", deleteSource: true,
			want: "",
		},
		{
			name:           "move with different targetHostname overrides",
			sourceHostname: "app.example.com", targetHostname: "new.example.com", deleteSource: true,
			want: "new.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTargetHostname(tt.sourceHostname, tt.targetHostname, tt.deleteSource)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
