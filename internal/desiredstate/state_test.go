package desiredstate

import (
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func testCapp() *cappv1alpha1.Capp {
	capp := &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{
		Name: "app", Namespace: "team-a", UID: "uid", ResourceVersion: "7",
		Labels:      map[string]string{"app": "web"},
		Annotations: map[string]string{"note": "keep"},
	}}
	capp.Spec.State = "enabled"
	capp.Spec.ScaleSpec.Metric = "concurrency"
	capp.Spec.ScaleSpec.MaxReplicas = ptr.To[int32](5)
	capp.Spec.ConfigurationSpec.Template.Spec.Containers = []corev1.Container{{
		Image: "web:1",
		Env:   []corev1.EnvVar{{Name: "A", Value: "1"}},
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1000m"), corev1.ResourceMemory: resource.MustParse("1Gi"),
		}},
	}}
	return capp
}

func mustState(t *testing.T, obj any) State {
	t.Helper()
	state, err := StateOf(obj)
	require.NoError(t, err)
	return state
}

func paths(fields []drift.Field) []string {
	result := make([]string, 0, len(fields))
	for _, field := range fields {
		result = append(result, field.Path)
	}
	return result
}

func TestCappOperatorWritesAreNotDrift(t *testing.T) {
	desired := mustState(t, testCapp())
	live := testCapp()
	live.ResourceVersion, live.Generation = "99", 4
	live.Finalizers = []string{"dana.io/cleanup"}
	live.Labels[k8s.LabelBackupToGit] = "true"
	live.Labels["added-by"] = "controller"
	for _, key := range []string{
		"rcs.dana.io/last-updated-by", "rcs.dana.io/skip-dns-check", k8s.AnnotationRollout,
		consts.BoundResourceVersionsAnnotation, consts.RestartedAtAnnotation, consts.RestartReasonAnnotation,
		"kubectl.kubernetes.io/last-applied-configuration", "added-by/controller",
	} {
		live.Annotations[key] = "x"
	}
	live.Status.StateStatus.State = "enabled"
	live.Spec.ConfigurationSpec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("1")
	live.Spec.VolumesSpec.NFSVolumes = []cappv1alpha1.NFSVolume{}

	assert.Empty(t, Compare(KindCapp, desired, mustState(t, live)))
}

func TestCappUserChangesAreDrift(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*cappv1alpha1.Capp)
		want   string
	}{
		"scale":     {func(c *cappv1alpha1.Capp) { c.Spec.ScaleSpec.MaxReplicas = ptr.To[int32](10) }, "/spec/scaleSpec/maxReplicas"},
		"image":     {func(c *cappv1alpha1.Capp) { container(c).Image = "web:2" }, "/spec/configurationSpec/template/spec/containers/0/image"},
		"hidden":    {func(c *cappv1alpha1.Capp) { container(c).Args = []string{"--debug"} }, "/spec/configurationSpec/template/spec/containers/0/args"},
		"env added": {func(c *cappv1alpha1.Capp) { container(c).Env = append(container(c).Env, corev1.EnvVar{Name: "B"}) }, "/spec/configurationSpec/template/spec/containers/0/env"},
		"quantity": {func(c *cappv1alpha1.Capp) {
			container(c).Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
		}, "/spec/configurationSpec/template/spec/containers/0/resources/limits/cpu"},
		"label":      {func(c *cappv1alpha1.Capp) { c.Labels["app"] = "other" }, "/metadata/labels/app"},
		"label gone": {func(c *cappv1alpha1.Capp) { c.Labels = nil }, "/metadata/labels/app"},
		"reload": {func(c *cappv1alpha1.Capp) {
			c.Annotations[consts.BoundResourceRolloutAnnotation] = consts.BoundResourceRolloutEnabled
		}, "/reloadOnBoundResourceChange"},
	} {
		t.Run(name, func(t *testing.T) {
			live := testCapp()
			tc.change(live)
			fields := Compare(KindCapp, mustState(t, testCapp()), mustState(t, live))
			assert.Equal(t, []string{tc.want}, paths(fields))
		})
	}
}

func container(c *cappv1alpha1.Capp) *corev1.Container {
	return &c.Spec.ConfigurationSpec.Template.Spec.Containers[0]
}

func TestCompareReportsBothValues(t *testing.T) {
	live := testCapp()
	live.Spec.State = "disabled"
	fields := Compare(KindCapp, mustState(t, testCapp()), mustState(t, live))
	assert.Equal(t, []drift.Field{{Path: "/spec/state", Desired: "enabled", Live: "disabled"}}, fields)
}

func TestQuantitiesSurviveStorage(t *testing.T) {
	sealer := testSealer(t, "k1")
	ref := Ref{Cluster: "c", Kind: KindCapp, Namespace: "team-a", Name: "app"}
	stored, err := mustState(t, testCapp()).Seal(ref, sealer)
	require.NoError(t, err)
	state, err := Open(&Record{Cluster: "c", Kind: KindCapp, Namespace: "team-a", Name: "app", Desired: stored}, sealer)
	require.NoError(t, err)

	restored := &cappv1alpha1.Capp{}
	require.NoError(t, state.Apply(restored))
	limits := container(restored).Resources.Limits
	assert.True(t, limits.Cpu().Equal(resource.MustParse("1")))
	assert.True(t, limits.Memory().Equal(resource.MustParse("1Gi")))
	assert.Empty(t, Compare(KindCapp, mustState(t, testCapp()), mustState(t, restored)))
}

func TestApplyCappKeepsDroppedFields(t *testing.T) {
	live := testCapp()
	live.Annotations[k8s.AnnotationRollout] = "promote:app-00002"
	live.Annotations[consts.RestartedAtAnnotation] = "2026-01-01T00:00:00Z"
	live.Annotations[consts.BoundResourceRolloutAnnotation] = consts.BoundResourceRolloutEnabled
	live.Labels["added-by"] = "controller"
	live.Labels["app"] = "changed"
	container(live).Args = []string{"--debug"}

	desired := mustState(t, testCapp())
	require.NoError(t, desired.Apply(live))
	assert.Empty(t, Compare(KindCapp, desired, mustState(t, live)))
	assert.Equal(t, "7", live.ResourceVersion)
	assert.Equal(t, "promote:app-00002", live.Annotations[k8s.AnnotationRollout])
	assert.Equal(t, "2026-01-01T00:00:00Z", live.Annotations[consts.RestartedAtAnnotation])
	assert.NotContains(t, live.Annotations, consts.BoundResourceRolloutAnnotation)
	assert.Equal(t, "controller", live.Labels["added-by"])
	assert.Nil(t, container(live).Args)
}

func TestConfigMapDrift(t *testing.T) {
	cm := func(data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{Data: data, BinaryData: map[string][]byte{"bin": {0xff}}}
	}
	desired := mustState(t, cm(map[string]string{"app.yaml": "a: 1", "gone": "x"}))
	live := cm(map[string]string{"app.yaml": "a: 2", "a/b~c": "new"})
	fields := Compare(KindConfigMap, desired, mustState(t, live))
	assert.Equal(t, []drift.Field{
		{Path: "/data/app.yaml", Desired: "a: 1", Live: "a: 2"},
		{Path: "/data/a~1b~0c", Live: "new"},
		{Path: "/data/gone", Desired: "x"},
	}, fields)

	require.NoError(t, desired.Apply(live))
	assert.Empty(t, Compare(KindConfigMap, desired, mustState(t, live)))
}

func TestSecretDriftNeverCarriesValues(t *testing.T) {
	secret := func(password string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"owner": password}},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"password": []byte(password), "tls.crt": []byte("same")},
		}
	}
	desired := mustState(t, secret("first"))
	live := secret("second")
	live.Data["added"] = []byte("third")
	fields := Compare(KindSecret, desired, mustState(t, live))
	assert.Equal(t, []drift.Field{{Path: "/data/added"}, {Path: "/data/password"}, {Path: "/metadata/annotations/owner"}}, fields)

	require.NoError(t, desired.Apply(live))
	assert.Equal(t, secret("first").Data, live.Data)
}

func TestSecretValuesAreNotInTheStoredJSON(t *testing.T) {
	ref := Ref{Cluster: "c", Kind: KindSecret, Namespace: "n", Name: "s"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			"kubectl.kubernetes.io/last-applied-configuration": `{"stringData":{"password":"hunter2"}}`,
		}},
		Data: map[string][]byte{"password": []byte("hunter2")},
	}
	stored, err := mustState(t, secret).Seal(ref, testSealer(t, "k1"))
	require.NoError(t, err)
	assert.NotContains(t, stored.JSON, "hunter2")
	assert.NotContains(t, string(stored.SecretData[0].Ciphertext), "hunter2")
}

func TestNamespaceStateIgnoresOrder(t *testing.T) {
	a := NamespaceState{Users: []string{"bob", "alice", "bob"}, Quota: &QuotaState{}}
	b := NamespaceState{Users: []string{"alice", "bob"}}
	assert.True(t, mustState(t, a).Equal(mustState(t, b)))

	grown := NamespaceState{Users: []string{"alice", "bob", "eve"}, Quota: &QuotaState{CPU: "2", Pods: 5}}
	assert.Equal(t, []string{"/quota", "/users"}, paths(Compare(KindNamespace, mustState(t, b), mustState(t, grown))))

	same := NamespaceState{Quota: &QuotaState{CPU: "2000m"}}
	assert.Empty(t, Compare(KindNamespace, mustState(t, same), mustState(t, NamespaceState{Quota: &QuotaState{CPU: "2"}})))
}

func TestVersionChangesWithState(t *testing.T) {
	sealer := testSealer(t, "k1")
	base := &corev1.Secret{Data: map[string][]byte{"a": []byte("b")}}
	version := mustState(t, base).Version(sealer)
	assert.Equal(t, version, mustState(t, base.DeepCopy()).Version(sealer))
	for _, other := range []map[string][]byte{{"a": []byte("c")}, {"ab": nil}, {}, {"a": []byte("b"), "c": nil}} {
		assert.NotEqual(t, version, mustState(t, &corev1.Secret{Data: other}).Version(sealer))
	}
}

func TestStateOfRejectsUnknownKinds(t *testing.T) {
	_, err := StateOf(&corev1.Pod{})
	require.Error(t, err)
	require.Error(t, State{json: "{}"}.Apply(&corev1.Pod{}))
}

func TestRefID(t *testing.T) {
	assert.Equal(t, "prod/Capp/team-a/app", Ref{Cluster: "prod", Kind: KindCapp, Namespace: "team-a", Name: "app"}.ID())
	assert.Equal(t, "prod/Namespace/team-a/team-a", NamespaceRef("prod", "team-a").ID())
	assert.Equal(t, 3, strings.Count(NamespaceRef("prod", "team-a").ID(), "/"))
}
