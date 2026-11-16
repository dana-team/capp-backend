package pools

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullPoolRequest returns a PoolRequest with every field populated.
// When a new field is added to PoolRequest, requireAllFieldsSet will fail
// until this fixture is updated, which forces the developer to also wire the
// new field through ToK8s/FromK8s — otherwise the round-trip assertions break.
func fullPoolRequest() PoolRequest {
	poolSize := 4
	return PoolRequest{
		Name:     "roundtrip-pool",
		Flavor:   "python",
		PoolSize: &poolSize,
		Env: []EnvVar{
			{Name: "PLAIN", Value: "val"},
			{Name: "FROM_SECRET", ValueFrom: &EnvVarSource{SecretKeyRef: &KeySelector{Name: "s1", Key: "k1"}}},
			{Name: "FROM_CM", ValueFrom: &EnvVarSource{ConfigMapKeyRef: &KeySelector{Name: "cm1", Key: "k2"}}},
		},
		Advanced: &Advanced{
			BuildImage:   "other.example/team/builder:2",
			BuildCommand: "./build.sh",
			Resources: &ResourceSpec{
				Requests: &ResourceQuantities{CPU: "300m", Memory: "200Mi"},
				Limits:   &ResourceQuantities{CPU: "600m", Memory: "400Mi"},
			},
		},
	}
}

// TestConvertCoverage is a drift-detection test that ensures every field in
// PoolRequest is handled by both ToK8s and FromK8s.
func TestConvertCoverage(t *testing.T) {
	req := fullPoolRequest()
	catalog := testCatalog(t)

	// Guard: every exported field must be non-zero in the fixture.
	// When a new field is added, this fails until the fixture populates it.
	requireAllFieldsSet(t, req, map[string]bool{
		"Size": true, // replaced by Advanced.Resources; covered by the size subtest
	})
	requireAllFieldsSet(t, *req.Advanced, nil)

	env, err := ToK8s(req, nil, "ns1", catalog)
	require.NoError(t, err)

	resp := FromK8s(env, catalog)

	// Verify ToK8s wrote fields correctly to the Environment object.
	t.Run("ToK8s", func(t *testing.T) {
		assert.Equal(t, "fission.io/v1", env.APIVersion)
		assert.Equal(t, "Environment", env.Kind)
		assert.Equal(t, req.Name, env.Name, "Name not written by ToK8s")
		assert.Equal(t, "ns1", env.Namespace)
		assert.Equal(t, pyImage, env.Spec.Runtime.Image, "Flavor not written by ToK8s")
		assert.Equal(t, *req.PoolSize, env.Spec.Poolsize, "PoolSize not written by ToK8s")
		assert.Equal(t, req.Advanced.BuildImage, env.Spec.Builder.Image, "Advanced.BuildImage not written by ToK8s")
		assert.Equal(t, req.Advanced.BuildCommand, env.Spec.Builder.Command, "Advanced.BuildCommand not written by ToK8s")
		assertResources(t, resourcesOf("300m", "200Mi", "600m", "400Mi"), env.Spec.Resources)
		require.NotNil(t, env.Spec.Runtime.Container, "Env not written by ToK8s")
		assert.Len(t, env.Spec.Runtime.Container.Env, 3, "Env not written by ToK8s")
	})

	// Verify FromK8s reads fields correctly from the Environment object.
	t.Run("FromK8s", func(t *testing.T) {
		assert.Equal(t, req.Name, resp.Name, "Name not read by FromK8s")
		assert.Equal(t, req.Flavor, resp.Flavor, "Flavor not read by FromK8s")
		assert.Equal(t, *req.PoolSize, resp.PoolSize, "PoolSize not read by FromK8s")
		require.NotNil(t, resp.Builder, "Builder not read by FromK8s")
		assert.Equal(t, req.Advanced.BuildImage, resp.Builder.Image, "Advanced.BuildImage not read by FromK8s")
		assert.Equal(t, req.Advanced.BuildCommand, resp.Builder.Command, "Advanced.BuildCommand not read by FromK8s")
		assert.Equal(t, req.Advanced.Resources, resp.Resources, "Advanced.Resources not read by FromK8s")
		assert.Empty(t, resp.Size, "custom resources match no size")
		assert.Equal(t, req.Env, resp.Env, "Env not read by FromK8s")
		assert.Equal(t, req.Advanced, resp.Advanced, "Advanced not read by FromK8s, so a response sent back would lose it")
	})

	// Size is skipped above because Advanced.Resources replaces it.
	t.Run("Size", func(t *testing.T) {
		req := PoolRequest{Name: "sized", Flavor: "python", Size: "large"}
		env, err := ToK8s(req, nil, "ns1", catalog)
		require.NoError(t, err)
		assertResources(t, resourcesOf("500m", "512Mi", "1", "1Gi"), env.Spec.Resources)
		assert.Equal(t, req.Size, FromK8s(env, catalog).Size, "Size not read by FromK8s")
	})
}

// requireAllFieldsSet uses reflection to verify that every exported field in
// the struct is non-zero. Fields listed in skip are excluded from the check.
func requireAllFieldsSet(t *testing.T, s any, skip map[string]bool) {
	t.Helper()
	v := reflect.ValueOf(s)
	typ := v.Type()
	for i := range v.NumField() {
		f := typ.Field(i)
		if !f.IsExported() || skip[f.Name] {
			continue
		}
		fv := v.Field(i)
		require.Falsef(t, fv.IsZero(),
			"fullPoolRequest() must populate field %q — add it to the fixture "+
				"and wire it through ToK8s/FromK8s", f.Name)
	}
}
