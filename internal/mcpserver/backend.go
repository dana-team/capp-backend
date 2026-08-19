package mcpserver

import (
	"context"
	"errors"

	"github.com/dana-team/capp-backend/internal/cli/client"
)

var ErrNoBearerToken = errors.New("no bearer token on this session: connect with an Authorization header carrying a capp-backend access token")

type bearerTokenKey struct{}

// WithBearerToken returns ctx annotated with the bearer token extracted from
// the incoming MCP session's Authorization header.
func WithBearerToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, bearerTokenKey{}, token)
}

func bearerTokenFromContext(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(bearerTokenKey{}).(string)
	return token, ok && token != ""
}

// Backend holds the fixed capp-backend connection settings shared by every
// tool set. each tool handler builds a client scoped to the bearer token carried on its call's
// context.
type Backend struct {
	BaseURL string

	Insecure bool
}

func (b *Backend) Client(ctx context.Context) (*client.Client, error) {
	token, ok := bearerTokenFromContext(ctx)
	if !ok {
		return nil, ErrNoBearerToken
	}
	return client.New(b.BaseURL, token, b.Insecure), nil
}
