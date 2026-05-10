package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Context holds all per-server connection settings for one named environment.
type Context struct {
	Name           string    `yaml:"name"`
	Server         string    `yaml:"server"`
	AuthMode       string    `yaml:"auth-mode"`
	Token          string    `yaml:"token,omitempty"`
	RefreshToken   string    `yaml:"refresh-token,omitempty"`
	TokenExpiresAt time.Time `yaml:"token-expires-at,omitempty"`
	Cluster        string    `yaml:"cluster,omitempty"`
	Namespace      string    `yaml:"namespace,omitempty"`
}

// Config is the top-level config file structure.
type Config struct {
	CurrentContext string    `yaml:"current-context"`
	Contexts       []Context `yaml:"contexts"`
}

// DefaultPath returns the XDG-style default config file path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "cappctl", "config.yaml")
}

// Load reads and parses the config file at path.
// Returns an empty Config (no error) if the file does not exist.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &cfg, nil
}

// Save atomically writes cfg to path with permissions 0600.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("serializing config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".cappctl-config-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { os.Remove(tmpName) }() //nolint:errcheck
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("setting config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("writing temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("persisting config: %w", err)
	}
	return nil
}

// GetContext returns a pointer to the named context, or (nil, false) if absent.
func (c *Config) GetContext(name string) (*Context, bool) {
	for i := range c.Contexts {
		if c.Contexts[i].Name == name {
			return &c.Contexts[i], true
		}
	}
	return nil, false
}

// UpsertContext adds or replaces the context with the same name.
func (c *Config) UpsertContext(ctx Context) {
	for i := range c.Contexts {
		if c.Contexts[i].Name == ctx.Name {
			c.Contexts[i] = ctx
			return
		}
	}
	c.Contexts = append(c.Contexts, ctx)
}

// DeleteContext removes the named context and clears CurrentContext if it matched.
// Returns true if the context existed.
func (c *Config) DeleteContext(name string) bool {
	for i, ctx := range c.Contexts {
		if ctx.Name == name {
			c.Contexts = append(c.Contexts[:i], c.Contexts[i+1:]...)
			if c.CurrentContext == name {
				c.CurrentContext = ""
			}
			return true
		}
	}
	return false
}

// ActiveContext returns the context named by CurrentContext.
// Returns an error if no current context is set or the named context is missing.
func (c *Config) ActiveContext() (*Context, error) {
	if c.CurrentContext == "" {
		return nil, fmt.Errorf("no current context set; run 'cappctl context use <name>'")
	}
	ctx, ok := c.GetContext(c.CurrentContext)
	if !ok {
		return nil, fmt.Errorf("current context %q not found in config", c.CurrentContext)
	}
	return ctx, nil
}
