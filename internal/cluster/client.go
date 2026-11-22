// Package cluster manages all configured Kubernetes/OpenShift cluster
// connections for the capp-backend.
//
// At startup, New reads the clusters config, builds a ClusterClient for each
// entry, and stores them in an in-memory registry. Handlers and middleware
// use SystemClients for backend-authenticated execution and observation.
//
// A background goroutine (StartHealthChecks) polls each cluster's /version
// endpoint on a configurable interval and marks clusters healthy or unhealthy.
// Readiness also requires synchronized policy observations for a healthy cluster.
package cluster

import (
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/dana-team/capp-backend/pkg/api/clusters"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

// ErrClusterNotFound is returned by Get when the named cluster is not configured.
var ErrClusterNotFound = errors.New("cluster not found")

// ClusterMeta holds identifying and health metadata for one managed cluster.
// It lives in pkg/api/clusters so clients can use it without this package.
type ClusterMeta = clusters.ClusterMeta

// ClusterClient wraps the base Kubernetes rest.Config and scheme for a single
// cluster. It is built once at startup and stored in the ClusterManager.
type ClusterClient struct {
	// Meta holds display and health metadata. The Healthy field in Meta is
	// only accurate in snapshots returned by List(). For live health status,
	// use IsHealthy().
	Meta ClusterMeta

	// healthy is the atomic source of truth for this cluster's health status.
	// Written by the health-check goroutine, read by middleware — no mutex needed.
	healthy atomic.Bool

	// healthClient is a reusable HTTP client for the /version health probe.
	// Created once at startup from RestConfig to avoid leaking transports.
	healthClient *http.Client

	// RestConfig contains the backend credentials and API server trust settings.
	// It must not be mutated after initialization.
	RestConfig *rest.Config

	// Scheme has all API types registered that capp-backend works with.
	// Shared across all per-request clients for this cluster.
	Scheme *runtime.Scheme
}

// IsHealthy returns the current health status. Safe for concurrent use.
func (cc *ClusterClient) IsHealthy() bool {
	return cc.healthy.Load()
}

// SetHealthy updates the cluster's health status. Safe for concurrent use.
func (cc *ClusterClient) SetHealthy(v bool) {
	cc.healthy.Store(v)
}
