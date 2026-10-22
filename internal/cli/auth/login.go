package auth

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/config"
	"github.com/dana-team/capp-backend/internal/cli/root"
)

// NewLoginCommand returns the `cappctl login` command.
func NewLoginCommand(state *root.State) *cobra.Command {
	var (
		authMode      string
		username      string
		password      string
		passwordStdin bool
	)

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate and save credentials to a named context",
		Long: `Authenticate using openshift, passthrough, or ldap mode.

LDAP requires certificate-verified HTTPS and does not allow --insecure.
Use --username with --password-stdin for unattended LDAP login, or --token
to import an access token with a future numeric JWT exp claim. Imported expiry
is local metadata only; the backend verifies the token when it is used.
LDAP contexts save the access token and expiry in a 0600 config file, with no
password or refresh credentials. Repeat login to obtain a new token.
LDAP credential requests never follow redirects.`,
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
			if cmd.Flags().Changed("password-stdin") && authMode != "ldap" {
				return fmt.Errorf("--password-stdin is only supported in ldap mode")
			}

			ctx, err := login(cmd, server, authMode, cluster, token, username, password, insecure)
			if err != nil {
				return err
			}
			ctx.Name = ctxName
			ns, _ := cmd.Flags().GetString("namespace")
			ctx.Namespace = ns

			cfg := *state.Cfg
			cfg.Contexts = slices.Clone(state.Cfg.Contexts)
			cfg.UpsertContext(ctx)
			cfg.CurrentContext = ctxName
			if err := config.Save(state.CfgPath, &cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			*state.Cfg = cfg
			fmt.Fprintf(cmd.OutOrStdout(), "Logged in. Context %q saved and set as current.\n", ctxName) //nolint:errcheck
			return nil
		},
	}

	// Only flags not already defined as root persistent flags.
	cmd.Flags().StringVar(&authMode, "auth-mode", "", "auth mode: openshift|passthrough|ldap (auto-detected if omitted)")
	cmd.Flags().StringVar(&username, "username", "", "username (openshift mode; required with --password-stdin in ldap mode)")
	cmd.Flags().StringVar(&password, "password", "", "password (openshift mode)")
	cmd.Flags().MarkHidden("password") //nolint:errcheck
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read LDAP password from stdin (UTF-8, at most 16 KiB; removes one trailing LF/CRLF)")

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
	case "passthrough":
		if token == "" {
			return ctx, fmt.Errorf("--token is required for %s mode", authMode)
		}
		ctx.Token = token
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

	case "ldap":
		return loginLDAP(cmd, ctx, token, username, insecure)

	default:
		return ctx, fmt.Errorf("unsupported auth mode %q", authMode)
	}
}

func loginLDAP(cmd *cobra.Command, ctx config.Context, token, username string, insecure bool) (config.Context, error) {
	u, err := url.Parse(ctx.Server)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return ctx, fmt.Errorf("LDAP login requires a certificate-verified HTTPS server")
	}
	if insecure {
		return ctx, fmt.Errorf("--insecure is not allowed in ldap mode")
	}
	if cmd.Flags().Changed("token") {
		if cmd.Flags().Changed("username") || cmd.Flags().Changed("password") || cmd.Flags().Changed("password-stdin") {
			return ctx, fmt.Errorf("--token cannot be combined with --username, --password, or --password-stdin in ldap mode")
		}
		expiresAt, err := importedLDAPTokenExpiry(token)
		if err != nil {
			return ctx, err
		}
		ctx.Token = token
		ctx.TokenExpiresAt = expiresAt
		return ctx, nil
	}
	if cmd.Flags().Changed("password") {
		return ctx, fmt.Errorf("--password is not allowed in ldap mode; use --password-stdin")
	}
	if username == "" {
		return ctx, fmt.Errorf("--username is required for LDAP password login")
	}
	passwordStdin, _ := cmd.Flags().GetBool("password-stdin")
	if !passwordStdin {
		return ctx, fmt.Errorf("--password-stdin is required for LDAP password login")
	}
	password, err := readPasswordStdin(cmd.InOrStdin())
	if err != nil {
		return ctx, err
	}
	c := client.New(ctx.Server, "", false).WithoutRedirects()
	pair, err := loginWithPassword(cmd.Context(), c, username, password)
	if err != nil {
		return ctx, err
	}
	if strings.TrimSpace(pair.AccessToken) == "" || pair.ExpiresAt.IsZero() {
		return ctx, fmt.Errorf("LDAP login response requires an access token and expiry")
	}
	if err := validateLDAPTokenExpiry(pair.ExpiresAt); err != nil {
		return ctx, err
	}
	ctx.Token = pair.AccessToken
	ctx.TokenExpiresAt = pair.ExpiresAt
	return ctx, nil
}

func readPasswordStdin(r io.Reader) (string, error) {
	const maxPasswordInput = 16 << 10
	raw, err := io.ReadAll(io.LimitReader(r, maxPasswordInput+1))
	if err != nil {
		return "", fmt.Errorf("reading password from stdin: %w", err)
	}
	if len(raw) > maxPasswordInput {
		return "", fmt.Errorf("password stdin exceeds the 16 KiB limit")
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("password stdin must be valid UTF-8")
	}
	password, newline := strings.CutSuffix(string(raw), "\n")
	if newline {
		password = strings.TrimSuffix(password, "\r")
	}
	if password == "" {
		return "", fmt.Errorf("password stdin must not be empty")
	}
	return password, nil
}

// importedLDAPTokenExpiry reads untrusted expiry metadata; the backend alone
// authenticates the token when it is used for a request.
func importedLDAPTokenExpiry(raw string) (time.Time, error) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(raw, claims); err != nil {
		return time.Time{}, fmt.Errorf("invalid LDAP access token: %w", err)
	}
	expiresAt, err := claims.GetExpirationTime()
	if err != nil || expiresAt == nil || expiresAt.Unix() <= 0 {
		return time.Time{}, fmt.Errorf("LDAP access token requires a usable positive numeric exp claim")
	}
	if err := validateLDAPTokenExpiry(expiresAt.Time); err != nil {
		return time.Time{}, err
	}
	return expiresAt.Time, nil
}

func validateLDAPTokenExpiry(expiresAt time.Time) error {
	// Context timestamps must round-trip through the existing YAML persistence.
	if _, err := expiresAt.MarshalText(); err != nil {
		return fmt.Errorf("LDAP access token expiry cannot be stored: %w", err)
	}
	if !time.Now().Before(expiresAt) {
		return fmt.Errorf("LDAP access token expired; run cappctl login again")
	}
	return nil
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
		cmd = exec.CommandContext(context.Background(), "xdg-open", rawURL)
	case "windows":
		cmd = exec.CommandContext(context.Background(), "rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	go cmd.Wait() //nolint:errcheck
	return true
}
