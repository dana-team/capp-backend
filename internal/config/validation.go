package config

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/go-ldap/ldap/v3"
	"github.com/golang-jwt/jwt/v5"
	imagename "github.com/google/go-containerregistry/pkg/name"
	"github.com/prometheus/prometheus/promql/parser"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/api/validation"
)

// validAuthModes is the set of accepted values for AuthConfig.Mode.
var validAuthModes = map[string]struct{}{
	AuthModePassthrough: {},
	AuthModeOpenShift:   {},
	AuthModeLDAP:        {},
}

// validGitOpsAuthMethods is the set of accepted values for GitOpsConfig.AuthMethod.
var validGitOpsAuthMethods = map[string]struct{}{
	"token": {},
	"ssh":   {},
}

// validMetricTypes is the set of accepted values for MetricsQuery.Type.
var validMetricTypes = map[string]struct{}{
	"gauge":     {},
	"counter":   {},
	"histogram": {},
}

// metricNameRe matches a DNS-1123 label, because the metric name is a path
// segment.
var metricNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// promLabelNameRe matches a Prometheus label name.
var promLabelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Validate checks the fully-loaded Config for semantic errors that cannot be
// expressed as Viper defaults (e.g. required fields, cross-field constraints).
// All validation errors are collected and returned together so the operator
// can fix them all at once without repeated restarts.
func Validate(cfg *Config) error {
	var errs []error

	errs = append(errs, validateAuthorization(cfg)...)
	errs = append(errs, validateClusters(cfg.Clusters)...)
	errs = append(errs, validateAuth(&cfg.Auth)...)
	errs = append(errs, validateGitOps(&cfg.GitOps)...)
	errs = append(errs, validateObservability(&cfg.Resources.Observability, len(cfg.Clusters))...)
	errs = append(errs, validateCfunc(&cfg.Resources.Cfunc)...)
	errs = append(errs, validateCfuncRouterURLs(cfg.Clusters)...)

	if cfg.Metrics.Enabled {
		if err := ValidateMetricsPath(cfg.Metrics.Path); err != nil {
			errs = append(errs, fmt.Errorf("config: metrics.path: %w", err))
		}
	}

	if cfg.GitOps.Enabled {
		errs = append(errs, validateGitOpsPaths(cfg.Clusters)...)
	}

	// Backend policy always executes with a system credential; LDAP and OpenShift
	// also require one for Kubernetes impersonation.
	if cfg.Authorization.Mode == AuthorizationModeBackendPolicy || cfg.Auth.Mode == AuthModeOpenShift || cfg.Auth.Mode == AuthModeLDAP {
		for i, c := range cfg.Clusters {
			if c.Credential.Inline == nil || c.Credential.Inline.Token == "" {
				errs = append(errs, fmt.Errorf(
					"config: clusters[%d] (%q): credential.inline.token is required when auth.mode is '%s' with authorization.mode %q "+
						"(used as the system credential for cluster access)",
					i, c.Name, cfg.Auth.Mode, cfg.Authorization.Mode,
				))
			}
		}
	}

	return errors.Join(errs...)
}

// ValidateMetricsPath requires a nonempty, absolute, literal, canonical path
// outside the reserved /api subtree. Callers decide whether metrics are enabled.
func ValidateMetricsPath(metricsPath string) error {
	if metricsPath == "" {
		return errors.New("metrics path is required when metrics are enabled")
	}
	if !strings.HasPrefix(metricsPath, "/") || strings.ContainsAny(metricsPath, ":*?#") {
		return errors.New("metrics path must be an absolute literal path")
	}
	// Gin cleans registered paths but preserves a single trailing slash.
	canonicalPath := path.Clean(metricsPath)
	if strings.HasSuffix(metricsPath, "/") && canonicalPath != "/" {
		canonicalPath += "/"
	}
	if metricsPath != canonicalPath {
		return fmt.Errorf("metrics path must be canonical: use %q", canonicalPath)
	}
	if metricsPath == "/api" || strings.HasPrefix(metricsPath, "/api/") {
		return errors.New("metrics path must not use the reserved /api subtree")
	}
	return nil
}

// validateClusters checks that at least one cluster is defined, that every
// cluster has a unique non-empty name, and that each credential block is valid.
func validateClusters(clusters []ClusterConfig) []error {
	var errs []error

	if len(clusters) == 0 {
		return []error{errors.New("config: at least one cluster must be defined under 'clusters'")}
	}

	seen := make(map[string]struct{}, len(clusters))
	for i, c := range clusters {
		prefix := fmt.Sprintf("config: clusters[%d]", i)

		if c.Name == "" {
			errs = append(errs, fmt.Errorf("%s: 'name' is required", prefix))
		} else {
			if _, dup := seen[c.Name]; dup {
				errs = append(errs, fmt.Errorf("%s: duplicate cluster name %q", prefix, c.Name))
			}
			seen[c.Name] = struct{}{}
		}

		errs = append(errs, validateCredential(prefix, &c.Credential)...)
	}

	return errs
}

// validateCredential ensures exactly one of kubeconfigPath or inline is set,
// and that inline credentials are complete when provided.
func validateCredential(prefix string, cred *CredentialConfig) []error {
	var errs []error

	hasKubeconfig := cred.KubeconfigPath != ""
	hasInline := cred.Inline != nil

	if !hasKubeconfig && !hasInline {
		errs = append(errs, fmt.Errorf(
			"%s.credential: either 'kubeconfigPath' or 'inline' must be set", prefix,
		))
		return errs
	}

	if hasInline {
		if cred.Inline.APIServer == "" {
			errs = append(errs, fmt.Errorf(
				"%s.credential.inline: 'apiServer' is required", prefix,
			))
		}
		// Token may be empty during development when the cluster does not
		// enforce authentication, so we do not require it here. The cluster
		// loader will produce an unauthenticated rest.Config in that case.
	}

	return errs
}

// validateAuth checks that auth.mode is known and that mode-specific required
// fields are present.
func validateAuth(auth *AuthConfig) []error {
	var errs []error

	if auth.RateLimit.Enabled && auth.RateLimit.MaxIPTracked <= 0 {
		errs = append(errs, fmt.Errorf(
			"config: auth.rateLimit.maxIPTracked must be greater than 0, got %d",
			auth.RateLimit.MaxIPTracked,
		))
	}

	if _, ok := validAuthModes[auth.Mode]; !ok {
		errs = append(errs, fmt.Errorf(
			"config: auth.mode %q is not valid; must be one of: passthrough, openshift, ldap",
			auth.Mode,
		))
		// Cannot validate mode-specific fields if mode is unknown.
		return errs
	}

	if auth.Mode == AuthModeOpenShift {
		if auth.OpenShift.APIServer == "" {
			errs = append(errs, errors.New(
				"config: auth.openshift.apiServer is required when auth.mode is 'openshift'",
			))
		}
		if auth.OpenShift.ClientID == "" {
			errs = append(errs, errors.New(
				"config: auth.openshift.clientId is required when auth.mode is 'openshift'",
			))
		}
		if auth.OpenShift.ClientSecret == "" {
			errs = append(errs, errors.New(
				"config: auth.openshift.clientSecret is required when auth.mode is 'openshift'; "+
					"set via CAPP_AUTH_OPENSHIFT_CLIENTSECRET environment variable",
			))
		}
		if auth.OpenShift.RedirectURI == "" {
			errs = append(errs, errors.New(
				"config: auth.openshift.redirectUri is required when auth.mode is 'openshift'",
			))
		}
	}
	if auth.Mode == AuthModeLDAP {
		errs = append(errs, validateLDAP(&auth.LDAP)...)
	}

	return errs
}

// validateLDAP checks only the selected LDAP mode and never contacts the directory.
func validateLDAP(cfg *LDAPConfig) []error {
	var errs []error
	if err := validateLDAPURL(cfg.URL); err != nil {
		errs = append(errs, err)
	}
	if cfg.CACert != "" {
		pemBytes, err := base64.StdEncoding.DecodeString(cfg.CACert)
		if err != nil || !x509.NewCertPool().AppendCertsFromPEM(pemBytes) {
			errs = append(errs, errors.New("config: auth.ldap.caCert must be a base64 PEM CA bundle"))
		}
	}
	errs = append(errs, validateLDAPDirectory(cfg)...)
	for _, field := range []struct {
		name  string
		value string
	}{
		{"issuer", cfg.Issuer}, {"audience", cfg.Audience},
	} {
		if strings.TrimSpace(field.value) == "" {
			errs = append(errs, fmt.Errorf("config: auth.ldap.%s is required", field.name))
		}
	}
	for _, field := range []struct {
		name  string
		value int
	}{
		{"timeoutSeconds", cfg.TimeoutSeconds}, {"tokenTTLSeconds", cfg.TokenTTLSeconds},
	} {
		if field.value <= 0 {
			errs = append(errs, fmt.Errorf("config: auth.ldap.%s must be greater than 0", field.name))
		}
	}
	return append(errs, validateLDAPKeys(cfg)...)
}

func validateLDAPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "ldaps" || u.Hostname() == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return errors.New("config: auth.ldap.url must be an ldaps:// host/port URL without credentials, path, query, or fragment")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return errors.New("config: auth.ldap.url port must be between 1 and 65535")
		}
	}
	return nil
}

func validateLDAPDirectory(cfg *LDAPConfig) []error {
	var errs []error
	for _, field := range []struct {
		name  string
		value string
	}{
		{"bindDN", cfg.BindDN}, {"userBaseDN", cfg.UserBaseDN}, {"groupBaseDN", cfg.GroupBaseDN},
	} {
		if strings.TrimSpace(field.value) == "" {
			errs = append(errs, fmt.Errorf("config: auth.ldap.%s is required", field.name))
		} else if _, err := ldap.ParseDN(field.value); err != nil {
			errs = append(errs, fmt.Errorf("config: auth.ldap.%s must be a valid LDAP DN", field.name))
		}
	}
	if cfg.BindPassword == "" {
		errs = append(errs, errors.New("config: auth.ldap.bindPassword is required; supply it through a Secret or CAPP_AUTH_LDAP_BINDPASSWORD"))
	}
	for _, field := range []struct {
		name        string
		value       string
		placeholder string
	}{
		{"userFilter", cfg.UserFilter, "{username}"}, {"groupFilter", cfg.GroupFilter, "{userDN}"},
	} {
		if err := validateLDAPFilter(field.name, field.value, field.placeholder); err != nil {
			errs = append(errs, err)
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"usernameAttribute", cfg.UsernameAttribute}, {"groupNameAttribute", cfg.GroupNameAttribute},
	} {
		if !ldapAttributePattern.MatchString(field.value) {
			errs = append(errs, fmt.Errorf("config: auth.ldap.%s must be an LDAP attribute description", field.name))
		}
	}
	return errs
}

// Attribute descriptions use RFC 4512 descriptors or numeric OIDs and optional options.
var ldapAttributePattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*|(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*))+)(;[A-Za-z0-9-]+)*$`)

func validateLDAPFilter(name, filter, placeholder string) error {
	if strings.Count(filter, placeholder) != 1 {
		return fmt.Errorf("config: auth.ldap.%s must contain exactly one %s placeholder", name, placeholder)
	}
	if _, err := ldap.CompileFilter(strings.ReplaceAll(filter, placeholder, "value")); err != nil {
		return fmt.Errorf("config: auth.ldap.%s must be a valid LDAP filter", name)
	}
	return nil
}

func validateLDAPKeys(cfg *LDAPConfig) []error {
	var errs []error
	if !ldapKeyIDPattern.MatchString(cfg.SigningKeyID) {
		errs = append(errs, errors.New("config: auth.ldap.signingKeyID must contain only lowercase ASCII letters, digits, '-' or '_' and be nonempty"))
	}
	parsedPrivate, err := jwt.ParseEdPrivateKeyFromPEM([]byte(cfg.SigningPrivateKey))
	privateKey, ok := parsedPrivate.(ed25519.PrivateKey)
	if err != nil || !ok {
		errs = append(errs, errors.New("config: auth.ldap.signingPrivateKey must be a PEM Ed25519 private key"))
	}
	for id, value := range cfg.VerificationKeys {
		if err := validateLDAPVerificationKeyID(id); err != nil {
			errs = append(errs, err)
		}
		parsedPublic, err := jwt.ParseEdPublicKeyFromPEM([]byte(value))
		publicKey, ok := parsedPublic.(ed25519.PublicKey)
		if err != nil || !ok {
			errs = append(errs, errors.New("config: auth.ldap.verificationKeys must contain PEM Ed25519 public keys"))
			continue
		}
		if id == cfg.SigningKeyID && privateKey != nil && !publicKey.Equal(privateKey.Public()) {
			errs = append(errs, errors.New("config: auth.ldap.verificationKeys conflicts with the active signing key ID"))
		}
	}
	return errs
}

func validateLDAPFileKeyIDs(value any, path string) error {
	var entries map[string]any
	switch values := value.(type) {
	case map[string]any:
		entries = values
	case map[any]any:
		// YAML may decode numeric IDs as numbers; preserve Viper's string mapping
		// without silently merging IDs that stringify identically.
		entries = make(map[string]any, len(values))
		for key, value := range values {
			name := fmt.Sprint(key)
			if _, exists := entries[name]; exists {
				return errors.New("config: auth.ldap.verificationKeys contains conflicting key IDs")
			}
			entries[name] = value
		}
	}
	for key, value := range entries {
		if path == "" {
			if err := validateLDAPVerificationKeyID(key); err != nil {
				return err
			}
			continue
		}
		lower := strings.ToLower(key)
		switch {
		case lower == path:
			if err := validateLDAPFileKeyIDs(value, ""); err != nil {
				return err
			}
		case strings.HasPrefix(path, lower+"."):
			if err := validateLDAPFileKeyIDs(value, path[len(key)+1:]); err != nil {
				return err
			}
		case strings.HasPrefix(lower, path+"."):
			if err := validateLDAPVerificationKeyID(key[len(path)+1:]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateLDAPVerificationKeyID(id string) error {
	if !ldapKeyIDPattern.MatchString(id) {
		return errors.New("config: auth.ldap.verificationKeys IDs must contain only lowercase ASCII letters, digits, '-' or '_' and be nonempty")
	}
	return nil
}

var ldapKeyIDPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// validateGitOps checks that gitops configuration is complete when enabled.
func validateGitOps(g *GitOpsConfig) []error {
	if !g.Enabled {
		return nil
	}

	var errs []error

	if g.RepoURL == "" {
		errs = append(errs, errors.New(
			"config: gitops.repoURL is required when gitops.enabled is true",
		))
	}

	if g.TimeoutSeconds <= 0 {
		errs = append(errs, fmt.Errorf(
			"config: gitops.timeoutSeconds must be greater than 0, got %d",
			g.TimeoutSeconds,
		))
	}

	if _, ok := validGitOpsAuthMethods[g.AuthMethod]; !ok {
		errs = append(errs, fmt.Errorf(
			"config: gitops.authMethod %q is not valid; must be one of: token, ssh",
			g.AuthMethod,
		))
		return errs
	}

	switch g.AuthMethod {
	case "token":
		if g.Token == "" {
			errs = append(errs, errors.New(
				"config: gitops.token is required when gitops.authMethod is 'token'; "+
					"set via CAPP_GITOPS_TOKEN environment variable",
			))
		}
	case "ssh":
		if g.SSHKeyPath == "" {
			errs = append(errs, errors.New(
				"config: gitops.sshKeyPath is required when gitops.authMethod is 'ssh'; "+
					"set via CAPP_GITOPS_SSHKEYPATH environment variable",
			))
		}
	}

	return errs
}

// validateMetricSeries checks that a metric sets exactly one of query and
// series, and that every query is scoped to the requested Capp.
func validateMetricSeries(field string, m MetricsQuery) []error {
	hasQuery := strings.TrimSpace(m.Query) != ""
	switch {
	case !hasQuery && len(m.Series) == 0:
		return []error{fmt.Errorf("%s.query or series is required", field)}
	case hasQuery && len(m.Series) > 0:
		return []error{fmt.Errorf("%s sets both query and series; set one", field)}
	case hasQuery:
		return validateSeriesQuery(field+".query", m.Query)
	}

	var errs []error
	if len(m.Series) > MaxMetricSeries {
		errs = append(errs, fmt.Errorf("%s.series has %d entries; at most %d are allowed", field, len(m.Series), MaxMetricSeries))
	}
	legends := make(map[string]struct{}, len(m.Series))
	for j, s := range m.Series {
		sf := fmt.Sprintf("%s.series[%d]", field, j)
		if s.Legend == "" {
			errs = append(errs, fmt.Errorf("%s.legend is required", sf))
		} else if _, dup := legends[s.Legend]; dup {
			errs = append(errs, fmt.Errorf("%s.legend %q is a duplicate", sf, s.Legend))
		}
		legends[s.Legend] = struct{}{}
		if strings.TrimSpace(s.Query) == "" {
			errs = append(errs, fmt.Errorf("%s.query is required", sf))
		} else {
			errs = append(errs, validateSeriesQuery(sf+".query", s.Query)...)
		}
	}
	return errs
}

// selectorSentinel stands in for MetricsSelectorPlaceholder while a query is
// parsed, so the parser sees a valid label matcher where the placeholder
// would expand to the Capp's matchers.
const selectorSentinel = "__capp_selector_placeholder__"

// validateSeriesQuery parses query as PromQL and checks that every vector
// selector, including ones written with no braces (e.g. `errors_total`),
// contains MetricsSelectorPlaceholder in its label matchers. Any unscoped
// selector could return other Capps' series to a caller authorized only for
// the requested Capp.
func validateSeriesQuery(field, query string) []error {
	expanded := strings.ReplaceAll(query, MetricsSelectorPlaceholder, selectorSentinel+`="1"`)
	expr, err := parser.ParseExpr(expanded)
	if err != nil {
		return []error{fmt.Errorf("%s is not valid PromQL: %w", field, err)}
	}

	var unscoped []string
	scoped := 0
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		vs, ok := node.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		for _, lm := range vs.LabelMatchers {
			if lm.Name == selectorSentinel {
				scoped++
				return nil
			}
		}
		unscoped = append(unscoped, vs.String())
		return nil
	})

	if scoped == 0 {
		return []error{fmt.Errorf("%s must contain %s inside a vector selector's {} so results are limited to one Capp", field, MetricsSelectorPlaceholder)}
	}
	errs := make([]error, 0, len(unscoped))
	for _, sel := range unscoped {
		errs = append(errs, fmt.Errorf(
			"%s has a selector (%s) that does not contain %s, so its results are not limited to one Capp",
			field, sel, MetricsSelectorPlaceholder,
		))
	}
	return errs
}

// validateObservability checks that observability configuration is complete
// when enabled: an absolute http(s) query URL and at least one well-formed
// metric whose query is scoped to the requested Capp. queryURL is shared by
// every configured cluster, so with more than one cluster each metric must
// set clusterLabel; otherwise a Capp with the same namespace and name on
// another cluster would match too.
func validateObservability(o *ObservabilityConfig, clusterCount int) []error {
	if !o.Enabled {
		return nil
	}

	var errs []error

	queryURL := strings.TrimSpace(o.QueryURL)
	if queryURL == "" {
		errs = append(errs, errors.New(
			"config: resources.observability.queryURL is required when resources.observability.enabled is true; "+
				"set via CAPP_RESOURCES_OBSERVABILITY_QUERYURL environment variable",
		))
	} else if u, err := url.Parse(queryURL); err != nil || u.Host == "" || (u.Scheme != httpScheme && u.Scheme != httpsScheme) {
		// The value is left out of the error because it can carry a token.
		errs = append(errs, errors.New("config: resources.observability.queryURL must be an absolute http or https URL"))
	}

	if len(o.Metrics) == 0 {
		errs = append(errs, errors.New(
			"config: resources.observability.metrics is required when resources.observability.enabled is true",
		))
	}

	for name := range o.Labels {
		if !promLabelNameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("config: resources.observability.labels key %q is not a valid Prometheus label name", name))
		}
	}

	namesSeen := make(map[string]struct{}, len(o.Metrics))
	for i, m := range o.Metrics {
		field := fmt.Sprintf("config: resources.observability.metrics[%d]", i)

		switch {
		case m.Name == "":
			errs = append(errs, fmt.Errorf("%s.name is required", field))
		case !metricNameRe.MatchString(m.Name):
			errs = append(errs, fmt.Errorf("%s.name %q must be a DNS-1123 label", field, m.Name))
		default:
			if _, dup := namesSeen[m.Name]; dup {
				errs = append(errs, fmt.Errorf("%s.name %q is a duplicate", field, m.Name))
			}
			namesSeen[m.Name] = struct{}{}
		}

		errs = append(errs, validateMetricSeries(field, m)...)

		if m.Type == "" {
			errs = append(errs, fmt.Errorf("%s.type is required", field))
		} else if _, ok := validMetricTypes[m.Type]; !ok {
			errs = append(errs, fmt.Errorf("%s.type must be one of 'gauge', 'counter', or 'histogram'", field))
		}

		if m.NamespaceLabel == "" {
			errs = append(errs, fmt.Errorf("%s.namespaceLabel is required", field))
		}
		if m.ObjectLabel == "" {
			errs = append(errs, fmt.Errorf("%s.objectLabel is required", field))
		}
		if m.ClusterLabel == "" && clusterCount > 1 {
			errs = append(errs, fmt.Errorf(
				"%s.clusterLabel is required when more than one cluster is configured, "+
					"so results are limited to the requested cluster", field,
			))
		}
		for key, label := range map[string]string{
			"clusterLabel":   m.ClusterLabel,
			"namespaceLabel": m.NamespaceLabel,
			"objectLabel":    m.ObjectLabel,
		} {
			if label != "" && !promLabelNameRe.MatchString(label) {
				errs = append(errs, fmt.Errorf("%s.%s %q is not a valid Prometheus label name", field, key, label))
			}
			if _, collides := o.Labels[label]; label != "" && collides {
				errs = append(errs, fmt.Errorf(
					"%s.%s %q also appears in resources.observability.labels; it would duplicate a matcher in every query",
					field, key, label,
				))
			}
		}
	}

	return errs
}

// MetricsWithoutClusterLabel returns the names of the metrics whose queries
// are not limited to one cluster. It returns nil when observability is
// disabled.
//
// Validate requires clusterLabel only when more than one cluster is
// configured, because a Prometheus that serves a single cluster usually has no
// cluster label to match. The backend cannot tell whether queryURL is such a
// server or one shared with other clusters, where a Capp with the same
// namespace and name elsewhere would match too. The caller logs these names at
// startup so the operator can check.
func (o *ObservabilityConfig) MetricsWithoutClusterLabel() []string {
	if !o.Enabled {
		return nil
	}
	var names []string
	for _, m := range o.Metrics {
		if m.ClusterLabel == "" {
			names = append(names, m.Name)
		}
	}
	return names
}

// validateCfunc checks that the flavor and size maps are usable when enabled.
func validateCfunc(e *CfuncConfig) []error {
	const prefix = "config: resources.cfunc"
	if !e.Enabled {
		if e.Functions.Enabled {
			return []error{fmt.Errorf("%s.functions.enabled needs %s.enabled", prefix, prefix)}
		}
		return nil
	}

	var errs []error
	errs = append(errs, validateCfuncFunctions(&e.Functions)...)

	if strings.Contains(e.RegistryPrefix, "://") || strings.HasSuffix(e.RegistryPrefix, "/") {
		errs = append(errs, fmt.Errorf("%s.registryPrefix must be a registry path with no scheme and no trailing slash", prefix))
	}
	if e.ImagePullSecret != "" {
		if msgs := validation.NameIsDNSSubdomain(e.ImagePullSecret, false); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("%s.imagePullSecret: %s", prefix, strings.Join(msgs, "; ")))
		}
	}

	if e.MaxPoolSize < 1 {
		errs = append(errs, fmt.Errorf("%s.maxPoolSize must be 1 or greater, got %d", prefix, e.MaxPoolSize))
	}

	if len(e.Sizes) == 0 {
		errs = append(errs, fmt.Errorf("%s.sizes must name at least one size when cfunc is enabled", prefix))
	} else if _, ok := e.Sizes[e.DefaultSize]; !ok {
		errs = append(errs, fmt.Errorf("%s.defaultSize %q is not one of the configured sizes", prefix, e.DefaultSize))
	}
	// A read reports the size by matching the pool's resources, so two
	// sizes with the same values could not be told apart.
	sizeByValues := make(map[string]string, len(e.Sizes))
	for _, name := range e.SizeNames() {
		values, err := sizeValues(e.Sizes[name])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.sizes.%s: %w", prefix, name, err))
			continue
		}
		if other, ok := sizeByValues[values]; ok {
			errs = append(errs, fmt.Errorf("%s.sizes.%s has the same resources as size %q", prefix, name, other))
			continue
		}
		sizeByValues[values] = name
	}

	if len(e.Flavors) == 0 {
		errs = append(errs, fmt.Errorf("%s.flavors must name at least one flavor when cfunc is enabled", prefix))
	}
	// A read reports the flavor by matching the pool's runtime image.
	flavorByImage := make(map[string]string, len(e.Flavors))
	for _, name := range e.FlavorNames() {
		flavor := e.Flavors[name]
		if flavor.Image == "" {
			errs = append(errs, fmt.Errorf("%s.flavors.%s.image is required", prefix, name))
			continue
		}
		if other, ok := flavorByImage[flavor.Image]; ok {
			errs = append(errs, fmt.Errorf("%s.flavors.%s has the same image as flavor %q", prefix, name, other))
		}
		flavorByImage[flavor.Image] = name
		if flavor.BuildCommand != "" && flavor.BuildImage == "" {
			errs = append(errs, fmt.Errorf("%s.flavors.%s.buildCommand needs a buildImage", prefix, name))
		}
		for _, field := range []struct{ key, image string }{{"image", flavor.Image}, {"buildImage", flavor.BuildImage}} {
			if err := validateFlavorImage(e, field.image); err != nil {
				errs = append(errs, fmt.Errorf("%s.flavors.%s.%s %q: %w", prefix, name, field.key, field.image, err))
			}
		}
		for i, variable := range flavor.BuilderEnv {
			if variable.Name == "" {
				errs = append(errs, fmt.Errorf("%s.flavors.%s.builderEnv[%d].name is required", prefix, name, i))
			}
		}
	}

	return errs
}

// validateCfuncFunctions checks the git server and the limits of functions.
// No message prints the token.
func validateCfuncFunctions(f *CfuncFunctionsConfig) []error {
	if !f.Enabled {
		return nil
	}

	const prefix = "config: resources.cfunc.functions"
	var errs []error

	if err := validateHTTPSBaseURL(f.Git.BaseURL); err != nil {
		errs = append(errs, fmt.Errorf("%s.git.baseURL: %w", prefix, err))
	}
	if f.Git.Token == "" {
		errs = append(errs, fmt.Errorf("%s.git.token is required; set CAPP_RESOURCES_CFUNC_FUNCTIONS_GIT_TOKEN", prefix))
	}
	if f.Git.Username == "" {
		errs = append(errs, fmt.Errorf("%s.git.username is required", prefix))
	}

	for _, limit := range []struct {
		key   string
		value int
	}{
		{"maxTriggers", f.MaxTriggers},
		{"maxTimeoutSeconds", f.MaxTimeoutSeconds},
		{"defaultConcurrency", f.DefaultConcurrency},
		{"maxConcurrency", f.MaxConcurrency},
		{"minCronIntervalSeconds", f.MinCronIntervalSeconds},
	} {
		if limit.value < 1 {
			errs = append(errs, fmt.Errorf("%s.%s must be 1 or greater, got %d", prefix, limit.key, limit.value))
		}
	}
	if f.DefaultConcurrency > f.MaxConcurrency {
		errs = append(errs, fmt.Errorf("%s.defaultConcurrency %d is larger than maxConcurrency %d",
			prefix, f.DefaultConcurrency, f.MaxConcurrency))
	}

	return errs
}

const (
	httpScheme  = "http"
	httpsScheme = "https"
)

// validateHTTPSBaseURL requires an https URL with a host and nothing a
// prefix match could be confused by: no credentials, query or fragment.
func validateHTTPSBaseURL(raw string) error {
	if raw == "" {
		return errors.New("is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if parsed.Scheme != httpsScheme || parsed.Host == "" {
		return errors.New("must be an https URL with a host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must not carry credentials, a query or a fragment")
	}
	return nil
}

// validateCfuncRouterURLs checks the optional Fission router URL of each cluster.
func validateCfuncRouterURLs(clusters []ClusterConfig) []error {
	var errs []error
	for i, c := range clusters {
		if c.CfuncRouterURL == "" {
			continue
		}
		parsed, err := url.Parse(c.CfuncRouterURL)
		if err != nil || (parsed.Scheme != httpScheme && parsed.Scheme != httpsScheme) || parsed.Host == "" ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			errs = append(errs, fmt.Errorf(
				"config: clusters[%d] (%q): cfuncRouterURL must be an http or https URL with a host and no credentials, query or fragment",
				i, c.Name))
		}
	}
	return errs
}

// validateFlavorImage checks that an image joined with the registry prefix is
// a pullable reference, so that a typo fails at startup and not as an image
// pull error on every pool of the flavor.
func validateFlavorImage(e *CfuncConfig, image string) error {
	if image == "" {
		return nil
	}
	// With a prefix, an image that names its own registry would be joined
	// into <prefix>/<registry>/<image>, which parses but cannot be pulled.
	if first, _, hasPath := strings.Cut(image, "/"); e.RegistryPrefix != "" && hasPath &&
		(strings.ContainsAny(first, ".:") || first == "localhost") {
		return fmt.Errorf("already names a registry (%s); with registryPrefix set, give the image relative to it", first)
	}
	if _, err := imagename.ParseReference(e.ImageRef(image)); err != nil {
		return fmt.Errorf("%s is not a valid image reference", e.ImageRef(image))
	}
	return nil
}

// validateCfuncFileNames rejects a flavor or size name that contains a dot.
// Viper splits such a key into nested maps, which would otherwise surface as
// a confusing "image is required" for the first half of the name.
func validateCfuncFileNames(raw map[string]any) error {
	cfunc := rawSection(rawSection(raw, "resources"), "cfunc")
	for _, section := range []string{"flavors", "sizes"} {
		for key := range rawSection(cfunc, section) {
			if strings.Contains(key, ".") {
				return fmt.Errorf("config: resources.cfunc.%s: name %q must not contain a dot", section, key)
			}
		}
	}
	return nil
}

// rawSection returns the nested map under key, matching the key without
// regard to case as Viper does.
func rawSection(m map[string]any, key string) map[string]any {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			section, _ := v.(map[string]any)
			return section
		}
	}
	return nil
}

// sizeValues parses a size and returns its four quantities in canonical form.
func sizeValues(size ResourceSize) (string, error) {
	fields := []struct{ name, value string }{
		{"requests.cpu", size.Requests.CPU},
		{"requests.memory", size.Requests.Memory},
		{"limits.cpu", size.Limits.CPU},
		{"limits.memory", size.Limits.Memory},
	}
	quantities := make([]resource.Quantity, len(fields))
	canonical := make([]string, len(fields))
	for i, field := range fields {
		quantity, err := resource.ParseQuantity(field.value)
		if err != nil {
			return "", fmt.Errorf("invalid %s %q: %w", field.name, field.value, err)
		}
		if quantity.Sign() < 0 {
			return "", fmt.Errorf("%s %q must not be negative", field.name, field.value)
		}
		quantities[i] = quantity
		canonical[i] = quantity.String()
	}
	if quantities[0].Cmp(quantities[2]) > 0 {
		return "", errors.New("requests.cpu is larger than limits.cpu")
	}
	if quantities[1].Cmp(quantities[3]) > 0 {
		return "", errors.New("requests.memory is larger than limits.memory")
	}
	return strings.Join(canonical, "/"), nil
}

// validateGitOpsPaths checks that every cluster's GitOpsPath (after fallback)
// is a valid filesystem path segment. Called only when gitops.enabled is true.
func validateGitOpsPaths(clusters []ClusterConfig) []error {
	var errs []error
	for i, c := range clusters {
		p := c.GitOpsPath
		if p == "" {
			p = c.Name
		}
		if strings.ContainsAny(p, "/ ") {
			errs = append(errs, fmt.Errorf(
				"config: clusters[%d] (%q): gitOpsPath %q must not contain slashes or spaces",
				i, c.Name, p,
			))
		}
	}
	return errs
}

func validateAuthorization(cfg *Config) []error {
	if cfg.Authorization.Mode != AuthorizationModeBackendPolicy {
		return []error{errors.New("config: authorization.mode must be explicitly set to backend-policy")}
	}
	var errs []error
	if cfg.Authorization.RestrictedBindingNamePrefixes == nil {
		errs = append(errs, errors.New("config: authorization.restrictedBindingNamePrefixes must be explicitly supplied and non-null"))
	} else if err := authorization.ValidateBindingNamePrefixes(cfg.Authorization.RestrictedBindingNamePrefixes); err != nil {
		errs = append(errs, fmt.Errorf("config: authorization.restrictedBindingNamePrefixes: %w", err))
	}
	for i, c := range cfg.Clusters {
		if cfg.Auth.Mode == AuthModePassthrough {
			errs = append(errs, validatePassthroughReviewer(i, c.Credential)...)
		}
		a := c.Authorization
		if len(validation.ValidateNamespaceName(a.PolicyNamespace, false)) != 0 {
			errs = append(errs, fmt.Errorf("config: clusters[%d].authorization.policyNamespace must be a valid namespace", i))
		}
		for _, ns := range a.ProtectedNamespaces {
			if len(validation.ValidateNamespaceName(ns, false)) != 0 {
				errs = append(errs, fmt.Errorf("config: clusters[%d].authorization.protectedNamespaces contains an invalid namespace", i))
			}
		}
		for _, identities := range [][]string{a.OperatorUsers, a.OperatorGroups} {
			for _, identity := range identities {
				if strings.TrimSpace(identity) == "" {
					errs = append(errs, fmt.Errorf("config: clusters[%d].authorization operator lists must not contain blank entries", i))
				}
			}
		}
	}
	return errs
}

func validatePassthroughReviewer(index int, credential CredentialConfig) []error {
	var errs []error
	prefix := fmt.Sprintf("config: clusters[%d].credential", index)
	// Kubeconfig takes precedence in client construction. Unused inline
	// trust must not make a different reviewer connection appear safe.
	if credential.KubeconfigPath != "" {
		errs = append(errs, fmt.Errorf("%s.kubeconfigPath must be unset for backend-policy passthrough; configure inline reviewer credentials", prefix))
	}
	if inline := credential.Inline; inline != nil {
		u, err := url.Parse(inline.APIServer)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			errs = append(errs, fmt.Errorf("%s.inline.apiServer must be an absolute HTTPS URL for backend-policy passthrough", prefix))
		}
		ca, err := base64.StdEncoding.DecodeString(inline.CACert)
		if err != nil || !x509.NewCertPool().AppendCertsFromPEM(ca) {
			errs = append(errs, fmt.Errorf("%s.inline.caCert must be a base64-encoded PEM certificate bundle for backend-policy passthrough", prefix))
		}
		if inline.Insecure {
			errs = append(errs, fmt.Errorf("%s.inline.insecure must be false for backend-policy passthrough", prefix))
		}
	}
	return errs
}
