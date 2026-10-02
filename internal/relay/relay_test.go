package relay

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freePort asks the OS for an unused TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// echoServer starts a TCP server that echoes everything it receives.
func echoServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	closer := func() {
		close(stop)
		_ = ln.Close()
	}
	return ln.Addr().String(), closer
}

func relayConfig(t *testing.T, port int, target string) *config.Config {
	return &config.Config{
		Role: config.RoleRelay,
		RelayPorts: []config.RelayMapping{
			{Port: port, Target: target},
		},
	}
}

func TestRelayForwardsBytes(t *testing.T) {
	echoAddr, stopEcho := echoServer(t)
	defer stopEcho()

	port := freePort(t)
	cfg := relayConfig(t, port, echoAddr)

	srv, err := New(Options{Config: cfg, Logger: testLogger(), DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer func() { _ = conn.Close() }()

	payload := "hello through the relay"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := io.ReadFull(conn, buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != payload {
		t.Fatalf("echo = %q, want %q", string(buf[:n]), payload)
	}
}

func TestRelayHandlesLargeTransfer(t *testing.T) {
	echoAddr, stopEcho := echoServer(t)
	defer stopEcho()

	port := freePort(t)
	srv, err := New(Options{
		Config:      relayConfig(t, port, echoAddr),
		Logger:      testLogger(),
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 1 MiB proves the relay does not depend on a fixed buffer size.
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	go func() { _, _ = conn.Write(payload) }()

	got := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte %d = %d, want %d", i, got[i], payload[i])
		}
	}
}

func TestRelayUnreachableTargetClosesConnection(t *testing.T) {
	// Port 1 is reserved and refuses connections.
	port := freePort(t)
	srv, err := New(Options{
		Config:      relayConfig(t, port, "127.0.0.1:1"),
		Logger:      testLogger(),
		DialTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// The relay should close the client connection rather than hang.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected the connection to be closed when the target is unreachable")
	}
}

func TestRelayRejectsEntryConfig(t *testing.T) {
	_, err := New(Options{
		Config: &config.Config{Role: config.RoleEntry, Listen: "127.0.0.1:0"},
		Logger: testLogger(),
	})
	if err == nil {
		t.Fatal("relay server must refuse an entry config")
	}
}

func TestRelayPortsReported(t *testing.T) {
	echoAddr, stopEcho := echoServer(t)
	defer stopEcho()

	port := freePort(t)
	srv, err := New(Options{
		Config: relayConfig(t, port, echoAddr),
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	ports := srv.Ports()
	if len(ports) != 1 || ports[0] != port {
		t.Fatalf("ports = %v, want [%d]", ports, port)
	}
}

func TestRelayDoubleStartRejected(t *testing.T) {
	echoAddr, stopEcho := echoServer(t)
	defer stopEcho()

	port := freePort(t)
	srv, err := New(Options{Config: relayConfig(t, port, echoAddr), Logger: testLogger()})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("starting twice should fail")
	}
}

func TestUpdateTargetsAddsAndRemovesPorts(t *testing.T) {
	echo1, stop1 := echoServer(t)
	defer stop1()
	echo2, stop2 := echoServer(t)
	defer stop2()

	port1 := freePort(t)
	srv, err := New(Options{Config: relayConfig(t, port1, echo1), Logger: testLogger()})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	port2 := freePort(t)
	err = srv.UpdateTargets([]config.RelayMapping{
		{Port: port1, Target: echo1},
		{Port: port2, Target: echo2},
	})
	if err != nil {
		t.Fatalf("update targets: %v", err)
	}

	// The new port must be usable.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port2), 2*time.Second)
	if err != nil {
		t.Fatalf("dial new port: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read from new port: %v", err)
	}

	// Dropping a port must stop accepting on it.
	if err := srv.UpdateTargets([]config.RelayMapping{{Port: port1, Target: echo1}}); err != nil {
		t.Fatalf("update targets: %v", err)
	}
	if _, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port2), 500*time.Millisecond); err == nil {
		t.Fatal("removed port should no longer accept connections")
	}
}

func TestUpdateTargetsValidatesInput(t *testing.T) {
	echoAddr, stopEcho := echoServer(t)
	defer stopEcho()

	port := freePort(t)
	srv, err := New(Options{Config: relayConfig(t, port, echoAddr), Logger: testLogger()})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	if err := srv.UpdateTargets([]config.RelayMapping{{Port: 0, Target: echoAddr}}); err == nil {
		t.Fatal("port 0 must be rejected")
	}
	if err := srv.UpdateTargets([]config.RelayMapping{{Port: port, Target: ""}}); err == nil {
		t.Fatal("empty target must be rejected")
	}
}

func TestStatsTrackActiveConnections(t *testing.T) {
	// A target that accepts but never responds keeps the relay occupied.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	release := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				<-release
				_ = conn.Close()
			}(c)
		}
	}()
	defer close(release)

	port := freePort(t)
	srv, err := New(Options{
		Config: relayConfig(t, port, ln.Addr().String()),
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Stats()[port] > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected an active connection on port %d, stats = %v", port, srv.Stats())
}

func TestCloseDoesNotHangOnIdlePeers(t *testing.T) {
	// A peer that connects and stays silent would block an unbounded drain.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open and never send anything.
			_ = c
		}
	}()

	port := freePort(t)
	srv, err := New(Options{
		Config:       relayConfig(t, port, ln.Addr().String()),
		Logger:       testLogger(),
		DrainTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Give the relay a moment to register the connection.
	time.Sleep(150 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- srv.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked while an idle peer was connected")
	}
}
