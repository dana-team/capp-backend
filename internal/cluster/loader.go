package cluster

import (
	"encoding/base64"
	"fmt"

	"github.com/dana-team/capp-backend/internal/config"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const userAgent = "capp-backend/1.0"

// BuildRestConfig constructs a *rest.Config from a ClusterConfig.
//
// Exactly one of KubeconfigPath or Inline must be set; otherwise an error is
// returned. When both are set, KubeconfigPath takes precedence.
//
// For inline credentials:
//   - APIServer is required.
//   - If CACert is provided it must be a base64-encoded PEM bundle; it is
//     decoded and placed in TLSClientConfig.CAData.
//   - If CACert is empty, TLS server verification is disabled
//     (TLSClientConfig.Insecure = true). This is acceptable for local dev
//     clusters but should NOT be used in production.
//   - Token is placed in the BearerToken field. In passthrough mode this
//     value is overridden per-request by ClusterManager.ClientFor.
func BuildRestConfig(cfg config.ClusterConfig) (*rest.Config, error) {
	var restCfg *rest.Config

	switch {
	case cfg.Credential.KubeconfigPath != "":
		var err error
		restCfg, err = clientcmd.BuildConfigFromFlags("", cfg.Credential.KubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: loading kubeconfig from %q: %w",
				cfg.Name, cfg.Credential.KubeconfigPath, err)
		}

	case cfg.Credential.Inline != nil:
		inline := cfg.Credential.Inline
		restCfg = &rest.Config{
			Host:        inline.APIServer,
			BearerToken: inline.Token,
		}

		if inline.CACert != "" {
			caData, err := base64.StdEncoding.DecodeString(inline.CACert)
			if err != nil {
				return nil, fmt.Errorf("cluster %q: decoding base64 CA cert: %w", cfg.Name, err)
			}
			restCfg.TLSClientConfig = rest.TLSClientConfig{CAData: caData}
		} else {
			// No CA provided — skip TLS verification. Log a warning at startup
			// (done by the caller) so operators are aware.
			restCfg.TLSClientConfig = rest.TLSClientConfig{Insecure: true}
		}

	default:
		return nil, fmt.Errorf("cluster %q: no credential configured (set kubeconfigPath or inline)", cfg.Name)
	}

	restCfg.UserAgent = userAgent
	return restCfg, nil
}

// BuildClusterClient builds a fully initialised ClusterClient for the provided
// ClusterConfig. It does NOT validate connectivity at build time — the caller
// (ClusterManager.New) performs the initial health check separately so that
// unreachable clusters can be registered as unhealthy rather than failing
// the entire startup.
func BuildClusterClient(cfg config.ClusterConfig, scheme *runtime.Scheme) (*ClusterClient, error) {
	restCfg, err := BuildRestConfig(cfg)
	if err != nil {
		return nil, err
	}

	displayName := cfg.DisplayName
	if displayName == "" {
		displayName = cfg.Name
	}

	return &ClusterClient{
		Meta: ClusterMeta{
			Name:              cfg.Name,
			DisplayName:       displayName,
			Healthy:           false, // updated by the first health check
			AllowedNamespaces: cfg.AllowedNamespaces,
		},
		RestConfig: restCfg,
		Scheme:     scheme,
	}, nil
}
