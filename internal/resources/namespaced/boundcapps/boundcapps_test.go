package boundcapps

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func cappWith(name string, reload bool, spec corev1.PodSpec) *cappv1alpha1.Capp {
	capp := &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"}}
	if reload {
		capp.Annotations = map[string]string{consts.BoundResourceRolloutAnnotation: "true"}
	}
	capp.Spec.ConfigurationSpec.Template.Spec.PodSpec = spec
	return capp
}

func containerSpec(c corev1.Container) corev1.PodSpec {
	return corev1.PodSpec{Containers: []corev1.Container{c}}
}

func TestReferences(t *testing.T) {
	secretEnv := corev1.EnvVar{Name: "A", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "target"}, Key: "k"}}}
	cmEnv := corev1.EnvVar{Name: "A", ValueFrom: &corev1.EnvVarSource{
		ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "target"}, Key: "k"}}}
	ref := corev1.LocalObjectReference{Name: "target"}

	tests := []struct {
		name string
		spec corev1.PodSpec
		kind Kind
		want bool
	}{
		{"empty spec", corev1.PodSpec{}, KindSecret, false},
		{"image pull secret", corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{ref}}, KindSecret, true},
		{"image pull secret is not a configmap", corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{ref}}, KindConfigMap, false},
		{"secret env", containerSpec(corev1.Container{Env: []corev1.EnvVar{secretEnv}}), KindSecret, true},
		{"secret env is not a configmap", containerSpec(corev1.Container{Env: []corev1.EnvVar{secretEnv}}), KindConfigMap, false},
		{"configmap env", containerSpec(corev1.Container{Env: []corev1.EnvVar{cmEnv}}), KindConfigMap, true},
		{"secret envFrom", containerSpec(corev1.Container{EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: ref}}}}), KindSecret, true},
		{"configmap envFrom", containerSpec(corev1.Container{EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: ref}}}}), KindConfigMap, true},
		{"init container env", corev1.PodSpec{InitContainers: []corev1.Container{{Env: []corev1.EnvVar{secretEnv}}}}, KindSecret, true},
		{"secret volume", corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "target"}}}}}, KindSecret, true},
		{"configmap volume", corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: ref}}}}}, KindConfigMap, true},
		{"projected secret", corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: ref}}}}}}}}, KindSecret, true},
		{"projected configmap", corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: ref}}}}}}}}, KindConfigMap, true},
		{"other name", corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "other"}}}, KindSecret, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, references(&tt.spec, tt.kind, "target"))
		})
	}
}

func TestFind(t *testing.T) {
	multiRef := corev1.PodSpec{
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "target"}},
		Volumes:          []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "target"}}}},
	}
	other := &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "ns2"}}
	other.Spec.ConfigurationSpec.Template.Spec.PodSpec = multiRef

	k8sClient := testutil.FakeClient(t,
		cappWith("reloads", true, multiRef),
		cappWith("static", false, multiRef),
		cappWith("unrelated", true, corev1.PodSpec{}),
		other,
	)

	resp, err := Find(context.Background(), k8sClient, "ns1", KindSecret, "target")
	require.NoError(t, err)
	// Each Capp appears once even though it references the Secret twice.
	assert.ElementsMatch(t, []string{"reloads", "static"}, resp.BoundCapps)
	assert.Equal(t, []string{"reloads"}, resp.ToReload)
}

func TestFind_NoneReturnsEmptySlices(t *testing.T) {
	resp, err := Find(context.Background(), testutil.FakeClient(t), "ns1", KindSecret, "target")
	require.NoError(t, err)
	assert.NotNil(t, resp.BoundCapps)
	assert.NotNil(t, resp.ToReload)
	assert.Empty(t, resp.BoundCapps)
}

// secretVolumeSpec references the Secret "target" through a volume, so
// pruneVersions keeps its entry.
func secretVolumeSpec() corev1.PodSpec {
	return corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "target"}}}}}
}

func getCapp(t *testing.T, k8sClient client.Client, name string) *cappv1alpha1.Capp {
	t.Helper()
	capp := &cappv1alpha1.Capp{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: name}, capp))
	return capp
}

func versionsOf(t *testing.T, capp *cappv1alpha1.Capp) map[string]string {
	t.Helper()
	versions := map[string]string{}
	require.NoError(t, json.Unmarshal([]byte(capp.Annotations[consts.BoundResourceVersionsAnnotation]), &versions))
	return versions
}

func TestTriggerRollouts(t *testing.T) {
	reloads := cappWith("reloads", true, secretVolumeSpec())
	reloads.Annotations["keep"] = "me"
	k8sClient := testutil.FakeClient(t, reloads, cappWith("static", false, secretVolumeSpec()))

	err := TriggerRollouts(context.Background(), k8sClient, "ns1", []string{"reloads", "static"}, KindSecret, "target", "100")
	require.NoError(t, err)

	got := getCapp(t, k8sClient, "reloads")
	assert.Equal(t, map[string]string{"Secret/target": "100"}, versionsOf(t, got))
	assert.NotEmpty(t, got.Annotations[consts.RestartedAtAnnotation])
	assert.Equal(t, "Secret/target", got.Annotations[consts.RestartReasonAnnotation])
	assert.Equal(t, "me", got.Annotations["keep"])
	assert.Equal(t, "true", got.Annotations[consts.BoundResourceRolloutAnnotation])

	// A Capp that does not opt in is left untouched.
	assert.NotContains(t, getCapp(t, k8sClient, "static").Annotations, consts.BoundResourceVersionsAnnotation)
}

func TestTriggerRollouts_SameVersionIsNoop(t *testing.T) {
	k8sClient := testutil.FakeClient(t, cappWith("reloads", true, secretVolumeSpec()))
	ctx := context.Background()

	require.NoError(t, TriggerRollouts(ctx, k8sClient, "ns1", []string{"reloads"}, KindSecret, "target", "100"))
	before := getCapp(t, k8sClient, "reloads").ResourceVersion

	require.NoError(t, TriggerRollouts(ctx, k8sClient, "ns1", []string{"reloads"}, KindSecret, "target", "100"))
	assert.Equal(t, before, getCapp(t, k8sClient, "reloads").ResourceVersion, "same resourceVersion must not write the Capp again")
}

// Two updates in the same second have different resourceVersions, so each one
// changes the annotation and rolls out a new revision.
func TestTriggerRollouts_NewVersionChangesAnnotation(t *testing.T) {
	k8sClient := testutil.FakeClient(t, cappWith("reloads", true, secretVolumeSpec()))
	ctx := context.Background()

	require.NoError(t, TriggerRollouts(ctx, k8sClient, "ns1", []string{"reloads"}, KindSecret, "target", "100"))
	first := getCapp(t, k8sClient, "reloads").Annotations[consts.BoundResourceVersionsAnnotation]
	require.NoError(t, TriggerRollouts(ctx, k8sClient, "ns1", []string{"reloads"}, KindSecret, "target", "101"))
	second := getCapp(t, k8sClient, "reloads").Annotations[consts.BoundResourceVersionsAnnotation]

	assert.NotEqual(t, first, second)
	assert.Equal(t, map[string]string{"Secret/target": "101"}, versionsOf(t, getCapp(t, k8sClient, "reloads")))
}

func TestTriggerRollouts_KeepsOtherResourcesAndPrunesStale(t *testing.T) {
	spec := secretVolumeSpec()
	spec.Containers = []corev1.Container{{EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}}}}}
	capp := cappWith("reloads", true, spec)
	capp.Annotations[consts.BoundResourceVersionsAnnotation] = `{"ConfigMap/cfg":"7","Secret/removed":"3"}`
	k8sClient := testutil.FakeClient(t, capp)

	require.NoError(t, TriggerRollouts(context.Background(), k8sClient, "ns1", []string{"reloads"}, KindSecret, "target", "100"))
	assert.Equal(t, map[string]string{"ConfigMap/cfg": "7", "Secret/target": "100"}, versionsOf(t, getCapp(t, k8sClient, "reloads")))
}

func TestTriggerRollouts_MalformedAnnotationIsReplaced(t *testing.T) {
	capp := cappWith("reloads", true, secretVolumeSpec())
	capp.Annotations[consts.BoundResourceVersionsAnnotation] = "not json"
	k8sClient := testutil.FakeClient(t, capp)

	require.NoError(t, TriggerRollouts(context.Background(), k8sClient, "ns1", []string{"reloads"}, KindSecret, "target", "100"))
	assert.Equal(t, map[string]string{"Secret/target": "100"}, versionsOf(t, getCapp(t, k8sClient, "reloads")))
}

func TestTriggerRollouts_RetriesOnConflict(t *testing.T) {
	conflicts := 1
	k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if conflicts > 0 {
				conflicts--
				return k8serrors.NewConflict(schema.GroupResource{Group: "rcs.dana.io", Resource: "capps"}, obj.GetName(), errors.New("stale"))
			}
			return c.Update(ctx, obj, opts...)
		},
	}, cappWith("reloads", true, secretVolumeSpec()))

	require.NoError(t, TriggerRollouts(context.Background(), k8sClient, "ns1", []string{"reloads"}, KindSecret, "target", "100"))
	assert.Equal(t, map[string]string{"Secret/target": "100"}, versionsOf(t, getCapp(t, k8sClient, "reloads")))
}

func TestTriggerRollouts_JoinsErrors(t *testing.T) {
	k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return errors.New("boom")
		},
	}, cappWith("a", true, corev1.PodSpec{}), cappWith("b", true, corev1.PodSpec{}))

	err := TriggerRollouts(context.Background(), k8sClient, "ns1", []string{"a", "b", "missing"}, KindConfigMap, "cm", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "capp a: boom")
	assert.Contains(t, err.Error(), "capp b: boom")
	assert.Contains(t, err.Error(), "capp missing:")
}

func TestFind_SpecReferencesAreBoundButNotReloaded(t *testing.T) {
	logCapp := cappWith("logs", true, corev1.PodSpec{})
	logCapp.Spec.LogSpec.PasswordSecret = "target"
	kafkaCapp := cappWith("kafka", true, corev1.PodSpec{})
	kafkaCapp.Spec.EventSourcesSpec.Sources = []cappv1alpha1.SourceConfiguration{
		{Name: "ping", PingSourceConfiguration: &cappv1alpha1.PingSourceConfiguration{Schedule: "* * * * *"}},
		{Name: "k", KafkaSourceConfiguration: &cappv1alpha1.KafkaSourceConfiguration{
			BootstrapServers: []string{"kafka:9092"}, Topics: []string{"t"},
			SecretRef: corev1.LocalObjectReference{Name: "target"},
		}},
	}
	otherKafka := cappWith("other-kafka", true, corev1.PodSpec{})
	otherKafka.Spec.EventSourcesSpec.Sources = []cappv1alpha1.SourceConfiguration{
		{Name: "k", KafkaSourceConfiguration: &cappv1alpha1.KafkaSourceConfiguration{SecretRef: corev1.LocalObjectReference{Name: "other"}}},
	}
	k8sClient := testutil.FakeClient(t, logCapp, kafkaCapp, otherKafka)

	resp, err := Find(context.Background(), k8sClient, "ns1", KindSecret, "target")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"logs", "kafka"}, resp.BoundCapps)
	assert.Empty(t, resp.ToReload)

	// The same names are not ConfigMap references.
	resp, err = Find(context.Background(), k8sClient, "ns1", KindConfigMap, "target")
	require.NoError(t, err)
	assert.Empty(t, resp.BoundCapps)
}
