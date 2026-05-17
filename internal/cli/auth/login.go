package auth

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/config"
	"github.com/dana-team/capp-backend/internal/cli/root"
)

// NewLoginCommand returns the `cappctl login` command.
func NewLoginCommand(state *root.State) *cobra.Command {
	var (
		authMode string
		username string
		password string
	)

	cmd := &cobra.Command{
		Use:         "login",
		Short:       "Authenticate and save credentials to a named context",
		Annotations: root.SkipAuthAnnotation(),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Read connection flags from inherited root persistent flags.
			server, _ := cmd.Flags().GetString("server")
			ctxName, _ := cmd.Flags().GetString("context")
			cluster, _ := cmd.Flags().GetString("cluster")
			token, _ := cmd.Flags().GetString("token")
			insecure, _ := cmd.Flags().GetBool("insecure")

			if server == "" {
				return fmt.Errorf("--server is required")
			}
			if ctxName == "" {
				return fmt.Errorf("--context is required")
			}
			if authMode == "" {
				detected, err := detectAuthMode(cmd.Context(), server, insecure)
				if err != nil {
					return fmt.Errorf("detecting auth mode: %w", err)
				}
				authMode = detected
				fmt.Fprintf(cmd.OutOrStdout(), "Detected auth mode: %s\n", authMode) //nolint:errcheck
			}

			ctx, err := login(cmd, server, authMode, cluster, token, username, password, insecure)
			if err != nil {
				return err
			}
			ctx.Name = ctxName
			ns, _ := cmd.Flags().GetString("namespace")
			ctx.Namespace = ns

			state.Cfg.UpsertContext(ctx)
			state.Cfg.CurrentContext = ctxName
			if err := config.Save(state.CfgPath, state.Cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged in. Context %q saved and set as current.\n", ctxName) //nolint:errcheck
			return nil
		},
	}

	// Only flags not already defined as root persistent flags.
	cmd.Flags().StringVar(&authMode, "auth-mode", "", "auth mode: jwt|dex|openshift|static|passthrough (auto-detected if omitted)")
	cmd.Flags().StringVar(&username, "username", "", "username (dex or openshift mode)")
	cmd.Flags().StringVar(&password, "password", "", "password (dex or openshift mode)")
	cmd.Flags().MarkHidden("password") //nolint:errcheck

	return cmd
}

func detectAuthMode(ctx context.Context, server string, insecure bool) (string, error) {
	c := client.New(server, "", insecure)
	var resp struct {
		Mode string `json:"mode"`
	}
	if err := c.Get(ctx, "/api/v1/auth/mode", &resp); err != nil {
		return "", err
	}
	return resp.Mode, nil
}

func login(cmd *cobra.Command, server, authMode, cluster, token, username, password string, insecure bool) (config.Context, error) {
	ctx := config.Context{
		Server:   server,
		AuthMode: authMode,
		Cluster:  cluster,
	}

	switch authMode {
	case "passthrough", "static":
		if token == "" {
			return ctx, fmt.Errorf("--token is required for %s mode", authMode)
		}
		ctx.Token = token
		return ctx, nil

	case "jwt":
		if token == "" {
			return ctx, fmt.Errorf("--token is required for %s mode", authMode)
		}
		if cluster == "" {
			return ctx, fmt.Errorf("--cluster is required for %s mode", authMode)
		}
		c := client.New(server, "", insecure)
		var pair client.TokenPair
		if err := c.Post(cmd.Context(), "/api/v1/auth/login", map[string]string{
			"cluster": cluster,
			"token":   token,
		}, &pair); err != nil {
			return ctx, fmt.Errorf("login failed: %w", err)
		}
		ctx.Token = pair.AccessToken
		ctx.RefreshToken = pair.RefreshToken
		ctx.TokenExpiresAt = pair.ExpiresAt
		return ctx, nil

	case "dex":
		var err error
		username, password, err = promptCredentials(cmd, username, password)
		if err != nil {
			return ctx, err
		}
		c := client.New(server, "", insecure)
		pair, err := loginWithPassword(cmd.Context(), c, username, password)
		if err != nil {
			return ctx, err
		}
		ctx.Token = pair.AccessToken
		ctx.RefreshToken = pair.RefreshToken
		ctx.TokenExpiresAt = pair.ExpiresAt
		return ctx, nil

	case "openshift":
		// Direct token: store as-is, no OAuth flow needed. OpenShift mode is
		// stateless — the backend validates via TokenReview (with a short
		// in-memory cache) on each request.
		if token != "" {
			ctx.Token = token
			return ctx, nil
		}
		return loginOpenShift(cmd, server, insecure, ctx, username, password)

	default:
		return ctx, fmt.Errorf("unsupported auth mode %q", authMode)
	}
}

func loginOpenShift(cmd *cobra.Command, server string, insecure bool, ctx config.Context, username, password string) (config.Context, error) {
	c := client.New(server, "", insecure)

	// Password path: skip the browser flow entirely.
	if username != "" || password != "" {
		var err error
		username, password, err = promptCredentials(cmd, username, password)
		if err != nil {
			return ctx, err
		}
		pair, err := loginWithPassword(cmd.Context(), c, username, password)
		if err != nil {
			return ctx, err
		}
		ctx.Token = pair.AccessToken
		ctx.RefreshToken = pair.RefreshToken
		ctx.TokenExpiresAt = pair.ExpiresAt
		return ctx, nil
	}

	// OAuth browser flow with local callback server.
	resultCh, errCh, redirectURI, stop, err := startCallbackServer()
	if err != nil {
		return ctx, err
	}
	defer stop()

	var authResp struct {
		AuthorizeURL string `json:"authorizeUrl"`
		State        string `json:"state"`
	}
	authorizeEndpoint := "/api/v1/auth/openshift/authorize?redirect_uri=" + url.QueryEscape(redirectURI)
	if err := c.Get(cmd.Context(), authorizeEndpoint, &authResp); err != nil {
		return ctx, fmt.Errorf("getting authorize URL: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Opening browser for authentication...\n\n  %s\n\n", authResp.AuthorizeURL) //nolint:errcheck
	if !openBrowser(authResp.AuthorizeURL) {
		fmt.Fprintf(cmd.OutOrStdout(), "Could not open browser. Visit the URL above manually.\n") //nolint:errcheck
	}

	cbResult, err := awaitOAuthCallback(cmd.Context(), resultCh, errCh)
	if err != nil {
		return ctx, err
	}

	// Use the state from the authorize response. The OAuth server echoes it
	// back in the redirect; if they don't match, the backend will reject it.
	state := authResp.State
	if cbResult.State != "" {
		state = cbResult.State
	}

	var pair client.TokenPair
	if err := c.Post(cmd.Context(), "/api/v1/auth/openshift/callback", map[string]string{
		"code":        cbResult.Code,
		"state":       state,
		"redirectUri": redirectURI,
	}, &pair); err != nil {
		return ctx, fmt.Errorf("exchanging code: %w", err)
	}
	ctx.Token = pair.AccessToken
	ctx.RefreshToken = pair.RefreshToken
	ctx.TokenExpiresAt = pair.ExpiresAt
	return ctx, nil
}

func openBrowser(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	go cmd.Wait() //nolint:errcheck
	return true
}
