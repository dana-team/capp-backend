package cluster

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/pkg/k8s"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClusterManager manages all configured Kubernetes cluster connections.
// All methods are safe for concurrent use.
type ClusterManager interface {
	// Get returns the ClusterClient for the named cluster.
	// Returns ErrClusterNotFound if the cluster is not configured.
	Get(name string) (*ClusterClient, error)

	// List returns a snapshot of metadata for all configured clusters,
	// including their current health status.
	List() []ClusterMeta

	// ClientFor creates a controller-runtime client scoped to the given
	// ClusterCredential. In passthrough mode, cred.BearerToken overrides
	// the base RestConfig's token for this request only — the shared base
	// config is never mutated.
	//
	// Returns an error only if the underlying K8s client factory fails, which
	// in practice only happens for malformed rest.Config values.
	ClientFor(cluster *ClusterClient, cred auth.ClusterCredential) (client.Client, error)

	// IsNamespaceAllowed returns true if the given namespace is accessible on
	// this cluster. Always returns true when AllowedNamespaces is empty.
	IsNamespaceAllowed(cluster *ClusterClient, namespace string) bool

	// StartHealthChecks runs periodic liveness checks against all clusters
	// and updates each cluster's Healthy status. This method blocks until ctx
	// is cancelled; callers should run it in a goroutine:
	//
	//   go mgr.StartHealthChecks(ctx, 30)
	StartHealthChecks(ctx context.Context, intervalSeconds int)

	// IsAnyHealthy returns true if at least one cluster is currently healthy.
	// Used by the /readyz probe.
	IsAnyHealthy() bool
}

// defaultClusterManager is the production ClusterManager implementation.
// The clusters map is read-only after New returns. Health status is stored
// in each ClusterClient.healthy (atomic.Bool) so no mutex is needed.
type defaultClusterManager struct {
	clusters map[string]*ClusterClient
	logger   *zap.Logger
}

// New creates a ClusterManager from the provided cluster configs.
//
// Each cluster is registered into the in-memory registry. If a cluster cannot
// be initialised (e.g. bad kubeconfig syntax), New returns an error. If a
// cluster is simply unreachable at startup, it is registered as unhealthy and
// a warning is logged — this is not fatal so that the server can still serve
// other healthy clusters.
func New(cfgs []config.ClusterConfig, scheme *runtime.Scheme, logger *zap.Logger) (ClusterManager, error) {
	mgr := &defaultClusterManager{
		clusters: make(map[string]*ClusterClient, len(cfgs)),
		logger:   logger,
	}

	for _, cfg := range cfgs {
		cc, err := BuildClusterClient(cfg, scheme)
		if err != nil {
			return nil, fmt.Errorf("cluster manager: initialising cluster %q: %w", cfg.Name, err)
		}

		if cc.RestConfig.Insecure {
			logger.Warn("TLS verification disabled for cluster — do not use in production",
				zap.String("cluster", cfg.Name),
			)
		}

		// Build a reusable HTTP client for health probes to avoid leaking
		// transports on every check interval.
		hc, err := rest.HTTPClientFor(cc.RestConfig)
		if err != nil {
			return nil, fmt.Errorf("cluster manager: building health client for %q: %w", cfg.Name, err)
		}
		cc.healthClient = hc

		// Perform an initial health check synchronously so the first /readyz
		// response reflects real cluster state rather than always returning 503.
		if err := checkHealth(cc.healthClient, cc.RestConfig.Host); err != nil {
			logger.Warn("cluster is unreachable at startup",
				zap.String("cluster", cfg.Name),
				zap.Error(err),
			)
			cc.healthy.Store(false)
		} else {
			cc.healthy.Store(true)
			logger.Info("cluster connected", zap.String("cluster", cfg.Name))
		}

		// Detect whether the cluster runs OpenShift by probing the route.openshift.io API group.
		isOS, err := k8s.IsOpenShift(context.Background(), cc.RestConfig)
		if err != nil {
			logger.Warn("could not detect OpenShift platform",
				zap.String("cluster", cfg.Name),
				zap.Error(err),
			)
		} else {
			cc.Meta.IsOpenShift = isOS
			if isOS {
				logger.Info("OpenShift cluster detected", zap.String("cluster", cfg.Name))
			}
		}

		mgr.clusters[cfg.Name] = cc
	}

	return mgr, nil
}

// Get returns the ClusterClient for the named cluster.
func (m *defaultClusterManager) Get(name string) (*ClusterClient, error) {
	cc, ok := m.clusters[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrClusterNotFound, name)
	}
	return cc, nil
}

// List returns a copy of all cluster metadata with current health status.
func (m *defaultClusterManager) List() []ClusterMeta {
	metas := make([]ClusterMeta, 0, len(m.clusters))
	for _, cc := range m.clusters {
		meta := cc.Meta
		meta.Healthy = cc.healthy.Load()
		metas = append(metas, meta)
	}
	return metas
}

// ClientFor builds a per-request controller-runtime client. The base
// RestConfig is copied so that injecting the caller's bearer token does not
// affect other in-flight requests.
func (m *defaultClusterManager) ClientFor(cluster *ClusterClient, cred auth.ClusterCredential) (client.Client, error) {
	// Copy the base config to avoid mutating the shared struct.
	reqCfg := rest.CopyConfig(cluster.RestConfig)

	// Override the token with the caller's credential when provided.
	// In passthrough mode, every request carries a different user token.
	// In jwt / service-account mode, cred may be empty and the base token is used.
	if cred.BearerToken != "" {
		reqCfg.BearerToken = cred.BearerToken
		// Clear any token file path so BearerToken takes precedence.
		reqCfg.BearerTokenFile = ""
	}

	// Apply impersonation headers when present (openshift auth mode).
	// The base config's service-account token is used for authentication,
	// while Impersonate-User/Group headers enforce the end-user's RBAC identity.
	if cred.ImpersonateUser != "" {
		reqCfg.Impersonate = rest.ImpersonationConfig{
			UserName: cred.ImpersonateUser,
			Groups:   cred.ImpersonateGroups,
		}
	}

	return client.New(reqCfg, client.Options{Scheme: cluster.Scheme})
}

// IsNamespaceAllowed returns true if namespace is in the allowed list,
// or if the allowed list is empty (meaning all namespaces are permitted).
func (m *defaultClusterManager) IsNamespaceAllowed(cluster *ClusterClient, namespace string) bool {
	if len(cluster.Meta.AllowedNamespaces) == 0 {
		return true
	}
	for _, ns := range cluster.Meta.AllowedNamespaces {
		if ns == namespace {
			return true
		}
	}
	return false
}

// StartHealthChecks ticks every intervalSeconds and pings each cluster's
// /version endpoint. It updates each ClusterClient.healthy atomically and
// logs state transitions.
func (m *defaultClusterManager) StartHealthChecks(ctx context.Context, intervalSeconds int) {
	interval := time.Duration(intervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runHealthChecks()
		}
	}
}

// IsAnyHealthy returns true if at least one cluster is currently healthy.
func (m *defaultClusterManager) IsAnyHealthy() bool {
	for _, cc := range m.clusters {
		if cc.healthy.Load() {
			return true
		}
	}
	return false
}

// runHealthChecks checks all clusters and updates their health status.
func (m *defaultClusterManager) runHealthChecks() {
	for name, cc := range m.clusters {
		err := checkHealth(cc.healthClient, cc.RestConfig.Host)

		wasHealthy := cc.healthy.Load()
		cc.healthy.Store(err == nil)

		if err != nil && wasHealthy {
			m.logger.Warn("cluster became unhealthy",
				zap.String("cluster", name), zap.Error(err))
		} else if err == nil && !wasHealthy {
			m.logger.Info("cluster recovered", zap.String("cluster", name))
		}
	}
}

// checkHealth performs a GET /version probe against the cluster to verify
// reachability. A 200 response is considered healthy regardless of the
// response body. The provided httpClient is reused across calls to avoid
// leaking transports.
func checkHealth(httpClient *http.Client, host string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := host + "/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe returned status %d", resp.StatusCode)
	}
	return nil
}
