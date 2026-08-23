// Command capp-mcp exposes capp-backend's REST API as MCP (Model
// Context Protocol) tools over Streamable HTTP, for use by LLM agents.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dana-team/capp-backend/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	var (
		port       int
		backendURL string
		insecure   bool
	)
	flag.IntVar(&port, "port", envOrInt("CAPP_MCP_PORT", 8081), "port to listen on (env: CAPP_MCP_PORT)")
	flag.StringVar(&backendURL, "backend-url", envOr("CAPP_BACKEND_URL", "http://localhost:8080"), "capp-backend base URL (env: CAPP_BACKEND_URL)")
	flag.BoolVar(&insecure, "insecure", envOr("CAPP_MCP_INSECURE", "") == "true", "skip TLS certificate verification when calling capp-backend (env: CAPP_MCP_INSECURE=true)")
	flag.Parse()

	addr := fmt.Sprintf(":%d", port)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	mcpServer := mcpserver.NewServer(mcpserver.Config{
		Backend: mcpserver.Backend{BaseURL: backendURL, Insecure: insecure},
	})

	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{
		Logger: logger,
		// A same-pod sidecar forwards via 127.0.0.1 but preserves the external
		// Host header, so disable the SDK's loopback protection. Tool calls still
		// require a bearer token validated by capp-backend.
		DisableLocalhostProtection: true,
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.AuthMiddleware(streamable))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("capp-mcp starting", "port", port, "backendURL", backendURL)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	<-ctx.Done()

	logger.Info("capp-mcp shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
