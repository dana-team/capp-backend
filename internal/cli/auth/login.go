package auth

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/term"

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
	cmd.Flags().StringVar(&username, "username", "", "username (dex mode)")
	cmd.Flags().StringVar(&password, "password", "", "password (dex mode)")
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
		if username == "" {
			fmt.Fprint(cmd.OutOrStdout(), "Username: ") //nolint:errcheck
			scanner := bufio.NewScanner(cmd.InOrStdin())
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return ctx, fmt.Errorf("reading input: %w", err)
				}
				return ctx, fmt.Errorf("unexpected EOF reading input")
			}
			username = strings.TrimSpace(scanner.Text())
		}
		if password == "" {
			fmt.Fprint(cmd.OutOrStdout(), "Password: ") //nolint:errcheck
			pw, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(cmd.OutOrStdout()) //nolint:errcheck
			if err != nil {
				return ctx, fmt.Errorf("reading password: %w", err)
			}
			password = string(pw)
		}
		c := client.New(server, "", insecure)
		var pair client.TokenPair
		if err := c.Post(cmd.Context(), "/api/v1/auth/login", map[string]string{
			"username": username,
			"password": password,
		}, &pair); err != nil {
			return ctx, fmt.Errorf("login failed: %w", err)
		}
		ctx.Token = pair.AccessToken
		ctx.RefreshToken = pair.RefreshToken
		ctx.TokenExpiresAt = pair.ExpiresAt
		return ctx, nil

	case "openshift":
		return loginOpenShift(cmd, server, insecure, ctx)

	default:
		return ctx, fmt.Errorf("unsupported auth mode %q", authMode)
	}
}

func loginOpenShift(cmd *cobra.Command, server string, insecure bool, ctx config.Context) (config.Context, error) {
	c := client.New(server, "", insecure)
	var authResp struct {
		AuthorizeURL string `json:"authorizeUrl"`
	}
	if err := c.Get(cmd.Context(), "/api/v1/auth/openshift/authorize", &authResp); err != nil {
		return ctx, fmt.Errorf("getting authorize URL: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Open this URL in your browser to authenticate:\n\n  %s\n\n", authResp.AuthorizeURL) //nolint:errcheck

	if openBrowser(authResp.AuthorizeURL) {
		fmt.Fprintln(cmd.OutOrStdout(), "Browser opened. After authenticating, copy the authorization code from the redirect URL.") //nolint:errcheck
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "Copy the URL into your browser. After authenticating, copy the authorization code from the redirect URL.") //nolint:errcheck
	}

	fmt.Fprint(cmd.OutOrStdout(), "Authorization code: ") //nolint:errcheck
	scanner := bufio.NewScanner(cmd.InOrStdin())
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return ctx, fmt.Errorf("reading input: %w", err)
		}
		return ctx, fmt.Errorf("unexpected EOF reading input")
	}
	code := strings.TrimSpace(scanner.Text())
	if code == "" {
		return ctx, fmt.Errorf("authorization code is required")
	}

	var pair client.TokenPair
	if err := c.Post(cmd.Context(), "/api/v1/auth/openshift/callback", map[string]string{"code": code}, &pair); err != nil {
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
		cmd = exec.Command("xdg-open", "--", rawURL)
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
