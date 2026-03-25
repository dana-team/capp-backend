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
}

// Validate checks the fully-loaded Config for semantic errors that cannot be
// expressed as Viper defaults (e.g. required fields, cross-field constraints).
// All validation errors are collected and returned together so the operator
// can fix them all at once without repeated restarts.
func Validate(cfg *Config) error {
	var errs []error

	errs = append(errs, validateClusters(cfg.Clusters)...)
	errs = append(errs, validateAuth(&cfg.Auth)...)

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

	if _, ok := validAuthModes[auth.Mode]; !ok {
		errs = append(errs, fmt.Errorf(
			"config: auth.mode %q is not valid; must be one of: passthrough, jwt, static",
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
	}

	return errs
}
