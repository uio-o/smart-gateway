// Command smart-gateway-agent runs one node of the gateway.
//
// A node is either an entry node, which accepts client traffic and selects a
// route, or a relay node, which forwards bytes to its next hop.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
	"github.com/uio-o/smart-gateway/internal/control"
	"github.com/uio-o/smart-gateway/internal/entry"
	"github.com/uio-o/smart-gateway/internal/probe"
	"github.com/uio-o/smart-gateway/internal/relay"
)

// version is the build version reported by -version.
const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "smart-gateway-agent: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "agent.yaml", "path to the agent configuration file")
		showVersion = flag.Bool("version", false, "print the version and exit")
		printConfig = flag.Bool("print-config", false, "validate the configuration and print a summary, then exit")
		logLevel    = flag.String("log-level", "", "override the configured log level (debug, info, warn, error)")
		role        = flag.String("role", "", "role to run in managed mode: entry or relay")
		nodeName    = flag.String("node", "", "node name to register with the panel, managed mode only")
		panelURL    = flag.String("panel", "", "panel base URL, enables managed mode")
		panelToken  = flag.String("panel-token", "", "agent token issued by the panel")
		listen      = flag.String("listen", "127.0.0.1:8080", "entry listen address in managed mode")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	// Managed mode: the panel is the source of truth, so the local file is only
	// used for the bootstrap details.
	if *panelURL != "" {
		return runManaged(context.Background(), managedOptions{
			PanelURL: *panelURL,
			Token:    *panelToken,
			NodeName: *nodeName,
			Role:     *role,
			Listen:   *listen,
			Logger:   logger,
		})
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *logLevel != "" {
		cfg.Logging.Level = *logLevel
	}
	if *printConfig {
		fmt.Print(summarize(cfg))
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cfg.Role {
	case config.RoleEntry:
		return runEntry(ctx, cfg, logger)
	case config.RoleRelay:
		return runRelay(ctx, cfg, logger)
	default:
		return fmt.Errorf("unsupported role %q", cfg.Role)
	}
}

// managedOptions carries the bootstrap parameters for managed mode.
type managedOptions struct {
	PanelURL string
	Token    string
	NodeName string
	Role     string
	Listen   string
	Logger   *slog.Logger
}

// runManaged runs the agent against a panel. The panel owns the routing table,
// so every poll can swap configuration without restarting the process.
func runManaged(ctx context.Context, opts managedOptions) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	client, err := control.New(control.Options{
		PanelURL: opts.PanelURL,
		Token:    opts.Token,
		NodeName: opts.NodeName,
		Role:     opts.Role,
		Logger:   logger,
	})
	if err != nil {
		return err
	}

	logger.Info("starting in managed mode",
		"panel", opts.PanelURL, "node", opts.NodeName, "role", opts.Role, "listen", opts.Listen)

	// The runtime is built on the first successful fetch, and later fetches
	// update it in place. Holding it in a small struct keeps the apply/state
	// callbacks free of globals.
	rt := &managedRuntime{logger: logger, listen: opts.Listen}
	defer rt.close()

	return client.Run(ctx, rt.apply, rt.state)
}

// managedRuntime owns whichever server the current role needs and swaps its
// configuration in place.
type managedRuntime struct {
	mu     sync.Mutex
	logger *slog.Logger
	listen string

	entry       *entry.Server
	relay       *relay.Server
	cancelProbe context.CancelFunc

	role    string
	lastErr string
	applied int
}

// apply installs a new configuration, restarting a server only when the role
// changes. Configuration changes on an existing server are applied live.
func (r *managedRuntime) apply(cfg *config.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	role := string(cfg.Role)
	if role == "" {
		role = r.role
	}
	// The panel does not dictate a node's bind address, so an empty value means
	// "use the address this agent was started with", falling back to the local
	// default. A non-empty value is an explicit operator override.
	if cfg.Listen == "" {
		if r.listen != "" {
			cfg.Listen = r.listen
		} else {
			cfg.Listen = config.DefaultListen
		}
	}

	// A role change cannot be applied in place: tear the old server down and
	// build the other one. This is rare, so a brief gap is acceptable.
	if r.role != "" && r.role != role {
		r.closeLocked()
	}

	switch role {
	case string(config.RoleEntry):
		if r.entry == nil {
			srv, err := entry.New(entry.Options{Config: cfg, Logger: r.logger})
			if err != nil {
				r.lastErr = err.Error()
				return err
			}
			if err := serve(ctxBackground(), srv, cfg.Listen, r.logger); err != nil {
				return err
			}
			r.entry = srv
			r.restartProbeLocked(cfg, srv)
		} else if r.entry.Addr() != cfg.Listen {
			// The listen address moved, so the socket must be rebound. The old
			// one is closed first; a brief gap is unavoidable when rebinding.
			prev := r.entry
			srv, err := entry.New(entry.Options{Config: cfg, Logger: r.logger})
			if err != nil {
				r.lastErr = err.Error()
				return err
			}
			if err := serve(ctxBackground(), srv, cfg.Listen, r.logger); err != nil {
				r.lastErr = err.Error()
				return err
			}
			prev.Close()
			r.entry = srv
			r.restartProbeLocked(cfg, srv)
		} else if err := r.entry.Reload(cfg); err != nil {
			r.lastErr = err.Error()
			return err
		}
	case string(config.RoleRelay):
		if r.relay == nil {
			srv, err := relay.New(relay.Options{Config: cfg, Logger: r.logger})
			if err != nil {
				r.lastErr = err.Error()
				return err
			}
			if err := srv.Start(context.Background()); err != nil {
				return err
			}
			r.relay = srv
		} else if err := r.relay.UpdateTargets(cfg.RelayPorts); err != nil {
			r.lastErr = err.Error()
			return err
		}
	default:
		err := fmt.Errorf("unsupported role %q", role)
		r.lastErr = err.Error()
		return err
	}

	r.role = role
	r.lastErr = ""
	r.applied++
	return nil
}

// restartProbeLocked stops any running prober and starts a new one when the
// configuration enables it.
func (r *managedRuntime) restartProbeLocked(cfg *config.Config, srv *entry.Server) {
	if r.cancelProbe != nil {
		r.cancelProbe()
		r.cancelProbe = nil
	}
	if !cfg.Probe.Enabled {
		return
	}
	pctx, cancel := context.WithCancel(context.Background())
	r.cancelProbe = cancel
	p := probe.New(cfg.Probe.Timeout())
	go p.Run(pctx, srv.Config, srv.Tracker())
}

// state reports runtime health for heartbeats.
func (r *managedRuntime) state() (int64, bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var conns int64
	if r.entry != nil {
		conns = r.entry.ActiveConnections()
	}
	return conns, r.lastErr == "" && r.role != "", r.lastErr
}

// close releases the running server and any probe goroutine.
func (r *managedRuntime) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked()
}

func (r *managedRuntime) closeLocked() {
	if r.cancelProbe != nil {
		r.cancelProbe()
		r.cancelProbe = nil
	}
	if r.entry != nil {
		r.entry.Close()
		r.entry = nil
	}
	if r.relay != nil {
		if err := r.relay.Close(); err != nil {
			r.logger.Warn("closing relay server", "error", err)
		}
		r.relay = nil
	}
}

// serve starts the HTTP listener for an entry server in the background.
func serve(ctx context.Context, srv *entry.Server, addr string, logger *slog.Logger) error {
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	srv.Attach(httpServer)
	srv.SetAddr(addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	go func() {
		sc := srv.Config()
		logger.Info("entry node listening", "addr", addr, "services", len(sc.Services), "routes", len(sc.Routes))
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("entry server stopped", "error", err)
		}
	}()
	return nil
}

// ctxBackground exists so the entry server listener outlives the bootstrap
// context; shutdown is driven by Close instead.
func ctxBackground() context.Context { return context.Background() }

// runEntry starts the entry node: HTTP listener plus background probing.
func runEntry(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	srv, err := entry.New(entry.Options{Config: cfg, Logger: logger})
	if err != nil {
		return err
	}
	defer srv.Close()

	if cfg.Probe.Enabled {
		p := probe.New(cfg.Probe.Timeout())
		go p.Run(ctx, srv.Config, srv.Tracker())
		logger.Info("active probing enabled", "interval", cfg.Probe.Interval().String())
	}

	httpServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv,
		// No write timeout: streaming responses can legitimately run for a
		// long time and a deadline here would cut them off.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("entry node listening",
			"addr", cfg.Listen,
			"services", len(cfg.Services),
			"routes", len(cfg.Routes),
			"mode", cfg.Routing.Mode)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("entry server: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown requested, draining connections")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown incomplete", "error", err)
			_ = httpServer.Close()
		}
		return nil
	}
}

// runRelay starts the relay node.
func runRelay(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	srv, err := relay.New(relay.Options{Config: cfg, Logger: logger})
	if err != nil {
		return err
	}
	if err := srv.Start(ctx); err != nil {
		return err
	}

	ports := make([]int, 0, len(cfg.RelayPorts))
	for _, m := range cfg.RelayPorts {
		logger.Info("relaying", "port", m.Port, "target", m.Target)
		ports = append(ports, m.Port)
	}
	logger.Info("relay node listening", "ports", ports)

	<-ctx.Done()
	logger.Info("shutdown requested, closing listeners")
	return srv.Close()
}

// newLogger builds a text logger at the requested level.
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
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

// summarize renders a human readable validation report.
func summarize(cfg *config.Config) string {
	out := fmt.Sprintf("role: %s\n", cfg.Role)
	if cfg.Role == config.RoleEntry {
		out += fmt.Sprintf("listen: %s\n", cfg.Listen)
		out += fmt.Sprintf("routing mode: %s\n", cfg.Routing.Mode)
		out += fmt.Sprintf("sticky: %v\n", cfg.Sticky.Enabled)
		out += fmt.Sprintf("probing: %v\n", cfg.Probe.Enabled)
	}
	for _, s := range cfg.Services {
		out += fmt.Sprintf("service %s -> %s\n", s.Name, s.Upstream)
	}
	for _, n := range cfg.Nodes {
		out += fmt.Sprintf("node %s -> %s\n", n.Name, n.Address)
	}
	for _, r := range cfg.Routes {
		out += fmt.Sprintf("route %s (service %s, %d hops)\n", r.Name, r.Service, len(r.Hops))
	}
	for _, m := range cfg.RelayPorts {
		out += fmt.Sprintf("relay :%d -> %s\n", m.Port, m.Target)
	}
	return out
}
