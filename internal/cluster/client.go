// Package cluster manages all configured Kubernetes/OpenShift cluster
// connections for the capp-backend.
//
// At startup, New reads the clusters config, builds a ClusterClient for each
// entry, and stores them in an in-memory registry. Handlers and middleware
// retrieve per-request scoped clients via ClusterManager.ClientFor, which
// copies the base rest.Config and injects the caller's bearer token so that
// Kubernetes RBAC is enforced at the individual user level in passthrough mode.
//
// A background goroutine (StartHealthChecks) polls each cluster's /version
// endpoint on a configurable interval and marks clusters healthy or unhealthy.
// The /readyz probe returns 503 until at least one cluster is healthy.
package cluster

import (
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/dana-team/capp-backend/pkg/api/clusters"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

var (
	// ErrClusterNotFound is returned by Get when the named cluster is not in
	// the registry.
	ErrClusterNotFound = errors.New("cluster not found")

	// ErrClusterUnhealthy is returned when a cluster exists in the registry
	// but failed its most recent health check.
	ErrClusterUnhealthy = errors.New("cluster is not healthy")

	// ErrNamespaceForbidden is returned by the cluster middleware when the
	// requested namespace is not in the cluster's AllowedNamespaces list.
	ErrNamespaceForbidden = errors.New("namespace is not allowed for this cluster")
)

// ClusterMeta holds identifying and health metadata for one managed cluster.
// It lives in pkg/api/clusters so clients can use it without this package.
type ClusterMeta = clusters.ClusterMeta

// ClusterClient wraps the base Kubernetes rest.Config and scheme for a single
// cluster. It is built once at startup and stored in the ClusterManager.
//
// It does NOT hold a ready-to-use client.Client because the client must carry
// the per-request bearer token (in passthrough mode). Use ClusterManager.ClientFor
// to obtain a scoped client for a specific request.
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

	// RestConfig is the base configuration for this cluster. It contains the
	// API server URL, CA certificate, and (for service-account / inline mode)
	// the backend's own bearer token. In passthrough mode the token is
	// overridden per-request by ClientFor.
	//
	// This field must not be mutated after initialisation — ClientFor copies it.
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
