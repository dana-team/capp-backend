// Package config loads, parses, and exposes the capp-backend configuration.
//
// Configuration is sourced from three places in priority order (highest first):
//  1. Environment variables prefixed with CAPP_ (e.g. CAPP_SERVER_PORT)
//  2. A YAML config file whose path is passed to Load
//  3. Hard-coded default values embedded in this package
//
// All nested keys are flattened with underscores for env var mapping, e.g.
// auth.jwt.secretKey → CAPP_AUTH_JWT_SECRETKEY.
package config

import (
	"strings"

	"github.com/spf13/viper"
)

// ServerConfig holds HTTP server networking settings.
type ServerConfig struct {
	// Port is the TCP port the HTTP server listens on. Default: 8080.
	Port int `mapstructure:"port"`

	// ReadTimeoutSeconds is the maximum duration for reading the full request,
	// including the body. Default: 30.
	ReadTimeoutSeconds int `mapstructure:"readTimeoutSeconds"`

	// WriteTimeoutSeconds is the maximum duration before timing out writes of
	// the response. Default: 30.
	WriteTimeoutSeconds int `mapstructure:"writeTimeoutSeconds"`

	// IdleTimeoutSeconds is the maximum amount of time to wait for the next
	// request when keep-alives are enabled. Default: 60.
	IdleTimeoutSeconds int `mapstructure:"idleTimeoutSeconds"`

	// CORSAllowedOrigins is the list of origins the server will accept
	// cross-origin requests from. Use ["*"] to allow all origins (not
	// recommended in production).
	CORSAllowedOrigins []string `mapstructure:"corsAllowedOrigins"`
}

// JWTConfig holds settings for the JWT auth mode.
type JWTConfig struct {
	// SecretKey is the HMAC signing secret. Must be kept private.
	// Required when auth.mode == "jwt". Inject via CAPP_AUTH_JWT_SECRETKEY.
	SecretKey string `mapstructure:"secretKey"`

	// TokenTTLMinutes is the lifetime of an access JWT in minutes. Default: 60.
	TokenTTLMinutes int `mapstructure:"tokenTTLMinutes"`

	// RefreshTTLMinutes is the lifetime of a refresh JWT in minutes. Default: 1440 (24 h).
	RefreshTTLMinutes int `mapstructure:"refreshTTLMinutes"`
}

// StaticConfig holds settings for the static API-key auth mode.
// This mode is intended for development and integration testing only.
type StaticConfig struct {
	// APIKeys is the list of accepted bearer tokens.
	APIKeys []string `mapstructure:"apiKeys"`
}

// RateLimitConfig controls per-IP request rate limiting.
type RateLimitConfig struct {
	// Enabled toggles rate limiting globally. Default: true.
	Enabled bool `mapstructure:"enabled"`

	// RequestsPerSecond is the sustained token-bucket refill rate. Default: 20.
	RequestsPerSecond float64 `mapstructure:"requestsPerSecond"`

	// Burst is the maximum number of requests allowed in a burst above the
	// sustained rate. Default: 40.
	Burst int `mapstructure:"burst"`
}

// AuthConfig holds all authentication-related settings.
type AuthConfig struct {
	// Mode selects the authentication strategy. One of:
	//   passthrough — the client's Kubernetes bearer token is forwarded directly
	//                 to the cluster; no session is created server-side.
	//   jwt         — a login endpoint issues short-lived JWTs backed by a
	//                 server-side session store.
	//   static      — a fixed list of API keys (development only).
	// Default: "passthrough".
	Mode      string          `mapstructure:"mode"`
	JWT       JWTConfig       `mapstructure:"jwt"`
	Static    StaticConfig    `mapstructure:"static"`
	RateLimit RateLimitConfig `mapstructure:"rateLimit"`
}

// LoggingConfig controls structured log output.
type LoggingConfig struct {
	// Level sets the minimum log severity. One of: debug, info, warn, error.
	// Default: "info".
	Level string `mapstructure:"level"`

	// Format selects the log encoder. One of: json, console.
	// Default: "json".
	Format string `mapstructure:"format"`

	// AddCallerInfo adds the file:line caller location to each log entry.
	// Default: false.
	AddCallerInfo bool `mapstructure:"addCallerInfo"`
}

// MetricsConfig controls Prometheus metrics exposition.
type MetricsConfig struct {
	// Enabled toggles the /metrics endpoint. Default: true.
	Enabled bool `mapstructure:"enabled"`

	// Path is the HTTP path at which Prometheus can scrape metrics.
	// Default: "/metrics".
	Path string `mapstructure:"path"`
}

// TracingConfig controls OpenTelemetry tracing.
type TracingConfig struct {
	// Enabled toggles OTLP trace export. Default: false.
	Enabled bool `mapstructure:"enabled"`

	// OTLPEndpoint is the gRPC endpoint of the OTLP collector, e.g. "localhost:4317".
	OTLPEndpoint string `mapstructure:"otlpEndpoint"`

	// ServiceName is the service.name resource attribute sent with every span.
	// Default: "capp-backend".
	ServiceName string `mapstructure:"serviceName"`

	// SampleRate is the fraction of traces to sample, in [0,1]. Default: 0.1.
	SampleRate float64 `mapstructure:"sampleRate"`
}

// InlineCredential holds cluster credentials specified directly in the config
// rather than via a kubeconfig file. Sensitive fields should be injected via
// environment variables (e.g. CAPP_CLUSTERS_0_CREDENTIAL_INLINE_TOKEN).
type InlineCredential struct {
	// APIServer is the Kubernetes API server URL, e.g. "https://api.example.com:6443".
	APIServer string `mapstructure:"apiServer"`

	// CACert is a base64-encoded PEM certificate authority bundle.
	// If empty, TLS server verification is disabled (Insecure: true).
	CACert string `mapstructure:"caCert"`

	// Token is the service-account or user bearer token.
	Token string `mapstructure:"token"`
}

// CredentialConfig specifies how to authenticate to a cluster.
// Exactly one of KubeconfigPath or Inline must be set.
type CredentialConfig struct {
	// KubeconfigPath is the path to a kubeconfig file on disk.
	// If set, Inline is ignored.
	KubeconfigPath string `mapstructure:"kubeconfigPath"`

	// Inline holds credentials embedded directly in this config file.
	Inline *InlineCredential `mapstructure:"inline"`
}

// ClusterConfig defines one managed Kubernetes/OpenShift cluster.
type ClusterConfig struct {
	// Name is the unique identifier used in API paths: /api/v1/clusters/{name}/...
	// Must be a valid URL path segment (no slashes or spaces).
	Name string `mapstructure:"name"`

	// DisplayName is a human-readable label returned to the frontend.
	// Defaults to Name if unset.
	DisplayName string `mapstructure:"displayName"`

	// Credential specifies how the backend authenticates to this cluster.
	Credential CredentialConfig `mapstructure:"credential"`

	// AllowedNamespaces restricts which namespaces are accessible via this
	// cluster entry. An empty list permits all namespaces.
	AllowedNamespaces []string `mapstructure:"allowedNamespaces"`
}

// ResourceToggle is a simple feature flag for a resource handler.
type ResourceToggle struct {
	// Enabled controls whether this resource's routes are registered.
	// Default: true.
	Enabled bool `mapstructure:"enabled"`
}

// ResourcesConfig is a collection of per-resource feature flags.
// Disabling a resource removes its routes entirely at startup — no 404s,
// no handler overhead.
type ResourcesConfig struct {
	Namespaces ResourceToggle `mapstructure:"namespaces"`
	Capps      ResourceToggle `mapstructure:"capps"`
}

// Config is the root configuration object for the capp-backend server.
// It is populated once at startup by Load and then treated as read-only.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Logging   LoggingConfig   `mapstructure:"logging"`
	Metrics   MetricsConfig   `mapstructure:"metrics"`
	Tracing   TracingConfig   `mapstructure:"tracing"`
	Clusters  []ClusterConfig `mapstructure:"clusters"`
	Resources ResourcesConfig `mapstructure:"resources"`
}

// Load reads configuration from the file at path (if non-empty) and from
// CAPP_* environment variables, applies defaults, and returns a validated
// Config. It does NOT call Validate — callers should do that separately so
// they can decide how to handle validation errors.
func Load(path string) (*Config, error) {
	v := viper.New()
	setDefaults(v)

	// Bind environment variables. CAPP_SERVER_PORT → server.port, etc.
	v.SetEnvPrefix("CAPP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	// If DisplayName was not set, fall back to Name.
	for i := range cfg.Clusters {
		if cfg.Clusters[i].DisplayName == "" {
			cfg.Clusters[i].DisplayName = cfg.Clusters[i].Name
		}
	}

	return &cfg, nil
}

// setDefaults registers Viper default values for every config key.
// These are the lowest-priority values: they are overridden by the config
// file and by environment variables.
func setDefaults(v *viper.Viper) {
	// Server
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.readTimeoutSeconds", 30)
	v.SetDefault("server.writeTimeoutSeconds", 30)
	v.SetDefault("server.idleTimeoutSeconds", 60)
	v.SetDefault("server.corsAllowedOrigins", []string{})

	// Auth
	v.SetDefault("auth.mode", "passthrough")
	v.SetDefault("auth.jwt.tokenTTLMinutes", 60)
	v.SetDefault("auth.jwt.refreshTTLMinutes", 1440)
	v.SetDefault("auth.rateLimit.enabled", true)
	v.SetDefault("auth.rateLimit.requestsPerSecond", 20.0)
	v.SetDefault("auth.rateLimit.burst", 40)

	// Logging
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")
	v.SetDefault("logging.addCallerInfo", false)

	// Metrics
	v.SetDefault("metrics.enabled", true)
	v.SetDefault("metrics.path", "/metrics")

	// Tracing
	v.SetDefault("tracing.enabled", false)
	v.SetDefault("tracing.serviceName", "capp-backend")
	v.SetDefault("tracing.sampleRate", 0.1)

	// Resources — all enabled by default
	v.SetDefault("resources.namespaces.enabled", true)
	v.SetDefault("resources.capps.enabled", true)
}
