// Package server wires together the Gin HTTP engine, middleware stack, and
// resource handler registry into a runnable HTTP server.
package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	cappapi "github.com/dana-team/capp-backend/api"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources"
	"github.com/dana-team/capp-backend/internal/server/ui"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// Server wraps the http.Server and exposes a clean Start/Stop API.
type Server struct {
	httpServer *http.Server
	logger     *zap.Logger
}

// New builds the Gin engine, registers all middleware and routes, and returns
// a Server ready to call Start on.
func New(
	cfg *config.Config,
	authMgr auth.AuthManager,
	clusterMgr cluster.ClusterManager,
	registry *resources.Registry,
	logger *zap.Logger,
) *Server {
	// In production, suppress Gin's default debug output — our logging
	// middleware handles structured request logging.
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	// ── Global middleware (applied to every route) ────────────────────────────
	engine.Use(middleware.Recovery(logger))
	engine.Use(middleware.Logging(logger))
	engine.Use(middleware.Metrics())
	engine.Use(middleware.CORS(cfg.Server.CORSAllowedOrigins))

	if cfg.Auth.RateLimit.Enabled {
		engine.Use(middleware.RateLimit(
			cfg.Auth.RateLimit.RequestsPerSecond,
			cfg.Auth.RateLimit.Burst,
		))
	}

	// ── Observability endpoints (no auth required) ────────────────────────────
	engine.GET("/healthz", healthz)
	engine.GET("/readyz", readyz(clusterMgr))

	if cfg.Metrics.Enabled {
		engine.GET(cfg.Metrics.Path, gin.WrapH(promhttp.Handler()))
	}

	// ── API documentation (no auth required) ─────────────────────────────────
	// All three endpoints are served from assets embedded in the binary so
	// that the docs work in disconnected / air-gapped environments.
	engine.GET("/docs", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", ui.DocsHTML)
	})
	engine.GET("/ui/scalar.js", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/javascript; charset=utf-8", ui.ScalarJS)
	})
	engine.GET("/openapi.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", cappapi.Spec)
	})

	// ── Auth endpoints ────────────────────────────────────────────────────────
	authGroup := engine.Group("/api/v1/auth")
	registerAuthRoutes(authGroup, authMgr, cfg.Auth.Mode)

	// ── Cluster endpoints (listing only — no cluster middleware needed) ────────
	clusterListGroup := engine.Group("/api/v1/clusters")
	clusterListGroup.Use(middleware.Auth(authMgr))
	clusterListGroup.GET("", listClusters(clusterMgr))
	clusterListGroup.GET("/:cluster", getCluster(clusterMgr))

	// ── Resource endpoints (auth + cluster middleware) ────────────────────────
	// All resource routes live under /api/v1/clusters/:cluster and require
	// both authentication and cluster resolution.
	resourceGroup := engine.Group("/api/v1/clusters/:cluster")
	resourceGroup.Use(middleware.Auth(authMgr))
	resourceGroup.Use(middleware.Cluster(clusterMgr))
	registry.Mount(resourceGroup)

	return &Server{
		httpServer: &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
			Handler:      engine,
			ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSeconds) * time.Second,
			WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSeconds) * time.Second,
			IdleTimeout:  time.Duration(cfg.Server.IdleTimeoutSeconds) * time.Second,
		},
		logger: logger,
	}
}

// Start begins listening for HTTP requests. It blocks until the server is
// stopped by a call to Stop or until a listener error occurs.
// http.ErrServerClosed is suppressed (it is the expected error on graceful shutdown).
func (s *Server) Start() error {
	s.logger.Info("starting HTTP server", zap.String("addr", s.httpServer.Addr))
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: listen: %w", err)
	}
	return nil
}

// Stop gracefully drains in-flight requests and shuts down the listener.
// The provided context controls the maximum drain timeout.
func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("shutting down HTTP server")
	return s.httpServer.Shutdown(ctx)
}

// ── Built-in handlers ─────────────────────────────────────────────────────────

// healthz is the liveness probe. Always returns 200 if the process is alive.
func healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz is the readiness probe. Returns 200 only when at least one cluster
// is healthy. The frontend / load balancer should not send traffic until
// the backend is ready.
func readyz(clusterMgr cluster.ClusterManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if clusterMgr.IsAnyHealthy() {
			c.JSON(http.StatusOK, gin.H{"status": "ready"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "not ready",
			"reason": "no healthy clusters available",
		})
	}
}

// listClusters returns metadata for all configured clusters.
func listClusters(mgr cluster.ClusterManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"items": mgr.List()})
	}
}

// getCluster returns metadata for a single cluster by name.
func getCluster(mgr cluster.ClusterManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("cluster")
		for _, m := range mgr.List() {
			if m.Name == name {
				c.JSON(http.StatusOK, m)
				return
			}
		}
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    "CLUSTER_NOT_FOUND",
				"message": fmt.Sprintf("cluster %q is not configured", name),
				"status":  http.StatusNotFound,
			},
		})
	}
}
