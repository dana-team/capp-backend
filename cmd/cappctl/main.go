package main

import (
	"os"

	"github.com/dana-team/capp-backend/internal/cli/auth"
	"github.com/dana-team/capp-backend/internal/cli/capps"
	"github.com/dana-team/capp-backend/internal/cli/completion"
	"github.com/dana-team/capp-backend/internal/cli/resource"
	"github.com/dana-team/capp-backend/internal/cli/root"
)

// version is set at build time via -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	state := &root.State{}

	registry := resource.NewRegistry()
	registry.Register(capps.New(state))

	rootCmd := root.New(state, registry)
	rootCmd.Version = version
	rootCmd.AddCommand(
		auth.NewLoginCommand(state),
		auth.NewLogoutCommand(state),
		auth.NewContextCommand(state),
		completion.NewCompletionCommand(),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
