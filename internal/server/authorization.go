package server

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/cluster/namespaces"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"k8s.io/klog/v2"
)

// authorizationRuntime owns observation independently of HTTP request lifetimes.
// Server.Stop cancels observation after HTTP shutdown returns, then waits within
// the remaining deadline.
type authorizationRuntime struct {
	targets        map[string]policy.Target
	administration map[string]*namespaces.Administration
	workers        []func(context.Context) error
	cancel         context.CancelFunc
	done           chan struct{}
	reconcilers    []*authorization.Reconciler
}

func newAuthorization(cfg *config.Config, mgr cluster.ClusterManager) (_ *authorizationRuntime, err error) {
	a := &authorizationRuntime{
		targets:        make(map[string]policy.Target),
		administration: make(map[string]*namespaces.Administration),
	}
	// Queue construction starts client-go background work even before Run.
	defer func() {
		if err != nil {
			for _, reconciler := range a.reconcilers {
				reconciler.Close()
			}
		}
	}()
	for _, target := range cfg.Clusters {
		cc, err := mgr.Get(target.Name)
		if err != nil {
			return nil, err
		}
		system, err := cluster.NewSystemClients(cc)
		if err != nil {
			return nil, fmt.Errorf("cluster %q system clients: %w", target.Name, err)
		}
		opts := authorization.StoreOptions{
			PolicyNamespace:               target.Authorization.PolicyNamespace,
			ProtectedNamespaces:           target.Authorization.ProtectedNamespaces,
			RestrictedBindingNamePrefixes: cfg.Authorization.RestrictedBindingNamePrefixes,
		}
		store, err := authorization.NewStore(system.Kubernetes, opts)
		if err != nil {
			return nil, fmt.Errorf("cluster %q observation: %w", target.Name, err)
		}
		reconciler, err := authorization.NewReconciler(system.Kubernetes, store)
		if err != nil {
			return nil, err
		}
		a.reconcilers = append(a.reconcilers, reconciler)
		administration, err := namespaces.NewAdministration(system.Resources, opts.PolicyNamespace, opts.ProtectedNamespaces, opts.RestrictedBindingNamePrefixes)
		if err != nil {
			return nil, err
		}
		a.administration[target.Name] = administration
		a.targets[target.Name] = policy.Target{Store: store, ExecutionClient: system.Resources, OperatorUsers: target.Authorization.OperatorUsers, OperatorGroups: target.Authorization.OperatorGroups}
		a.workers = append(a.workers, store.Run, reconciler.Run)
	}
	return a, nil
}

func (a *authorizationRuntime) start(logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = klog.NewContext(ctx, zapr.NewLogger(logger))
	a.cancel, a.done = cancel, make(chan struct{})
	var workers sync.WaitGroup
	for _, run := range a.workers {
		workers.Go(func() {
			if err := run(ctx); err != nil {
				logger.Error("authorization worker stopped")
				cancel()
			}
		})
	}
	workers.Go(func() { a.observeHealth(ctx, logger) })
	go func() { workers.Wait(); close(a.done) }()
}

func (a *authorizationRuntime) stop(ctx context.Context) error {
	a.cancel()
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *authorizationRuntime) ready(mgr cluster.ClusterManager) bool {
	for _, meta := range mgr.List() {
		if target, ok := a.targets[meta.Name]; ok && meta.Healthy && target.Store.Health().Synced {
			return true
		}
	}
	return false
}

func (a *authorizationRuntime) observeHealth(ctx context.Context, logger *zap.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	previous := make(map[string][]string)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for name, target := range a.targets {
				degraded := target.Store.Health().Degraded
				if slices.Equal(previous[name], degraded) {
					continue
				}
				if len(degraded) != 0 {
					logger.Warn("authorization observation degraded", zap.String("cluster", name), zap.Strings("collections", degraded))
				} else {
					logger.Info("authorization observation recovered", zap.String("cluster", name))
				}
				previous[name] = degraded
			}
		}
	}
}
