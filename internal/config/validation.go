package config

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-ldap/ldap/v3"
	"github.com/golang-jwt/jwt/v5"
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

// Validate checks the fully-loaded Config for semantic errors that cannot be
// expressed as Viper defaults (e.g. required fields, cross-field constraints).
// All validation errors are collected and returned together so the operator
// can fix them all at once without repeated restarts.
func Validate(cfg *Config) error {
	var errs []error

	errs = append(errs, validateClusters(cfg.Clusters)...)
	errs = append(errs, validateAuth(&cfg.Auth)...)
	errs = append(errs, validateGitOps(&cfg.GitOps)...)
	errs = append(errs, validateBenchmarks(&cfg.Resources.Benchmarks)...)

	if cfg.GitOps.Enabled {
		errs = append(errs, validateGitOpsPaths(cfg.Clusters)...)
	}

	// In impersonation modes every cluster must have an inline token (SA token)
	// because the backend uses impersonation instead of forwarding user tokens.
	if cfg.Auth.Mode == AuthModeOpenShift || cfg.Auth.Mode == AuthModeLDAP {
		for i, c := range cfg.Clusters {
			if c.Credential.Inline == nil || c.Credential.Inline.Token == "" {
				errs = append(errs, fmt.Errorf(
					"config: clusters[%d] (%q): credential.inline.token is required when auth.mode is '%s' "+
						"(used as the service-account token for K8s impersonation)",
					i, c.Name, cfg.Auth.Mode,
				))
			}
		}
	}

	return errors.Join(errs...)
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

// validateBenchmarks checks that benchmark configuration is complete when enabled.
func validateBenchmarks(b *BenchmarkConfig) []error {
	if !b.Enabled {
		return nil
	}

	var errs []error

	if b.Image == "" {
		errs = append(errs, errors.New(
			"config: resources.benchmarks.image is required when resources.benchmarks.enabled is true",
		))
	}
	if len(compactVMURLs(b.VMURLs)) == 0 {
		errs = append(errs, errors.New(
			"config: resources.benchmarks.vmURLs is required when resources.benchmarks.enabled is true; "+
				"set via CAPP_RESOURCES_BENCHMARKS_VMURLS environment variable",
		))
	}

	return errs
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
