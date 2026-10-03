// Command smart-gateway-panel runs the control plane.
//
// The panel stores the desired topology and serves rendered configurations to
// the agents that implement it. It is the only component an operator needs to
// reach directly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/uio-o/smart-gateway/internal/panel"
	"github.com/uio-o/smart-gateway/internal/store"
)

// version is the build version reported by -version. It is a variable rather
// than a constant so the linker can stamp the release tag into the binary
// (-ldflags "-X main.version=..."). A constant would silently ignore that.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "smart-gateway-panel: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr        = flag.String("addr", ":8090", "address to listen on")
		dataDir     = flag.String("data", "./data", "directory holding the panel database")
		tokenFile   = flag.String("token-file", "", "file containing the panel admin token")
		mountPrefix = flag.String("mount-prefix", "", "optional URL prefix to serve the panel under")
		logLevel    = flag.String("log-level", "info", "debug, info, warn or error")
		showVersion = flag.Bool("version", false, "print the version and exit")
		healthURL   = flag.String("healthcheck-url", "", "if set, probe this URL and exit")
		agentDir    = flag.String("agent-dir", "", "directory holding agent binaries served to nodes")
		releaseBase = flag.String("release-base", "", "origin prefix agent binaries are proxied from")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	if *healthURL != "" {
		return runHealthcheck(*healthURL)
	}

	// The admin token is read from a file rather than a flag so that it does
	// not appear in the process list.
	token, err := resolveAdminToken(*tokenFile)
	if err != nil {
		return err
	}

	dbPath := filepath.Join(*dataDir, "panel.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	srv, err := panel.New(panel.Options{
		Store:       st,
		AdminToken:  token,
		MountPrefix: *mountPrefix,
		AgentDir:    *agentDir,
		ReleaseBase: *releaseBase,
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("panel listening", "addr", *addr, "data", dbPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("panel server: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown incomplete", "error", err)
			_ = httpServer.Close()
		}
		return nil
	}
}

// resolveAdminToken loads the operator credential.
//
// Only a file or the environment is accepted. Refusing to start without a
// token is deliberate: an unauthenticated panel can reconfigure every agent.
func resolveAdminToken(path string) (string, error) {
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("token file %s is empty", path)
		}
		return token, nil
	}
	if token := strings.TrimSpace(os.Getenv("PANEL_ADMIN_TOKEN")); token != "" {
		return token, nil
	}
	return "", errors.New("a panel admin token is required: set PANEL_ADMIN_TOKEN or pass -token-file")
}

// runHealthcheck probes the panel and exits, for container health checks.
func runHealthcheck(url string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("healthcheck returned %d", resp.StatusCode)
	}
	return nil
}

// newLogger builds a text logger at the requested level.
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
