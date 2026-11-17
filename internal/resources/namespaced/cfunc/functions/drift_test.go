package functions

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullFunctionRequest returns a FunctionRequest with every field populated.
// When a new field is added to FunctionRequest, requireAllFieldsSet will fail
// until this fixture is updated, which forces the developer to also wire the
// new field through ToK8s/FromK8s — otherwise the round-trip assertions break.
func fullFunctionRequest() FunctionRequest {
	return FunctionRequest{
		Name:           "shop",
		Pool:           "py",
		Source:         FunctionSource{URL: "https://git.example/team/functions.git", Ref: "main", Path: "shop", Build: new(true)},
		Entrypoint:     "main.handle",
		TimeoutSeconds: new(30),
		SecretVolumes: []SecretVolume{
			{Name: "shop-db", SecretName: "shop-db", MountPath: "/secrets/ns1/shop-db"},
		},
		ConfigMapVolumes: []ConfigMapVolume{
			{Name: "shop-cfg", ConfigMapName: "shop-cfg", MountPath: "/configs/ns1/shop-cfg"},
		},
		Triggers: []FunctionTrigger{
			{Name: "api", Type: TriggerTypeHTTP, Path: "/orders", Methods: []string{"GET", "POST"}},
			{Name: "nightly", Type: TriggerTypeCron, Schedule: "0 2 * * *"},
		},
		Advanced: &FunctionAdvanced{
			Concurrency:                  new(8),
			RequestsPerPod:               new(4),
			IdleTimeoutSeconds:           new(300),
			OnceOnly:                     true,
			RetainPods:                   new(2),
			SpecializationTimeoutSeconds: new(180),
			BuildCommand:                 "./build.sh",
		},
	}
}

// TestConvertCoverage is a drift-detection test that ensures every field in
// FunctionRequest is handled by both ToK8s and FromK8s.
func TestConvertCoverage(t *testing.T) {
	req := fullFunctionRequest()
	settings := testSettings()
	code := testCode()

	requireAllFieldsSet(t, req, nil)
	requireAllFieldsSet(t, *req.Advanced, nil)
	requireAllFieldsSet(t, req.Source, map[string]bool{"Commit": true})

	objects, err := ToK8s(req, nil, "ns1", builderPool(), code, settings)
	require.NoError(t, err)

	resp := FromK8s(objects, "https://fn.example", settings)

	t.Run("ToK8s", func(t *testing.T) {
		fn := objects.Function
		assert.Equal(t, "fission.io/v1", fn.APIVersion)
		assert.Equal(t, "Function", fn.Kind)
		assert.Equal(t, req.Name, fn.Name, "Name not written by ToK8s")
		assert.Equal(t, "ns1", fn.Namespace)
		assert.Equal(t, req.Pool, fn.Spec.Environment.Name, "Pool not written by ToK8s")
		assert.Equal(t, "ns1", fn.Spec.Environment.Namespace)
		assert.Equal(t, req.Entrypoint, fn.Spec.Package.FunctionName, "Entrypoint not written by ToK8s")
		assert.Equal(t, 30, fn.Spec.FunctionTimeout, "TimeoutSeconds not written by ToK8s")
		require.Len(t, fn.Spec.Secrets, 1, "SecretVolumes not written by ToK8s")
		assert.Equal(t, "shop-db", fn.Spec.Secrets[0].Name)
		assert.Equal(t, "ns1", fn.Spec.Secrets[0].Namespace)
		require.Len(t, fn.Spec.ConfigMaps, 1, "ConfigMapVolumes not written by ToK8s")
		assert.Equal(t, "shop-cfg", fn.Spec.ConfigMaps[0].Name)
		assert.Equal(t, 8, fn.Spec.Concurrency, "Advanced.Concurrency not written by ToK8s")
		assert.Equal(t, 4, fn.Spec.RequestsPerPod, "Advanced.RequestsPerPod not written by ToK8s")
		require.NotNil(t, fn.Spec.IdleTimeout)
		assert.Equal(t, 300, *fn.Spec.IdleTimeout, "Advanced.IdleTimeoutSeconds not written by ToK8s")
		assert.True(t, fn.Spec.OnceOnly, "Advanced.OnceOnly not written by ToK8s")
		assert.Equal(t, 2, fn.Spec.RetainPods, "Advanced.RetainPods not written by ToK8s")
		assert.Equal(t, 180, fn.Spec.InvokeStrategy.ExecutionStrategy.SpecializationTimeout)

		pkg := objects.Package
		assert.Equal(t, req.Name, pkg.Name)
		assert.Equal(t, code.Zip, pkg.Spec.Source.Literal, "Source not written by ToK8s")
		assert.Equal(t, "./build.sh", pkg.Spec.BuildCommand, "Advanced.BuildCommand not written by ToK8s")
		assert.Equal(t, req.Source.URL, pkg.Annotations[SourceURLAnnotation])

		require.Len(t, objects.HTTPTriggers, 1, "Triggers not written by ToK8s")
		assert.Equal(t, "shop-api", objects.HTTPTriggers[0].Name)
		require.Len(t, objects.TimeTriggers, 1, "Triggers not written by ToK8s")
		assert.Equal(t, "shop-nightly", objects.TimeTriggers[0].Name)
	})

	t.Run("FromK8s", func(t *testing.T) {
		assert.Equal(t, req.Name, resp.Name, "Name not read by FromK8s")
		assert.Equal(t, req.Pool, resp.Pool, "Pool not read by FromK8s")
		assert.Equal(t, req.Entrypoint, resp.Entrypoint, "Entrypoint not read by FromK8s")
		assert.Equal(t, *req.TimeoutSeconds, resp.TimeoutSeconds, "TimeoutSeconds not read by FromK8s")
		assert.Equal(t, req.SecretVolumes, resp.SecretVolumes, "SecretVolumes not read by FromK8s")
		assert.Equal(t, req.ConfigMapVolumes, resp.ConfigMapVolumes, "ConfigMapVolumes not read by FromK8s")
		assert.Equal(t, req.Advanced, resp.Advanced, "Advanced not read by FromK8s, so a response sent back would lose it")

		wantSource := req.Source
		wantSource.Commit = code.Commit
		assert.Equal(t, wantSource, resp.Source, "Source not read by FromK8s")

		require.Len(t, resp.Triggers, 2, "Triggers not read by FromK8s")
		assert.Equal(t, FunctionTrigger{
			Name: "api", Type: TriggerTypeHTTP, Path: "/orders", Methods: []string{"GET", "POST"},
			FullPath: "/ns1/shop/orders", URL: "https://fn.example/ns1/shop/orders",
		}, resp.Triggers[0])
		assert.Equal(t, req.Triggers[1], resp.Triggers[1])
	})

	// A client edits what GET returned and sends it back. Every request
	// field must survive that, and the response-only fields must be accepted.
	t.Run("a response sent back gives the same objects", func(t *testing.T) {
		raw, err := json.Marshal(resp)
		require.NoError(t, err)
		var again FunctionRequest
		require.NoError(t, json.Unmarshal(raw, &again))

		second, err := ToK8s(again, nil, "ns1", builderPool(), code, settings)
		require.NoError(t, err)

		assert.Equal(t, objects.Function.Spec, second.Function.Spec)
		assert.Equal(t, objects.Package.Spec, second.Package.Spec)
		assert.Equal(t, objects.HTTPTriggers[0].Spec, second.HTTPTriggers[0].Spec)
		assert.Equal(t, objects.TimeTriggers[0].Spec, second.TimeTriggers[0].Spec)
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
			"fullFunctionRequest() must populate field %q — add it to the fixture "+
				"and wire it through ToK8s/FromK8s", f.Name)
	}
}
