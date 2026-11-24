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
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/resources"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	capphandler "github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	revhandler "github.com/dana-team/capp-backend/internal/resources/namespaced/capps/revisions"
	functionhandler "github.com/dana-team/capp-backend/internal/resources/namespaced/cfunc/functions"
	poolhandler "github.com/dana-team/capp-backend/internal/resources/namespaced/cfunc/pools"
	cmhandler "github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	secrethandler "github.com/dana-team/capp-backend/internal/resources/namespaced/secrets"
	"github.com/dana-team/capp-backend/internal/resources/tracking"
	"github.com/dana-team/capp-backend/internal/server"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	// ── 5. Connect the desired state store (optional) ────────────────────────
	if cfg.GitSyncConfigured {
		logger.Warn("gitops.enabled is ignored: Git sync was removed and Capps are no longer backed up to Git; see \"Moving off Git sync\" in docs/desired-state.md",
			zap.Bool("desiredStateEnabled", cfg.DesiredState.Enabled),
		)
	}
	tracker, closeTracker, err := buildTracker(rootCtx, cfg.DesiredState, logger)
	if err != nil {
		logger.Fatal("failed to initialise desired state", zap.Error(err))
	}

	// ── 6. Build resource registry ────────────────────────────────────────────
	functionsEnabled := cfg.Resources.Cfunc.Enabled && cfg.Resources.Cfunc.Functions.Enabled
	enabledResources := map[string]bool{
		"namespaces":     cfg.Resources.Namespaces.Enabled,
		"capps":          cfg.Resources.Capps.Enabled,
		"capprevisions":  cfg.Resources.Capprevisions.Enabled,
		"configmaps":     cfg.Resources.Configmaps.Enabled,
		"secrets":        cfg.Resources.Secrets.Enabled,
		"cfuncpools":     cfg.Resources.Cfunc.Enabled,
		"cfuncfunctions": functionsEnabled,
	}
	registry := resources.NewRegistry(enabledResources)
	if unscoped := cfg.Resources.Observability.MetricsWithoutClusterLabel(); len(unscoped) > 0 {
		logger.Warn("observability metrics have no clusterLabel, so their queries match every cluster in the Prometheus at queryURL; "+
			"if that Prometheus also holds other clusters, set clusterLabel or pin it with resources.observability.labels",
			zap.Strings("metrics", unscoped),
		)
	}
	cappHandler, err := capphandler.New(clusterMgr, cfg.Sizes, cfg.Resources.Observability)
	if err != nil {
		logger.Fatal("failed to initialise capps handler", zap.Error(err))
	}
	registry.Register(cappHandler.WithMaxLogStreams(cfg.Resources.Capps.MaxLogStreams).WithTracker(tracker))
	registry.Register(revhandler.New(cfg.Sizes).WithTracker(tracker))
	registry.Register(cmhandler.New().WithFunctionGuard(functionsEnabled).WithTracker(tracker))
	registry.Register(secrethandler.New().WithFunctionGuard(functionsEnabled).WithTracker(tracker))
	registry.Register(tracking.NewAdopter(tracker, adoptedKinds(cfg.Resources)...))
	poolHandler, err := poolhandler.New(cfg.Resources.Cfunc)
	if err != nil {
		logger.Fatal("failed to initialise cfunc pools handler", zap.Error(err))
	}
	registry.Register(poolHandler)
	if functionsEnabled {
		functionHandler, err := functionhandler.New(cfg.Resources.Cfunc, cfg.Clusters)
		if err != nil {
			logger.Fatal("failed to initialise cfunc functions handler", zap.Error(err))
		}
		registry.Register(functionHandler)
	}
	// ── 7. Build and start HTTP server ────────────────────────────────────────
	srv, err := server.New(cfg, version, nil, clusterMgr, registry, logger, server.WithTracker(tracker))
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
	closeTracker()
	logger.Info("capp-backend stopped")
	_ = logger.Sync()
}

// buildTracker connects the desired state store. It returns a nil tracker when
// desired state is disabled. MongoDB being down does not fail the startup.
func buildTracker(ctx context.Context, cfg config.DesiredStateConfig, logger *zap.Logger) (*tracking.Tracker, func(), error) {
	if !cfg.Enabled {
		return nil, func() {}, nil
	}
	keys, err := desiredstate.ParseKeys(cfg.Encryption.Keys)
	if err != nil {
		return nil, nil, err
	}
	sealer, err := desiredstate.NewSealer(cfg.Encryption.ActiveKeyID, keys)
	if err != nil {
		return nil, nil, err
	}
	timeout := time.Duration(cfg.Mongo.TimeoutSeconds) * time.Second
	store, err := desiredstate.NewMongoStore(cfg.Mongo.URI, cfg.Mongo.Database, timeout)
	if err != nil {
		return nil, nil, err
	}
	indexCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := store.EnsureIndexes(indexCtx); err != nil {
		logger.Warn("desired state store is not ready; changes are not backed up until it is", zap.Error(err))
	} else {
		logger.Info("desired state store initialised", zap.String("database", cfg.Mongo.Database))
	}
	return tracking.New(desiredstate.WithCooldown(store, desiredstate.Cooldown), sealer, timeout), func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_ = store.Close(closeCtx)
	}, nil
}

// adoptedKinds lists what the adopt endpoint covers: every tracked kind whose
// handler is enabled.
func adoptedKinds(cfg config.ResourcesConfig) []tracking.AdoptKind {
	managed := []client.ListOption{client.MatchingLabels{consts.ManagedLabelKey: consts.ManagedLabelValue}}
	var kinds []tracking.AdoptKind
	if cfg.Capps.Enabled {
		kinds = append(kinds, tracking.AdoptKind{Kind: desiredstate.KindCapp, NewList: func() client.ObjectList { return &cappv1alpha1.CappList{} }})
	}
	if cfg.Configmaps.Enabled {
		kinds = append(kinds, tracking.AdoptKind{Kind: desiredstate.KindConfigMap, NewList: func() client.ObjectList { return &corev1.ConfigMapList{} }, Options: managed})
	}
	if cfg.Secrets.Enabled {
		kinds = append(kinds, tracking.AdoptKind{Kind: desiredstate.KindSecret, NewList: func() client.ObjectList { return &corev1.SecretList{} }, Options: managed})
	}
	return kinds
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
