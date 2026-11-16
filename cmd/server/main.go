// Command server is the entry point for the capp-backend HTTP server.
//
// It performs the following startup sequence:
//  1. Parse flags and load configuration.
//  2. Validate configuration.
//  3. Initialise the structured logger.
//  4. Build the Kubernetes scheme (registers all API types).
//  5. Build the ClusterManager (connects to all configured clusters).
//  6. Build and populate the resource handler registry.
//  7. Build the HTTP server and its configured authentication provider.
//  8. Start listening.
//  9. On SIGTERM/SIGINT: graceful shutdown with a 30-second drain timeout.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/gitops"
	"github.com/dana-team/capp-backend/internal/resources"
	capphandler "github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	revhandler "github.com/dana-team/capp-backend/internal/resources/namespaced/capps/revisions"
	poolhandler "github.com/dana-team/capp-backend/internal/resources/namespaced/cfunc/pools"
	cmhandler "github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	secrethandler "github.com/dana-team/capp-backend/internal/resources/namespaced/secrets"
	"github.com/dana-team/capp-backend/internal/server"
	"github.com/dana-team/capp-backend/pkg/k8s"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// version is set at build time via -ldflags "-X main.version=vX.Y.Z". cappctl
// clients at least one minor release behind it are warned to upgrade.
var version = "dev"

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "", "Path to the YAML configuration file")
	flag.Parse()

	// ── 1. Load configuration ─────────────────────────────────────────────────
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "capp-backend: failed to load config: %v\n", err)
		os.Exit(1)
	}

	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "capp-backend: invalid configuration:\n%v\n", err)
		os.Exit(1)
	}

	// ── 2. Initialise logger ──────────────────────────────────────────────────
	logger, err := buildLogger(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "capp-backend: failed to initialise logger: %v\n", err)
		os.Exit(1)
	}

	logger.Info("capp-backend starting",
		zap.String("version", version),
		zap.String("authMode", cfg.Auth.Mode),
		zap.Int("clusters", len(cfg.Clusters)),
	)

	// ── 3. Build Kubernetes scheme ────────────────────────────────────────────
	scheme, err := k8s.BuildScheme()
	if err != nil {
		logger.Fatal("failed to build Kubernetes scheme", zap.Error(err))
	}

	// ── 4. Build ClusterManager ───────────────────────────────────────────────
	clusterMgr, err := cluster.New(cfg.Clusters, scheme, logger)
	if err != nil {
		logger.Fatal("failed to initialise cluster manager", zap.Error(err))
	}

	// Start background health checks. The goroutine stops when the root
	// context is cancelled on shutdown.
	rootCtx, rootCancel := context.WithCancel(context.Background())
	go clusterMgr.StartHealthChecks(rootCtx, 30)

	// ── 5. Build GitOps client (optional) ─────────────────────────────────────
	var gitopsClient *gitops.Client
	if cfg.GitOps.Enabled {
		var err error
		gitopsClient, err = gitops.NewClient(cfg.GitOps, logger, "")
		if err != nil {
			logger.Fatal("failed to initialise gitops client", zap.Error(err))
		}
		logger.Info("gitops client initialised",
			zap.String("repo", cfg.GitOps.RepoURL),
			zap.String("branch", cfg.GitOps.Branch),
		)
	}

	// ── 6. Build resource registry ────────────────────────────────────────────
	enabledResources := map[string]bool{
		"namespaces":    cfg.Resources.Namespaces.Enabled,
		"capps":         cfg.Resources.Capps.Enabled,
		"capprevisions": cfg.Resources.Capprevisions.Enabled,
		"configmaps":    cfg.Resources.Configmaps.Enabled,
		"secrets":       cfg.Resources.Secrets.Enabled,
		"cfuncpools":    cfg.Resources.Cfunc.Enabled,
	}
	registry := resources.NewRegistry(enabledResources)
	if unscoped := cfg.Resources.Observability.MetricsWithoutClusterLabel(); len(unscoped) > 0 {
		logger.Warn("observability metrics have no clusterLabel, so their queries match every cluster in the Prometheus at queryURL; "+
			"if that Prometheus also holds other clusters, set clusterLabel or pin it with resources.observability.labels",
			zap.Strings("metrics", unscoped),
		)
	}
	cappHandler, err := capphandler.New(cfg.GitOps.Enabled, gitopsClient, clusterMgr, cfg.Sizes, cfg.Resources.Observability)
	if err != nil {
		logger.Fatal("failed to initialise capps handler", zap.Error(err))
	}
	registry.Register(cappHandler)
	registry.Register(revhandler.New(cfg.GitOps.Enabled, gitopsClient, cfg.Sizes))
	registry.Register(cmhandler.New())
	registry.Register(secrethandler.New())
	poolHandler, err := poolhandler.New(cfg.Resources.Cfunc)
	if err != nil {
		logger.Fatal("failed to initialise cfunc pools handler", zap.Error(err))
	}
	registry.Register(poolHandler)
	// ── 7. Build and start HTTP server ────────────────────────────────────────
	srv, err := server.New(cfg, version, nil, clusterMgr, registry, logger)
	if err != nil {
		logger.Fatal("failed to initialise server", zap.Error(err))
	}

	go srv.StartAuthCleanup(rootCtx)

	// Run in a goroutine so we can listen for signals concurrently.
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Start()
	}()

	// ── 8. Wait for shutdown signal ───────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		logger.Info("shutdown signal received", zap.String("signal", sig.String()))
	case err := <-serverErr:
		if err != nil {
			logger.Error("server exited with error", zap.Error(err))
		}
	}

	// ── 9. Graceful shutdown ─────────────────────────────────────────────────
	rootCancel() // stop health checks and auth cleanup

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)

	if err := srv.Stop(shutdownCtx); err != nil {
		logger.Error("error during graceful shutdown", zap.Error(err))
		shutdownCancel()
		_ = logger.Sync()
		os.Exit(1)
	}

	shutdownCancel()
	logger.Info("capp-backend stopped")
	_ = logger.Sync()
}

// buildLogger constructs a zap.Logger from the logging config.
// JSON format is used in production; console format is used in development.
func buildLogger(cfg *config.Config) (*zap.Logger, error) {
	level, err := zapcore.ParseLevel(cfg.Logging.Level)
	if err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", cfg.Logging.Level, err)
	}

	var zapCfg zap.Config
	if cfg.Logging.Format == "console" {
		zapCfg = zap.NewDevelopmentConfig()
	} else {
		zapCfg = zap.NewProductionConfig()
	}

	zapCfg.Level = zap.NewAtomicLevelAt(level)
	zapCfg.DisableCaller = !cfg.Logging.AddCallerInfo

	return zapCfg.Build()
}
