package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDesiredState(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, DesiredStateConfig{Mongo: DesiredStateMongoConfig{Database: "capp", TimeoutSeconds: 3}}, cfg.DesiredState)

	t.Setenv("CAPP_DESIREDSTATE_ENABLED", "true")
	t.Setenv("CAPP_DESIREDSTATE_MONGO_URI", "mongodb://mongo:27017")
	t.Setenv("CAPP_DESIREDSTATE_ENCRYPTION_ACTIVEKEYID", "k1")
	t.Setenv("CAPP_DESIREDSTATE_ENCRYPTION_KEYS", "k1=abc,k2=def")
	cfg, err = Load("")
	require.NoError(t, err)
	assert.True(t, cfg.DesiredState.Enabled)
	assert.Equal(t, "mongodb://mongo:27017", cfg.DesiredState.Mongo.URI)
	assert.Equal(t, DesiredStateEncryptionConfig{ActiveKeyID: "k1", Keys: "k1=abc,k2=def"}, cfg.DesiredState.Encryption)
}

func TestValidateDesiredState(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	valid := func() DesiredStateConfig {
		return DesiredStateConfig{
			Enabled:    true,
			Mongo:      DesiredStateMongoConfig{URI: "mongodb+srv://user:hunter2@mongo", Database: "capp", TimeoutSeconds: 3},
			Encryption: DesiredStateEncryptionConfig{ActiveKeyID: "k1", Keys: "k1=" + key},
		}
	}
	for name, tc := range map[string]struct {
		change func(*DesiredStateConfig)
		want   string
	}{
		"valid":             {func(*DesiredStateConfig) {}, ""},
		"disabled":          {func(d *DesiredStateConfig) { *d = DesiredStateConfig{} }, ""},
		"no uri":            {func(d *DesiredStateConfig) { d.Mongo.URI = "" }, "desiredState.mongo.uri"},
		"other scheme":      {func(d *DesiredStateConfig) { d.Mongo.URI = "https://user:hunter2@mongo" }, "desiredState.mongo.uri"},
		"no database":       {func(d *DesiredStateConfig) { d.Mongo.Database = "" }, "desiredState.mongo.database"},
		"no timeout":        {func(d *DesiredStateConfig) { d.Mongo.TimeoutSeconds = 0 }, "desiredState.mongo.timeoutSeconds"},
		"no keys":           {func(d *DesiredStateConfig) { d.Encryption.Keys = "" }, "desiredState.encryption"},
		"short key":         {func(d *DesiredStateConfig) { d.Encryption.Keys = "k1=aHVudGVyMg==" }, "desiredState.encryption.keys"},
		"unknown active id": {func(d *DesiredStateConfig) { d.Encryption.ActiveKeyID = "k2" }, "desiredState.encryption"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			tc.change(&cfg)
			err := errors.Join(validateDesiredState(&cfg)...)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), "hunter2")
			assert.NotContains(t, err.Error(), key)
		})
	}
}
