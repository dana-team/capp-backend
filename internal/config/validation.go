package config

import (
	"errors"
	"fmt"
)

// validAuthModes is the set of accepted values for AuthConfig.Mode.
var validAuthModes = map[string]struct{}{
	"passthrough": {},
	"jwt":         {},
	"static":      {},
	"dex":         {},
	"openshift":   {},
}

// Validate checks the fully-loaded Config for semantic errors that cannot be
// expressed as Viper defaults (e.g. required fields, cross-field constraints).
// All validation errors are collected and returned together so the operator
// can fix them all at once without repeated restarts.
// validGitOpsAuthMethods is the set of accepted values for GitOpsConfig.AuthMethod.
var validGitOpsAuthMethods = map[string]struct{}{
	"token": {},
	"ssh":   {},
}

func Validate(cfg *Config) error {
	var errs []error

	errs = append(errs, validateClusters(cfg.Clusters)...)
	errs = append(errs, validateAuth(&cfg.Auth)...)
	errs = append(errs, validateGitOps(&cfg.GitOps)...)

	// In openshift mode every cluster must have an inline token (SA token)
	// because the backend uses impersonation instead of forwarding user tokens.
	if cfg.Auth.Mode == "openshift" {
		for i, c := range cfg.Clusters {
			if c.Credential.Inline == nil || c.Credential.Inline.Token == "" {
				errs = append(errs, fmt.Errorf(
					"config: clusters[%d] (%q): credential.inline.token is required when auth.mode is 'openshift' "+
						"(used as the service-account token for K8s impersonation)",
					i, c.Name,
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

// validateAuth checks that auth.mode is known and that mode-specific required
// fields are present.
func validateAuth(auth *AuthConfig) []error {
	var errs []error

	if _, ok := validAuthModes[auth.Mode]; !ok {
		errs = append(errs, fmt.Errorf(
			"config: auth.mode %q is not valid; must be one of: passthrough, jwt, static, dex, openshift",
			auth.Mode,
		))
		// Cannot validate mode-specific fields if mode is unknown.
		return errs
	}

	switch auth.Mode {
	case "jwt":
		if auth.JWT.SecretKey == "" {
			errs = append(errs, errors.New(
				"config: auth.jwt.secretKey is required when auth.mode is 'jwt'; "+
					"set it via the CAPP_AUTH_JWT_SECRETKEY environment variable",
			))
		}
	case "static":
		if len(auth.Static.APIKeys) == 0 {
			errs = append(errs, errors.New(
				"config: auth.static.apiKeys must contain at least one key when auth.mode is 'static'",
			))
		}
	case "dex":
		if auth.Dex.Endpoint == "" {
			errs = append(errs, errors.New(
				"config: auth.dex.endpoint is required when auth.mode is 'dex'",
			))
		}
		if auth.Dex.ClientID == "" {
			errs = append(errs, errors.New(
				"config: auth.dex.clientId is required when auth.mode is 'dex'",
			))
		}
		if auth.Dex.ClientSecret == "" {
			errs = append(errs, errors.New(
				"config: auth.dex.clientSecret is required when auth.mode is 'dex'; "+
					"set via CAPP_AUTH_DEX_CLIENTSECRET environment variable",
			))
		}
		if auth.JWT.SecretKey == "" {
			errs = append(errs, errors.New(
				"config: auth.jwt.secretKey is required when auth.mode is 'dex' "+
					"(used for signing backend session JWTs); "+
					"set via CAPP_AUTH_JWT_SECRETKEY environment variable",
			))
		}
	case "openshift":
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

	return errs
}
