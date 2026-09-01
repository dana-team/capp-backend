package capps

import (
	"reflect"
	"testing"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullCappRequest returns a CappRequest with every field populated.
// When a new field is added to CappRequest, requireAllFieldsSet will fail
// until this fixture is updated, which forces the developer to also wire the
// new field through ToK8s/FromK8s — otherwise the round-trip assertions break.
func fullCappRequest() CappRequest {
	timeout := int64(30)
	min, max, delay := int32(1), int32(5), int32(60)
	consumers := int32(2)
	return CappRequest{
		Name:          "roundtrip-app",
		State:         "enabled",
		Image:         "nginx:1.25",
		ContainerName: "web",
		CustomResources: &ResourceSpec{
			Requests: &ResourceQuantities{CPU: "100m", Memory: "128Mi"},
			Limits:   &ResourceQuantities{CPU: "200m", Memory: "256Mi"},
		},
		ScaleSpec: ScaleSpec{
			Metric:            "concurrency",
			MinReplicas:       &min,
			MaxReplicas:       &max,
			ScaleDelaySeconds: &delay,
		},
		Env: []EnvVar{
			{Name: "PLAIN", Value: "val"},
			{Name: "FROM_SECRET", ValueFrom: &EnvVarSource{SecretKeyRef: &KeySelector{Name: "s1", Key: "k1"}}},
			{Name: "FROM_CM", ValueFrom: &EnvVarSource{ConfigMapKeyRef: &KeySelector{Name: "cm1", Key: "k2"}}},
		},
		VolumeMounts:     []VolumeMount{{Name: "data", MountPath: "/data"}},
		RouteSpec:        &RouteSpec{Hostname: "app.example.com", TLSEnabled: true, RouteTimeoutSeconds: &timeout},
		LogSpec:          &LogSpec{Type: "elastic", Host: "es:9200", Index: "logs", User: "admin", PasswordSecret: "pw", PasswordKey: "password"},
		NFSVolumes:       []NFSVolume{{Name: "nfs1", Server: "nfs.local", Path: "/export", Capacity: "10Gi"}},
		SecretVolumes:    []SecretVolume{{Name: "sec-vol", SecretName: "my-secret", MountPath: "/secrets"}},
		ConfigMapVolumes: []ConfigMapVolume{{Name: "cm-vol", ConfigMapName: "my-cm", MountPath: "/config"}},
		EventSourcesSpec: &EventSourcesSpec{Sources: []SourceConfig{
			{
				Name:             "ping1",
				URI:              "/events",
				PingSourceConfig: &PingSourceConfig{Schedule: "*/5 * * * *", Data: `{"msg":"hi"}`},
			},
			{
				Name: "kafka1",
				KafkaSourceConfig: &KafkaSourceConfig{
					BootstrapServers: []string{"kafka:9092"},
					Topics:           []string{"events"},
					ConsumerGroup:    "cg1",
					Consumers:        &consumers,
					SecretRef:        "kafka-creds",
				},
			},
		}},
	}
}

// TestConvertCoverage verifies that all CappRequest fields are correctly
// written by ToK8s and read back by FromK8s. It is self-maintaining: adding
// a field to CappRequest without updating this test causes an assertion failure.
func TestConvertCoverage(t *testing.T) {
	req := fullCappRequest()

	// Guard: every exported CappRequest field must be non-zero in the fixture.
	// When a new field is added, this fails until the fixture populates it.
	requireAllFieldsSet(t, req, map[string]bool{
		"Size": true, // mutually exclusive with CustomResources
	})

	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)

	resp := FromK8s(capp, minimalSizes())

	// Phase 1: verify ToK8s wrote fields correctly to the Capp object.
	t.Run("ToK8s", func(t *testing.T) {
		assert.Equal(t, req.Name, capp.Name, "Name not written by ToK8s")
		assert.Equal(t, req.State, capp.Spec.State, "State not written by ToK8s")
		c := capp.Spec.ConfigurationSpec.Template.Spec.Containers[0]
		assert.Equal(t, req.Image, c.Image, "Image not written by ToK8s")
		assert.Equal(t, req.ContainerName, c.Name, "ContainerName not written by ToK8s")
		assert.Equal(t, req.ScaleSpec.Metric, capp.Spec.ScaleSpec.Metric, "ScaleSpec.Metric not written by ToK8s")
		assert.Equal(t, req.ScaleSpec.MinReplicas, capp.Spec.ScaleSpec.MinReplicas, "ScaleSpec.MinReplicas not written by ToK8s")
		assert.Equal(t, req.ScaleSpec.MaxReplicas, capp.Spec.ScaleSpec.MaxReplicas, "ScaleSpec.MaxReplicas not written by ToK8s")
		assert.Equal(t, req.ScaleSpec.ScaleDelaySeconds, capp.Spec.ScaleSpec.ScaleDelaySeconds, "ScaleSpec.ScaleDelaySeconds not written by ToK8s")
		assert.Equal(t, req.RouteSpec.Hostname, capp.Spec.RouteSpec.Hostname, "RouteSpec.Hostname not written by ToK8s")
		assert.Equal(t, req.RouteSpec.TLSEnabled, capp.Spec.RouteSpec.TlsEnabled, "RouteSpec.TLSEnabled not written by ToK8s")
		assert.Equal(t, req.RouteSpec.RouteTimeoutSeconds, capp.Spec.RouteSpec.RouteTimeoutSeconds, "RouteSpec.RouteTimeoutSeconds not written by ToK8s")
		assert.Equal(t, req.LogSpec.Type, string(capp.Spec.LogSpec.Type), "LogSpec.Type not written by ToK8s")
		assert.Equal(t, req.LogSpec.Host, capp.Spec.LogSpec.Host, "LogSpec.Host not written by ToK8s")
		assert.Equal(t, req.LogSpec.Index, capp.Spec.LogSpec.Index, "LogSpec.Index not written by ToK8s")
		assert.Equal(t, req.LogSpec.User, capp.Spec.LogSpec.User, "LogSpec.User not written by ToK8s")
		assert.Equal(t, req.LogSpec.PasswordSecret, capp.Spec.LogSpec.PasswordSecret, "LogSpec.PasswordSecret not written by ToK8s")
		assert.Equal(t, req.LogSpec.PasswordKey, capp.Spec.LogSpec.PasswordKey, "LogSpec.PasswordKey not written by ToK8s")
		assert.Len(t, c.Env, 3, "Env not written by ToK8s")
		expectedMounts := len(req.VolumeMounts) + len(req.SecretVolumes) + len(req.ConfigMapVolumes)
		assert.Len(t, c.VolumeMounts, expectedMounts, "VolumeMounts not written by ToK8s")
		assert.NotEmpty(t, c.Resources.Requests, "CustomResources.Requests not written by ToK8s")
		assert.NotEmpty(t, c.Resources.Limits, "CustomResources.Limits not written by ToK8s")
		assert.Len(t, capp.Spec.VolumesSpec.NFSVolumes, 1, "NFSVolumes not written by ToK8s")
		assert.Equal(t, req.NFSVolumes[0].Name, capp.Spec.VolumesSpec.NFSVolumes[0].Name, "NFSVolumes.Name not written by ToK8s")
		assert.Equal(t, req.NFSVolumes[0].Server, capp.Spec.VolumesSpec.NFSVolumes[0].Server, "NFSVolumes.Server not written by ToK8s")
		assert.Equal(t, req.NFSVolumes[0].Path, capp.Spec.VolumesSpec.NFSVolumes[0].Path, "NFSVolumes.Path not written by ToK8s")
		assert.Len(t, capp.Spec.ConfigurationSpec.Template.Spec.Volumes, len(req.SecretVolumes)+len(req.ConfigMapVolumes), "SecretVolumes/ConfigMapVolumes not written by ToK8s")
		assert.Len(t, capp.Spec.EventSourcesSpec.Sources, 2, "EventSourcesSpec.Sources not written by ToK8s")
	})

	// Phase 2: verify FromK8s reads fields correctly from the Capp object.
	t.Run("FromK8s", func(t *testing.T) {
		assert.Equal(t, req.Name, resp.Name, "Name not read by FromK8s")
		assert.Equal(t, req.State, resp.State, "State not read by FromK8s")
		assert.Equal(t, req.Image, resp.Image, "Image not read by FromK8s")
		assert.Equal(t, req.ContainerName, resp.ContainerName, "ContainerName not read by FromK8s")
		assert.Equal(t, req.ScaleSpec, resp.ScaleSpec, "ScaleSpec not read by FromK8s")
		assert.Equal(t, req.RouteSpec.Hostname, resp.RouteSpec.Hostname, "RouteSpec.Hostname not read by FromK8s")
		assert.Equal(t, req.RouteSpec.TLSEnabled, resp.RouteSpec.TLSEnabled, "RouteSpec.TLSEnabled not read by FromK8s")
		assert.Equal(t, req.RouteSpec.RouteTimeoutSeconds, resp.RouteSpec.RouteTimeoutSeconds, "RouteSpec.RouteTimeoutSeconds not read by FromK8s")
		assert.Equal(t, req.LogSpec.Type, resp.LogSpec.Type, "LogSpec.Type not read by FromK8s")
		assert.Equal(t, req.LogSpec.Host, resp.LogSpec.Host, "LogSpec.Host not read by FromK8s")
		assert.Equal(t, req.LogSpec.Index, resp.LogSpec.Index, "LogSpec.Index not read by FromK8s")
		assert.Equal(t, req.LogSpec.User, resp.LogSpec.User, "LogSpec.User not read by FromK8s")
		assert.Equal(t, req.LogSpec.PasswordSecret, resp.LogSpec.PasswordSecret, "LogSpec.PasswordSecret not read by FromK8s")
		assert.Equal(t, req.LogSpec.PasswordKey, resp.LogSpec.PasswordKey, "LogSpec.PasswordKey not read by FromK8s")
		require.NotNil(t, resp.Resources, "CustomResources not read by FromK8s")
		assert.Equal(t, "100m", resp.Resources.Requests.CPU, "Resources.Requests.CPU not read by FromK8s")
		assert.Equal(t, "128Mi", resp.Resources.Requests.Memory, "Resources.Requests.Memory not read by FromK8s")
		assert.Equal(t, "200m", resp.Resources.Limits.CPU, "Resources.Limits.CPU not read by FromK8s")
		assert.Equal(t, "256Mi", resp.Resources.Limits.Memory, "Resources.Limits.Memory not read by FromK8s")
		assert.Len(t, resp.Env, 3, "Env not read by FromK8s")
		expectedMounts := len(req.VolumeMounts) + len(req.SecretVolumes) + len(req.ConfigMapVolumes)
		assert.Len(t, resp.VolumeMounts, expectedMounts, "VolumeMounts not read by FromK8s")
		assert.Len(t, resp.NFSVolumes, 1, "NFSVolumes not read by FromK8s")
		assert.Equal(t, req.NFSVolumes[0].Name, resp.NFSVolumes[0].Name, "NFSVolumes.Name not read by FromK8s")
		assert.Equal(t, req.NFSVolumes[0].Server, resp.NFSVolumes[0].Server, "NFSVolumes.Server not read by FromK8s")
		assert.Equal(t, req.NFSVolumes[0].Path, resp.NFSVolumes[0].Path, "NFSVolumes.Path not read by FromK8s")
		assert.Equal(t, req.NFSVolumes[0].Capacity, resp.NFSVolumes[0].Capacity, "NFSVolumes.Capacity not read by FromK8s")
		assert.Len(t, resp.SecretVolumes, 1, "SecretVolumes not read by FromK8s")
		assert.Equal(t, req.SecretVolumes[0].SecretName, resp.SecretVolumes[0].SecretName, "SecretVolumes.SecretName not read by FromK8s")
		assert.Len(t, resp.ConfigMapVolumes, 1, "ConfigMapVolumes not read by FromK8s")
		assert.Equal(t, req.ConfigMapVolumes[0].ConfigMapName, resp.ConfigMapVolumes[0].ConfigMapName, "ConfigMapVolumes.ConfigMapName not read by FromK8s")
		require.NotNil(t, resp.EventSourcesSpec, "EventSourcesSpec not read by FromK8s")
		assert.Len(t, resp.EventSourcesSpec.Sources, 2, "EventSourcesSpec.Sources not read by FromK8s")
	})
}

// TestCappSpecFieldsHandled fails when a new field is added to any
// operator-owned CRD type that isn't accounted for in the conversion layer.
func TestCappSpecFieldsHandled(t *testing.T) {
	cases := []struct {
		name    string
		typ     reflect.Type
		handled map[string]string
	}{
		{
			name: "CappSpec",
			typ:  reflect.TypeOf(cappv1alpha1.CappSpec{}),
			handled: map[string]string{
				"State":             "",
				"ScaleSpec":         "",
				"ConfigurationSpec": "container fields flattened into top-level DTO fields",
				"RouteSpec":         "",
				"LogSpec":           "",
				"VolumesSpec":       "",
				"EventSourcesSpec":  "",
			},
		},
		{
			name: "ScaleSpec",
			typ:  reflect.TypeOf(cappv1alpha1.ScaleSpec{}),
			handled: map[string]string{
				"Metric": "", "MinReplicas": "", "MaxReplicas": "", "ScaleDelaySeconds": "",
			},
		},
		{
			name: "RouteSpec",
			typ:  reflect.TypeOf(cappv1alpha1.RouteSpec{}),
			handled: map[string]string{
				"Hostname":            "",
				"TlsEnabled":          "",
				"TrafficTarget":       "internal Knative routing, not exposed in API",
				"RouteTimeoutSeconds": "",
			},
		},
		{
			name: "LogSpec",
			typ:  reflect.TypeOf(cappv1alpha1.LogSpec{}),
			handled: map[string]string{
				"Type": "", "Host": "", "Index": "", "User": "", "PasswordSecret": "", "PasswordKey": "",
			},
		},
		{
			name: "VolumesSpec",
			typ:  reflect.TypeOf(cappv1alpha1.VolumesSpec{}),
			handled: map[string]string{
				"NFSVolumes": "",
			},
		},
		{
			name: "NFSVolume",
			typ:  reflect.TypeOf(cappv1alpha1.NFSVolume{}),
			handled: map[string]string{
				"Name": "", "Server": "", "Path": "", "Capacity": "",
			},
		},
		{
			name: "EventSourcesSpec",
			typ:  reflect.TypeOf(cappv1alpha1.EventSourcesSpec{}),
			handled: map[string]string{
				"Sources": "",
			},
		},
		{
			name: "SourceConfiguration",
			typ:  reflect.TypeOf(cappv1alpha1.SourceConfiguration{}),
			handled: map[string]string{
				"Name": "", "URI": "", "PingSourceConfiguration": "", "KafkaSourceConfiguration": "",
			},
		},
		{
			name: "PingSourceConfiguration",
			typ:  reflect.TypeOf(cappv1alpha1.PingSourceConfiguration{}),
			handled: map[string]string{
				"Schedule": "", "Data": "",
			},
		},
		{
			name: "KafkaSourceConfiguration",
			typ:  reflect.TypeOf(cappv1alpha1.KafkaSourceConfiguration{}),
			handled: map[string]string{
				"BootstrapServers": "", "Topics": "", "ConsumerGroup": "", "Consumers": "",
				"SecretRef": "mapped as flat string, not LocalObjectReference",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.typ.NumField() {
				f := tc.typ.Field(i)
				if !f.IsExported() {
					continue
				}
				_, ok := tc.handled[f.Name]
				require.Truef(t, ok,
					"%s has new field %q — add handling in convert.go (ToK8s/FromK8s) "+
						"and register it in this test's handled map", tc.name, f.Name)
			}
		})
	}
}

// requireAllFieldsSet checks that every exported field of a struct is non-zero,
// except those in the skip set. This ensures the fixture stays complete when
// new fields are added to the type.
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
			"fullCappRequest() must populate field %q — add it to the fixture "+
				"and wire it through ToK8s/FromK8s", f.Name)
	}
}
