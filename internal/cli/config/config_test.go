package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dana-team/capp-backend/internal/cli/config"
)

func TestLoadMissingFile(t *testing.T) {
	cfg, err := config.Load("/nonexistent/path/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, "", cfg.CurrentContext)
	assert.Empty(t, cfg.Contexts)
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	original := &config.Config{
		CurrentContext: "staging",
		Contexts: []config.Context{
			{
				Name:           "staging",
				Server:         "https://capp.example.com",
				AuthMode:       "jwt",
				Token:          "mytoken",
				RefreshToken:   "myrefresh",
				TokenExpiresAt: time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC),
				Cluster:        "staging",
				Namespace:      "default",
			},
		},
	}

	require.NoError(t, config.Save(path, original))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

	loaded, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, original.CurrentContext, loaded.CurrentContext)
	require.Len(t, loaded.Contexts, 1)
	assert.Equal(t, original.Contexts[0].Name, loaded.Contexts[0].Name)
	assert.Equal(t, original.Contexts[0].Server, loaded.Contexts[0].Server)
	assert.Equal(t, original.Contexts[0].Token, loaded.Contexts[0].Token)
}

func TestUpsertContext(t *testing.T) {
	cfg := &config.Config{}

	cfg.UpsertContext(config.Context{Name: "a", Server: "https://a.example.com"})
	cfg.UpsertContext(config.Context{Name: "b", Server: "https://b.example.com"})
	assert.Len(t, cfg.Contexts, 2)

	cfg.UpsertContext(config.Context{Name: "a", Server: "https://a-updated.example.com"})
	assert.Len(t, cfg.Contexts, 2)
	ctx, ok := cfg.GetContext("a")
	require.True(t, ok)
	assert.Equal(t, "https://a-updated.example.com", ctx.Server)
}

func TestDeleteContext(t *testing.T) {
	cfg := &config.Config{
		CurrentContext: "a",
		Contexts: []config.Context{
			{Name: "a"},
			{Name: "b"},
		},
	}

	assert.True(t, cfg.DeleteContext("a"))
	assert.Len(t, cfg.Contexts, 1)
	assert.Equal(t, "", cfg.CurrentContext)
	assert.False(t, cfg.DeleteContext("nonexistent"))
}

func TestActiveContext(t *testing.T) {
	cfg := &config.Config{}

	_, err := cfg.ActiveContext()
	assert.ErrorContains(t, err, "no current context")

	cfg.CurrentContext = "missing"
	_, err = cfg.ActiveContext()
	assert.ErrorContains(t, err, "not found")

	cfg.UpsertContext(config.Context{Name: "missing", Server: "https://x.example.com"})
	ctx, err := cfg.ActiveContext()
	require.NoError(t, err)
	assert.Equal(t, "missing", ctx.Name)
}
