// Command server is the entry point for the capp-backend HTTP server.
//
// It performs the following startup sequence:
//  1. Parse flags and load configuration.
//  2. Validate configuration.
//  3. Initialise the structured logger.
//  4. Build the Kubernetes scheme (registers all API types).
//  5. Build the ClusterManager (connects to all configured clusters).
//  6. Build the AuthManager based on auth.mode.
//  7. Build and populate the resource handler registry.
//  8. Build the HTTP Server and start listening.
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

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources"
	capphandler "github.com/dana-team/capp-backend/internal/resources/capps"
	nshandler "github.com/dana-team/capp-backend/internal/resources/namespaces"
	"github.com/dana-team/capp-backend/internal/server"
	"github.com/dana-team/capp-backend/pkg/k8s"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// cleanupStarter is implemented by auth managers that run a background
// session garbage collector (jwt and dex modes).
type cleanupStarter interface {
	StartCleanup(context.Context)
}

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
	defer logger.Sync() //nolint:errcheck

	logger.Info("capp-backend starting",
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
	defer rootCancel()
	go clusterMgr.StartHealthChecks(rootCtx, 30)

	// ── 5. Build AuthManager ──────────────────────────────────────────────────
	authMgr, err := auth.New(cfg)
	if err != nil {
		logger.Fatal("failed to initialise auth manager", zap.Error(err))
	}

	// In JWT and dex modes, start the session garbage collector.
	if cs, ok := authMgr.(cleanupStarter); ok {
		go cs.StartCleanup(rootCtx)
	}

	// ── 6. Build resource registry ────────────────────────────────────────────
	enabledResources := map[string]bool{
		"namespaces": cfg.Resources.Namespaces.Enabled,
		"capps":      cfg.Resources.Capps.Enabled,
	}
	registry := resources.NewRegistry(enabledResources)
	registry.Register(nshandler.New())
	registry.Register(capphandler.New())

	// ── 7. Build and start HTTP server ────────────────────────────────────────
	srv := server.New(cfg, authMgr, clusterMgr, registry, logger)

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

	// ── 9. Graceful shutdown ──────────────────────────────────────────────────
	rootCancel() // stop health checks and JWT cleanup

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Stop(shutdownCtx); err != nil {
		logger.Error("error during graceful shutdown", zap.Error(err))
		os.Exit(1)
	}

	logger.Info("capp-backend stopped")
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
