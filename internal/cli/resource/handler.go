package resource

import "github.com/spf13/cobra"

// ResourceCommand is implemented by each resource type the CLI exposes.
// Adding a new resource: implement this interface and call registry.Register
// in cmd/cappctl/main.go. No other files need to change.
type ResourceCommand interface {
	// Name returns the resource argument (e.g. "capps").
	Name() string
	// Aliases returns accepted short-forms (e.g. ["capp", "ca"]).
	Aliases() []string
	// RegisterGetCommand attaches the get sub-command to parent.
	RegisterGetCommand(parent *cobra.Command)
	// RegisterCreateCommand attaches the create sub-command to parent.
	RegisterCreateCommand(parent *cobra.Command)
	// RegisterUpdateCommand attaches the update sub-command to parent.
	RegisterUpdateCommand(parent *cobra.Command)
	// RegisterDeleteCommand attaches the delete sub-command to parent.
	RegisterDeleteCommand(parent *cobra.Command)
	// RegisterSyncCommand attaches the sync sub-command to parent.
	RegisterSyncCommand(parent *cobra.Command)
}

// Registry holds all registered ResourceCommands.
type Registry struct {
	handlers []ResourceCommand
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds h to the registry.
func (r *Registry) Register(h ResourceCommand) {
	r.handlers = append(r.handlers, h)
}

// MountAll attaches each handler's commands to the five verb parents.
func (r *Registry) MountAll(get, create, update, delete, sync *cobra.Command) {
	for _, h := range r.handlers {
		h.RegisterGetCommand(get)
		h.RegisterCreateCommand(create)
		h.RegisterUpdateCommand(update)
		h.RegisterDeleteCommand(delete)
		h.RegisterSyncCommand(sync)
	}
}
