package auth

import (
	"bufio"
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/spf13/cobra"

	"github.com/dana-team/capp-backend/internal/cli/client"
)

// promptCredentials prompts for missing username and/or password interactively.
func promptCredentials(cmd *cobra.Command, username, password string) (string, string, error) {
	if username == "" {
		fmt.Fprint(cmd.OutOrStdout(), "Username: ") //nolint:errcheck
		scanner := bufio.NewScanner(cmd.InOrStdin())
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", "", fmt.Errorf("reading input: %w", err)
			}
			return "", "", fmt.Errorf("unexpected EOF reading input")
		}
		username = strings.TrimSpace(scanner.Text())
	}
	if password == "" {
		fmt.Fprint(cmd.OutOrStdout(), "Password: ") //nolint:errcheck
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.OutOrStdout()) //nolint:errcheck
		if err != nil {
			return "", "", fmt.Errorf("reading password: %w", err)
		}
		password = string(pw)
	}
	return username, password, nil
}

// loginWithPassword exchanges username+password credentials for a token pair.
func loginWithPassword(ctx context.Context, c *client.Client, username, password string) (client.TokenPair, error) {
	var pair client.TokenPair
	if err := c.Post(ctx, "/api/v1/auth/login", map[string]string{
		"username": username,
		"password": password,
	}, &pair); err != nil {
		return pair, fmt.Errorf("login failed: %w", err)
	}
	return pair, nil
}

// startCallbackServer starts a local HTTP server on a random port to receive
// the OAuth callback. Returns channels for the code/error, the redirect URI,
// and a stop function. The caller must call stop() when done.
func startCallbackServer() (<-chan string, <-chan error, string, func(), error) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("starting local callback server: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			desc := r.URL.Query().Get("error_description")
			fmt.Fprintf(w, "<html><body><h2>Authentication failed</h2><p>%s: %s</p><p>You may close this tab.</p></body></html>", //nolint:errcheck
				html.EscapeString(errParam), html.EscapeString(desc))
			once.Do(func() { errCh <- fmt.Errorf("OAuth error %q: %s", errParam, desc) })
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			fmt.Fprint(w, "<html><body><h2>Authentication failed</h2><p>No authorization code received.</p><p>You may close this tab.</p></body></html>") //nolint:errcheck
			once.Do(func() { errCh <- fmt.Errorf("no authorization code in callback") })
			return
		}
		fmt.Fprint(w, "<html><body><h2>Login successful</h2><p>You may close this tab and return to the terminal.</p></body></html>") //nolint:errcheck
		once.Do(func() { codeCh <- code })
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln) //nolint:errcheck

	return codeCh, errCh, redirectURI, func() { srv.Close() }, nil //nolint:errcheck
}

// awaitOAuthCode waits for the authorization code from the callback server,
// an OAuth error, a timeout, or context cancellation.
func awaitOAuthCode(ctx context.Context, codeCh <-chan string, errCh <-chan error) (string, error) {
	select {
	case code := <-codeCh:
		return code, nil
	case err := <-errCh:
		return "", err
	case <-time.After(5 * time.Minute):
		return "", fmt.Errorf("timed out waiting for browser authentication")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
