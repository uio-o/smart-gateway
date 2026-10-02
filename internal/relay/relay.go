// Package relay implements the node agent. A relay node forwards bytes to its
// next hop without interpreting the payload, so it adds as little overhead as
// possible to a request that the entry node has already routed.
package relay

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
)

// Server listens on the configured ports and forwards each connection to its
// mapped target.
type Server struct {
	cfg      *config.Config
	log      *slog.Logger
	idle     time.Duration
	dialTo   time.Duration
	drainFor time.Duration

	mu       sync.Mutex
	started  bool
	listener map[int]net.Listener
	conns    map[int]int64
	active   map[net.Conn]struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// Options configures a relay server.
type Options struct {
	Config *config.Config
	Logger *slog.Logger
	// IdleTimeout closes a connection after this much silence. Zero disables
	// the deadline, which is the right choice for long lived streams.
	IdleTimeout time.Duration
	// DialTimeout bounds connecting to the next hop.
	DialTimeout time.Duration
	// DrainTimeout bounds how long shutdown waits for in-flight relays.
	DrainTimeout time.Duration
}

// New creates a relay server.
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("config is required")
	}
	if opts.Config.Role != config.RoleRelay {
		return nil, fmt.Errorf("relay server requires role=relay")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = 5 * time.Second
	}
	return &Server{
		cfg:      opts.Config,
		log:      opts.Logger,
		idle:     opts.IdleTimeout,
		dialTo:   opts.DialTimeout,
		drainFor: opts.DrainTimeout,
		listener: map[int]net.Listener{},
		conns:    map[int]int64{},
		active:   map[net.Conn]struct{}{},
	}, nil
}

// Start begins listening on every configured port.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("relay server already started")
	}

	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	// Bind everything before serving so a partial failure leaves no listeners.
	for _, m := range s.cfg.RelayPorts {
		addr := fmt.Sprintf(":%d", m.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, opened := range s.listener {
				_ = opened.Close()
			}
			s.listener = map[int]net.Listener{}
			cancel()
			return fmt.Errorf("listen on %s: %w", addr, err)
		}
		s.listener[m.Port] = ln
	}

	s.started = true
	for port, ln := range s.listener {
		target := s.targetFor(port)
		s.wg.Add(1)
		go s.serve(ctx, ln, port, target)
	}
	s.log.Info("relay started", "ports", len(s.listener))
	return nil
}

// targetFor returns the configured downstream address for a port.
func (s *Server) targetFor(port int) string {
	for _, m := range s.cfg.RelayPorts {
		if m.Port == port {
			return m.Target
		}
	}
	return ""
}

// serve accepts connections until the listener is closed.
func (s *Server) serve(ctx context.Context, ln net.Listener, port int, target string) {
	defer s.wg.Done()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			s.log.Warn("accept failed", "port", port, "error", err)
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn, port, target)
		}()
	}
}

// handle relays one accepted connection to the downstream target.
func (s *Server) handle(ctx context.Context, client net.Conn, port int, target string) {
	defer func() { _ = client.Close() }()

	s.trackConn(port, 1)
	defer s.trackConn(port, -1)
	s.registerConn(client)
	defer s.unregisterConn(client)
	if target == "" {
		s.log.Error("no target configured for port", "port", port)
		return
	}
	if tc, ok := client.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	upstream, err := dialWithContext(ctx, target, s.dialTo)
	if err != nil {
		s.log.Warn("dial next hop failed", "port", port, "target", target, "error", err)
		return
	}
	defer func() { _ = upstream.Close() }()
	if tc, ok := upstream.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	relayBoth(client, upstream, s.idle)
}

// trackConn maintains a per-port connection gauge.
func (s *Server) trackConn(port int, delta int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[port] += delta
}

// registerConn records a live connection so shutdown can close it if needed.
func (s *Server) registerConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[conn] = struct{}{}
}

// unregisterConn forgets a connection.
func (s *Server) unregisterConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, conn)
}

// Stats reports active connections per port.
func (s *Server) Stats() map[int]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]int64, len(s.conns))
	for port, n := range s.conns {
		out[port] = n
	}
	return out
}

// Ports reports the ports this relay listens on.
func (s *Server) Ports() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.listener))
	for port := range s.listener {
		out = append(out, port)
	}
	return out
}

// Close stops every listener and waits for in-flight relays to finish, up to
// a bounded drain period.
//
// A relay cannot wait indefinitely: peers may hold long lived connections open
// (streaming responses, protocol upgrades), and a shutdown that never returns
// would stall the whole node. After the drain window the remaining connections
// are closed forcibly so the process can exit.
func (s *Server) Close() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	for _, ln := range s.listener {
		_ = ln.Close()
	}
	s.listener = map[int]net.Listener{}
	s.started = false
	s.mu.Unlock()

	s.drain()
	return nil
}

// drain waits for in-flight relays to finish, then forces the remainder closed.
func (s *Server) drain() {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(s.drainTimeout()):
	}

	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.active))
	for conn := range s.active {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s.log.Warn("relay connections did not drain after being closed")
	}
}

// drainTimeout returns how long to wait for a graceful drain.
func (s *Server) drainTimeout() time.Duration {
	if s.drainFor <= 0 {
		return 5 * time.Second
	}
	return s.drainFor
}

// UpdateTargets replaces the port to target mapping for future connections.
//
// Existing relays keep running; only new connections use the new mapping. This
// keeps configuration changes invisible to streams that are already in flight.
func (s *Server) UpdateTargets(mappings []config.RelayMapping) error {
	seen := map[int]bool{}
	for _, m := range mappings {
		if m.Port <= 0 || m.Port > 65535 {
			return fmt.Errorf("port %d out of range", m.Port)
		}
		if m.Target == "" {
			return fmt.Errorf("port %d has empty target", m.Port)
		}
		seen[m.Port] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Open listeners for new ports.
	added := []int{}
	for _, m := range mappings {
		if _, ok := s.listener[m.Port]; ok {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", m.Port))
		if err != nil {
			for _, port := range added {
				_ = s.listener[port].Close()
				delete(s.listener, port)
			}
			return fmt.Errorf("listen on :%d: %w", m.Port, err)
		}
		s.listener[m.Port] = ln
		added = append(added, m.Port)
	}

	// Close listeners for ports that disappeared.
	for port, ln := range s.listener {
		if seen[port] {
			continue
		}
		_ = ln.Close()
		delete(s.listener, port)
	}

	// Publish the new mapping and start serving any newly opened ports.
	s.cfg.RelayPorts = mappings
	for _, port := range added {
		target := s.targetFor(port)
		s.wg.Add(1)
		go s.serve(context.Background(), s.listener[port], port, target)
	}
	s.log.Info("relay targets updated", "ports", len(s.cfg.RelayPorts))
	return nil
}

func dialWithContext(ctx context.Context, target string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, "tcp", target)
}

// relayBoth copies bytes in both directions until one side closes.
//
// It is deliberately protocol agnostic so streaming responses and protocol
// upgrades pass through without buffering or rewriting.
func relayBoth(a, b net.Conn, idle time.Duration) {
	done := make(chan struct{}, 2)

	pump := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			if idle > 0 {
				_ = src.SetReadDeadline(time.Now().Add(idle))
			}
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}

	go pump(a, b)
	go pump(b, a)
	<-done

	// Closing both sides unblocks the remaining pump.
	_ = a.Close()
	_ = b.Close()
	<-done
}
