// Package config loads, parses, and exposes the capp-backend configuration.
//
// Configuration is sourced from three places in priority order (highest first):
//  1. Environment variables prefixed with CAPP_ (e.g. CAPP_AUTH_MODE)
//  2. A YAML config file whose path is passed to Load
//  3. Hard-coded default values embedded in this package
//
// All nested keys are flattened with underscores for env var mapping, e.g.
// auth.mode → CAPP_AUTH_MODE.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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

// OpenShiftConfig holds settings for the OpenShift OAuth auth mode.
// This mode authenticates users via the OpenShift OAuth server of the cluster
// where the backend is deployed and impersonates the user on all managed
// clusters using per-cluster service-account tokens.
type OpenShiftConfig struct {
	// APIServer is the external OpenShift API server URL (e.g.,
	// "https://api.ocp.example.com:6443"). Used for OAuth discovery and
	// building the authorize URL that the user's browser navigates to.
	// This is NOT kubernetes.default.svc — it is the external route.
	APIServer string `mapstructure:"apiServer"`

	// CACert is a base64-encoded PEM CA bundle for TLS to the OAuth server.
	// If empty, the system root CAs are used.
	CACert string `mapstructure:"caCert"`

	// ClientID is the OAuth client ID registered in OpenShift for capp-backend.
	ClientID string `mapstructure:"clientId"`

	// ClientSecret is the OAuth client secret. Inject via CAPP_AUTH_OPENSHIFT_CLIENTSECRET.
	ClientSecret string `mapstructure:"clientSecret"`

	// RedirectURI is the frontend callback URL that OpenShift redirects to
	// after successful authentication.
	RedirectURI string `mapstructure:"redirectUri"`

	// Scopes is the list of OAuth scopes to request.
	// Default: ["user:info", "user:check-access"].
	Scopes []string `mapstructure:"scopes"`

	// TokenCacheTTLSeconds is how long a validated token's identity is cached
	// in memory to avoid a TokenReview on every request. Default: 60.
	TokenCacheTTLSeconds int `mapstructure:"tokenCacheTTLSeconds"`
}

// LDAPConfig configures directory search-and-bind and backend-issued access tokens.
// Required fields apply only when auth.mode is ldap.
type LDAPConfig struct {
	// URL is the required directory endpoint. Use ldaps:// with verified TLS.
	URL string `mapstructure:"url"`
	// CACert is an optional base64 PEM CA bundle; empty uses system trust roots.
	CACert string `mapstructure:"caCert"`
	// BindDN is the required DN of the directory search account, not the target user.
	BindDN string `mapstructure:"bindDN"`
	// BindPassword is the required search-account password; supply it through
	// a Secret or CAPP_AUTH_LDAP_BINDPASSWORD.
	BindPassword string `mapstructure:"bindPassword"`

	// UserBaseDN is the required root DN for user searches.
	UserBaseDN string `mapstructure:"userBaseDN"`
	// UserFilter is the required user search filter, containing exactly one
	// {username} placeholder for an escaped value.
	UserFilter string `mapstructure:"userFilter"`
	// UsernameAttribute supplies the canonical username. Default: "sAMAccountName".
	UsernameAttribute string `mapstructure:"usernameAttribute"`
	// GroupBaseDN is the required root DN for group searches.
	GroupBaseDN string `mapstructure:"groupBaseDN"`
	// GroupFilter contains exactly one {userDN} placeholder for an escaped DN.
	// Default: "(&(objectClass=group)(member={userDN}))" (direct membership).
	GroupFilter string `mapstructure:"groupFilter"`
	// GroupNameAttribute supplies group names for authorization. Default: "sAMAccountName".
	GroupNameAttribute string `mapstructure:"groupNameAttribute"`

	// TimeoutSeconds bounds the entire directory login; must be positive. Default: 10.
	TimeoutSeconds int `mapstructure:"timeoutSeconds"`
	// TokenTTLSeconds sets access-token lifetime; must be positive. Default: 900.
	TokenTTLSeconds int `mapstructure:"tokenTTLSeconds"`
	// Issuer is the required token-issuer identifier, consistent across backend instances.
	Issuer string `mapstructure:"issuer"`
	// Audience is the required intended-recipient identifier, consistent across backend instances.
	Audience string `mapstructure:"audience"`

	// SigningKeyID is the required active signing-key identifier used in token headers.
	// Use only lowercase ASCII letters, digits, '-' or '_'.
	SigningKeyID string `mapstructure:"signingKeyID"`
	// SigningPrivateKey is the required PEM Ed25519 private key; supply it through
	// a Secret or CAPP_AUTH_LDAP_SIGNINGPRIVATEKEY.
	SigningPrivateKey string `mapstructure:"signingPrivateKey"`
	// VerificationKeys optionally maps retiring/future key IDs to PEM Ed25519 public keys.
	// The active public key is derived from SigningPrivateKey.
	VerificationKeys map[string]string `mapstructure:"verificationKeys"`
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

	// MaxIPTracked is the upper bound on tracked unique client IPs. When the
	// limit is reached, the least-recently-seen entries are evicted.
	// 10 000 entries × ~200 bytes ≈ 2 MB. Default: 10000.
	MaxIPTracked int `mapstructure:"maxIPTracked"`
}

// Accepted values for AuthConfig.Mode.
const (
	AuthModePassthrough = "passthrough"
	AuthModeOpenShift   = "openshift"
	AuthModeLDAP        = "ldap"
)

// AuthConfig holds all authentication-related settings.
type AuthConfig struct {
	// Mode selects the authentication strategy. One of:
	//   passthrough — the client's Kubernetes bearer token is forwarded directly
	//                 to the cluster; no session is created server-side.
	//   openshift   — OpenShift OAuth + K8s impersonation; fully stateless.
	//   ldap        — directory search-and-bind + locally verified access tokens.
	// Default: "passthrough".
	Mode      string          `mapstructure:"mode"`
	OpenShift OpenShiftConfig `mapstructure:"openshift"`
	LDAP      LDAPConfig      `mapstructure:"ldap"`
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
	// Required unless Insecure is explicitly set to true.
	CACert string `mapstructure:"caCert"`

	// Insecure disables TLS server verification for this cluster. Must be
	// explicitly set to true when CACert is omitted — the backend will not
	// silently skip verification.
	Insecure bool `mapstructure:"insecure"`

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

	// GitOpsPath is the directory name used in the GitOps repository path:
	// <pathPrefix>/<gitOpsPath>/<namespace>/<cappName>.yaml
	// Defaults to Name if unset. Must be a valid filesystem path segment
	// (no slashes or spaces).
	GitOpsPath string `mapstructure:"gitOpsPath"`
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
	Namespaces    ResourceToggle `mapstructure:"namespaces"`
	Capps         ResourceToggle `mapstructure:"capps"`
	Capprevisions ResourceToggle `mapstructure:"capprevisions"`
	Configmaps    ResourceToggle `mapstructure:"configmaps"`
	Secrets       ResourceToggle `mapstructure:"secrets"`

	// Observability configures the Capp metrics endpoints served by the
	// capps handler. It is not a handler toggle of its own.
	Observability ObservabilityConfig `mapstructure:"observability"`
}

// MetricsSelectorPlaceholder is replaced in every MetricsQuery.Query with the
// label matchers that scope the query to one Capp, for example
// `namespace="team-a",capp="web"`. It goes inside a vector selector's braces:
// `rate(http_requests_total{$selector}[5m])`.
const MetricsSelectorPlaceholder = "$selector"

// ObservabilityConfig configures the per-Capp metrics endpoints. Callers
// pick a metric by name; they never send PromQL.
type ObservabilityConfig struct {
	// Enabled toggles the metrics endpoints. Default: false.
	Enabled bool `mapstructure:"enabled"`

	// QueryURL is the Prometheus-compatible base URL that PromQL queries read
	// from. Any query parameters, such as an optional ?apikey=..., are kept on
	// every request, which is how a token reaches the server.
	// Required when enabled. Inject via CAPP_RESOURCES_OBSERVABILITY_QUERYURL.
	QueryURL string `mapstructure:"queryURL"`

	// Metrics are the queries served at
	// .../namespaces/{namespace}/capps/{name}/metrics/{metric}. At least one
	// is required when enabled.
	Metrics []MetricsQuery `mapstructure:"metrics"`

	// Labels are extra label matchers added to every query's selector, for
	// example to pin a Prometheus shared by several environments.
	Labels map[string]string `mapstructure:"labels"`
}

// MaxMetricSeries caps MetricsQuery.Series, because each entry is a separate
// Prometheus query run for one request.
const MaxMetricSeries = 10

// MetricsQuery is one named chart. It holds either a single Query or a list
// of Series drawn together, such as p50 and p95 latency.
type MetricsQuery struct {
	// Name identifies the metric in the API path. Must be a DNS-1123 label
	// and unique.
	Name string `mapstructure:"name"`

	// Description explains what the metric measures. Returned to clients.
	Description string `mapstructure:"description"`

	// Type is "gauge", "counter" or "histogram". Returned to clients as a
	// display hint.
	Type string `mapstructure:"type"`

	// Unit is the y-axis unit returned to clients, such as "seconds" or
	// "requests/s". Optional.
	Unit string `mapstructure:"unit"`

	// Query is a single PromQL expression, shorthand for one series whose
	// legend is Name. Set either Query or Series.
	Query string `mapstructure:"query"`

	// Series are several PromQL expressions shown as separate lines on the
	// same chart. Set either Query or Series. At most MaxMetricSeries.
	Series []MetricSeries `mapstructure:"series"`

	// ClusterLabel is the Prometheus label holding the cluster name. The
	// value matched is the cluster name from the request path. Required when
	// more than one cluster is configured, because QueryURL is shared by all
	// of them. With a single cluster it may be left empty; pin a shared
	// Prometheus with ObservabilityConfig.Labels instead.
	ClusterLabel string `mapstructure:"clusterLabel"`

	// NamespaceLabel is the Prometheus label holding the namespace. Required.
	NamespaceLabel string `mapstructure:"namespaceLabel"`

	// ObjectLabel is the Prometheus label holding the Capp name. Required.
	ObjectLabel string `mapstructure:"objectLabel"`
}

// MetricSeries is one line of a MetricsQuery.
type MetricSeries struct {
	// Legend labels the line. Required and unique within the metric.
	Legend string `mapstructure:"legend"`

	// Query is the PromQL expression. It must contain
	// MetricsSelectorPlaceholder in every vector selector.
	Query string `mapstructure:"query"`
}

// AllSeries returns the metric's lines, turning a single Query into one
// series whose legend is the metric name.
func (m MetricsQuery) AllSeries() []MetricSeries {
	if len(m.Series) > 0 {
		return m.Series
	}
	return []MetricSeries{{Legend: m.Name, Query: m.Query}}
}

// GitOpsConfig controls Helm values generation and git push for ArgoCD sync.
type GitOpsConfig struct {
	// Enabled toggles the /sync endpoints and auto-sync on Capps. Default: false.
	Enabled bool `mapstructure:"enabled"`

	// RepoURL is the git repository where values files are pushed.
	// Supports both HTTPS (https://github.com/org/repo.git) and SSH (git@github.com:org/repo.git).
	RepoURL string `mapstructure:"repoURL"`

	// Branch is the target branch in the git repository. Default: "main".
	Branch string `mapstructure:"branch"`

	// AuthMethod selects how the backend authenticates to the git repo.
	// One of: "token" (HTTPS personal/deploy token) or "ssh" (SSH key file).
	// Default: "token".
	AuthMethod string `mapstructure:"authMethod"`

	// Token is the HTTPS bearer/personal-access token. Required when authMethod is "token".
	// Inject via CAPP_GITOPS_TOKEN.
	Token string `mapstructure:"token"`

	// SSHKeyPath is the path to an SSH private key file. Required when authMethod is "ssh".
	SSHKeyPath string `mapstructure:"sshKeyPath"`

	// InsecureHostKey skips SSH host key verification when true. Required for
	// environments without a known_hosts file (e.g. distroless containers,
	// air-gapped clusters with self-signed git servers). Default: false.
	InsecureHostKey bool `mapstructure:"insecureHostKey"`

	// PathPrefix is the directory prefix for per-capp values files in the git
	// repository: <pathPrefix>/<siteName>/<namespace>/<cappName>.yaml
	// Default: "sites".
	PathPrefix string `mapstructure:"pathPrefix"`

	// TimeoutSeconds bounds each git sync/delete operation. Default: 20.
	TimeoutSeconds int `mapstructure:"timeoutSeconds"`
}

type ResourceQuantities struct {
	CPU    string `mapstructure:"cpu"    json:"cpu"`
	Memory string `mapstructure:"memory" json:"memory"`
}

type ResourceSize struct {
	Requests ResourceQuantities `mapstructure:"requests" json:"requests"`
	Limits   ResourceQuantities `mapstructure:"limits"   json:"limits"`
}

type CappSizes struct {
	Small  ResourceSize `mapstructure:"small"  json:"small"`
	Medium ResourceSize `mapstructure:"medium" json:"medium"`
	Large  ResourceSize `mapstructure:"large"  json:"large"`
}

// ResourceConfig holds configuration for all resource handlers,
type ResourceConfig struct {
	CappSizes CappSizes `mapstructure:"cappSizes"`
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
	GitOps    GitOpsConfig    `mapstructure:"gitops"`
	Sizes     CappSizes       `mapstructure:"cappSizes"`
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

	var raw map[string]any
	if path != "" {
		var err error
		raw, err = readConfigFile(v, path)
		if err != nil {
			return nil, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	// Viper lowercases every map key it unmarshals, but Prometheus label
	// names are case-sensitive. Recover the original casing from the raw
	// decoded file instead of the lowercased map produced above.
	if labels := rawStringMap(raw, "resources.observability.labels"); labels != nil {
		cfg.Resources.Observability.Labels = labels
	}

	for i := range cfg.Clusters {
		if cfg.Clusters[i].DisplayName == "" {
			cfg.Clusters[i].DisplayName = cfg.Clusters[i].Name
		}
		if cfg.Clusters[i].GitOpsPath == "" {
			cfg.Clusters[i].GitOpsPath = cfg.Clusters[i].Name
		}

		// Viper's AutomaticEnv does not apply env var overrides to elements
		// inside a slice when using Unmarshal (only to top-level/mapped keys),
		// so CAPP_CLUSTERS_<i>_CREDENTIAL_INLINE_TOKEN must be read explicitly.
		if cfg.Clusters[i].Credential.Inline != nil {
			envKey := fmt.Sprintf("CAPP_CLUSTERS_%d_CREDENTIAL_INLINE_TOKEN", i)
			if token := os.Getenv(envKey); token != "" {
				cfg.Clusters[i].Credential.Inline.Token = token
			}
		}
	}

	cfg.Resources.Observability.QueryURL = strings.TrimSpace(cfg.Resources.Observability.QueryURL)

	return &cfg, nil
}

// readConfigFile loads path into v and returns the file decoded into a plain
// map, with its original key casing intact (viper's own ReadConfig/Unmarshal
// path lowercases every map key, which loses information for values such as
// LDAP key IDs or Prometheus label names that are case-sensitive).
func readConfigFile(v *viper.Viper, path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v.SetConfigFile(path)
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, err
	}

	decoder, err := viper.NewCodecRegistry().Decoder(strings.TrimPrefix(filepath.Ext(path), "."))
	if err != nil {
		return nil, err
	}
	raw := make(map[string]any)
	if err := decoder.Decode(data, raw); err != nil {
		return nil, err
	}

	if v.GetString("auth.mode") == AuthModeLDAP {
		if err := validateLDAPFileKeyIDs(raw, "auth.ldap.verificationkeys"); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

// rawStringMap navigates raw (as decoded by readConfigFile) through the
// dot-separated path, matching each segment case-insensitively, and returns
// the map found at that path with its original key casing. Returns nil if
// the path does not lead to a map.
func rawStringMap(raw map[string]any, path string) map[string]string {
	var cur any = raw
	for segment := range strings.SplitSeq(path, ".") {
		m, ok := asStringKeyedMap(cur)
		if !ok {
			return nil
		}
		found := false
		for key, value := range m {
			if strings.EqualFold(key, segment) {
				cur = value
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}

	m, ok := asStringKeyedMap(cur)
	if !ok {
		return nil
	}
	result := make(map[string]string, len(m))
	for key, value := range m {
		result[key] = fmt.Sprint(value)
	}
	return result
}

// asStringKeyedMap normalizes the two map shapes a YAML/JSON decoder can
// produce (map[string]any, and map[any]any for some YAML decoders) into a
// single map[string]any view.
func asStringKeyedMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for key, value := range m {
			out[fmt.Sprint(key)] = value
		}
		return out, true
	default:
		return nil, false
	}
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
	v.SetDefault("auth.mode", AuthModePassthrough)
	v.SetDefault("auth.openshift.scopes", []string{"user:info", "user:check-access"})
	v.SetDefault("auth.openshift.tokenCacheTTLSeconds", 60)
	// Register required scalar keys even when LDAP is absent from YAML so
	// AutomaticEnv includes environment-only values during Unmarshal.
	for _, key := range []string{
		"url", "caCert", "bindDN", "bindPassword", "userBaseDN", "userFilter",
		"groupBaseDN", "issuer", "audience", "signingKeyID", "signingPrivateKey",
	} {
		v.SetDefault("auth.ldap."+key, "")
	}
	v.SetDefault("auth.ldap.usernameAttribute", "sAMAccountName")
	v.SetDefault("auth.ldap.groupFilter", "(&(objectClass=group)(member={userDN}))")
	v.SetDefault("auth.ldap.groupNameAttribute", "sAMAccountName")
	v.SetDefault("auth.ldap.timeoutSeconds", 10)
	v.SetDefault("auth.ldap.tokenTTLSeconds", 900)
	v.SetDefault("auth.ldap.verificationKeys", map[string]string{})
	v.SetDefault("auth.rateLimit.enabled", true)
	v.SetDefault("auth.rateLimit.requestsPerSecond", 20.0)
	v.SetDefault("auth.rateLimit.burst", 40)
	v.SetDefault("auth.rateLimit.maxIPTracked", 10_000)

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
	v.SetDefault("resources.capprevisions.enabled", true)
	v.SetDefault("resources.configmaps.enabled", true)
	v.SetDefault("resources.secrets.enabled", true)

	// GitOps — disabled by default
	v.SetDefault("gitops.enabled", false)
	v.SetDefault("gitops.branch", "main")
	v.SetDefault("gitops.authMethod", "token")
	v.SetDefault("gitops.pathPrefix", "sites")
	v.SetDefault("gitops.timeoutSeconds", 20)

	// Observability — disabled by default
	v.SetDefault("resources.observability.enabled", false)
	v.SetDefault("resources.observability.queryURL", "")

	// Capp t-shirt sizes
	v.SetDefault("cappSizes.small.requests.cpu", "250m")
	v.SetDefault("cappSizes.small.requests.memory", "256Mi")
	v.SetDefault("cappSizes.small.limits.cpu", "500m")
	v.SetDefault("cappSizes.small.limits.memory", "512Mi")
	v.SetDefault("cappSizes.medium.requests.cpu", "500m")
	v.SetDefault("cappSizes.medium.requests.memory", "512Mi")
	v.SetDefault("cappSizes.medium.limits.cpu", "1")
	v.SetDefault("cappSizes.medium.limits.memory", "1Gi")
	v.SetDefault("cappSizes.large.requests.cpu", "1")
	v.SetDefault("cappSizes.large.requests.memory", "1Gi")
	v.SetDefault("cappSizes.large.limits.cpu", "2")
	v.SetDefault("cappSizes.large.limits.memory", "2Gi")
}
