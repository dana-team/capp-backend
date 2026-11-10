// Package server wires together the Gin HTTP engine, middleware stack, and
// resource handler registry into a runnable HTTP server.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	cappapi "github.com/dana-team/capp-backend/api"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources"
	"github.com/dana-team/capp-backend/internal/resources/cluster/namespaces"
	"github.com/dana-team/capp-backend/internal/server/ui"
	"github.com/dana-team/capp-backend/pkg/clientversion"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// Server wraps the http.Server and exposes a clean Start/Stop API.
type Server struct {
	httpServer    *http.Server
	logger        *zap.Logger
	authorization *authorizationRuntime
}

// New builds the Gin engine and starts authorization observation when enabled.
// The backend version is compared with known client versions by middleware.ClientVersion.
// Namespace handlers are assembled here; registry supplies the other resources.
// Call Stop to release workers, even when Start has not been called.
func New(
	cfg *config.Config,
	version string,
	authMgr auth.AuthManager,
	clusterMgr cluster.ClusterManager,
	registry *resources.Registry,
	logger *zap.Logger,
) (*Server, error) {
	var authorization *authorizationRuntime
	var rules *policy.Routes
	if cfg.Authorization.Mode == config.AuthorizationModeBackendPolicy {
		metricsPath := ""
		if cfg.Metrics.Enabled {
			metricsPath = cfg.Metrics.Path
		}
		var err error
		rules, err = policy.NewRoutes(metricsPath)
		if err != nil {
			return nil, err
		}
		authorization, err = newAuthorization(cfg, clusterMgr)
		if err != nil {
			return nil, err
		}
	}

	// In production, suppress Gin's default debug output — our logging
	// middleware handles structured request logging.
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	// ── Global middleware (applied to every route) ────────────────────────────
	engine.Use(middleware.Recovery(logger))
	engine.Use(middleware.Logging(logger))
	engine.Use(middleware.Metrics())
	engine.Use(middleware.CORS(cfg.Server.CORSAllowedOrigins))
	engine.Use(middleware.ClientVersion(version, clientversion.Known, logger))

	if cfg.Auth.RateLimit.Enabled {
		engine.Use(middleware.RateLimit(
			cfg.Auth.RateLimit.RequestsPerSecond,
			cfg.Auth.RateLimit.Burst,
			cfg.Auth.RateLimit.MaxIPTracked,
		))
	}

	if authorization != nil {
		engine.Use(policy.New(authMgr, clusterMgr, rules, authorization.targets, logger))
	}

	// ── Observability endpoints (no auth required) ────────────────────────────
	engine.GET("/healthz", healthz)
	engine.GET("/readyz", readyz(clusterMgr, authorization))

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

	// ── Capp size definitions (no auth required) ────────────────────────────
	engine.GET("/api/v1/sizes", cappSizes(cfg.Sizes))

	// ── Auth endpoints ────────────────────────────────────────────────────────
	authGroup := engine.Group("/api/v1/auth")
	registerAuthRoutes(authGroup, authMgr, cfg.Auth.Mode)

	// ── Cluster endpoints (listing only — no cluster middleware needed) ────────
	clusterListGroup := engine.Group("/api/v1/clusters")
	if authorization == nil {
		clusterListGroup.Use(middleware.Auth(authMgr))
	}
	clusterListGroup.GET("", listClusters(clusterMgr))
	clusterListGroup.GET("/:cluster", getCluster(clusterMgr))

	// ── Resource endpoints ───────────────────────────────────────────────────
	// All resource routes live under /api/v1/clusters/:cluster and require
	// both authentication and cluster resolution. Kubernetes mode uses the group
	// middleware below; backend-policy mode uses the global policy chain.
	resourceGroup := engine.Group("/api/v1/clusters/:cluster")
	if authorization == nil {
		resourceGroup.Use(middleware.Auth(authMgr))
		resourceGroup.Use(middleware.Cluster(clusterMgr))
	}
	if cfg.Resources.Namespaces.Enabled {
		if authorization != nil {
			registry.Register(namespaces.NewWithAdministration(logger, authorization.administration))
		} else {
			registry.Register(namespaces.New(logger))
		}
	}
	registry.Mount(resourceGroup)

	if authorization != nil {
		authorization.start(logger)
	}

	return &Server{
		httpServer: &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
			Handler:      engine,
			ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSeconds) * time.Second,
			WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSeconds) * time.Second,
			IdleTimeout:  time.Duration(cfg.Server.IdleTimeoutSeconds) * time.Second,
		},
		logger:        logger,
		authorization: authorization,
	}, nil
}

// Start begins listening for HTTP requests. It blocks until the server is
// stopped by a call to Stop or until a listener error occurs.
// http.ErrServerClosed is suppressed (it is the expected error on graceful shutdown).
func (s *Server) Start() error {
	s.logger.Info("starting HTTP server", zap.String("addr", s.httpServer.Addr))
	if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		if s.authorization != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err = errors.Join(err, s.authorization.stop(ctx))
		}
		return fmt.Errorf("server: listen: %w", err)
	}
	return nil
}

// Stop gracefully drains in-flight requests and shuts down the listener.
// The provided context controls the maximum drain timeout.
func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("shutting down HTTP server")
	err := s.httpServer.Shutdown(ctx)
	if s.authorization != nil {
		err = errors.Join(err, s.authorization.stop(ctx))
	}
	return err
}

// ── Built-in handlers ─────────────────────────────────────────────────────────

// healthz is the liveness probe. Always returns 200 if the process is alive.
func healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz requires a healthy target and, in backend-policy mode, completed
// initial observations on that same target. Transient watch failures do not
// discard previously synchronized state.
func readyz(clusterMgr cluster.ClusterManager, authorization *authorizationRuntime) gin.HandlerFunc {
	return func(c *gin.Context) {
		var ready bool
		var reason string
		if authorization != nil {
			ready = authorization.ready(clusterMgr)
			reason = "no healthy synchronized clusters available"
		} else {
			ready = clusterMgr.IsAnyHealthy()
			reason = "no healthy clusters available"
		}
		if ready {
			c.JSON(http.StatusOK, gin.H{"status": "ready"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "not ready",
			"reason": reason,
		})
	}
}

// cappSizes returns the configured t-shirt size definitions.
func cappSizes(sizes config.CappSizes) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, sizes)
	}
}
