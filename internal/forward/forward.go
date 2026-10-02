// Package forward implements the data plane. The entry node speaks HTTP and
// understands request semantics; relay nodes stay protocol agnostic and move
// bytes. Nothing here knows about a specific upstream service.
package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
)

// ErrTimeout is returned when an upstream does not respond in time.
var ErrTimeout = errors.New("upstream timeout")

// Transport builds a per-route http.Transport. Keeping a transport per route
// avoids one unhealthy path poisoning the connection pool of another.
type Transport struct {
	pool map[string]*http.Transport
}

// NewTransport creates an empty transport pool.
func NewTransport() *Transport {
	return &Transport{pool: map[string]*http.Transport{}}
}

// For returns the transport bound to a route key.
func (t *Transport) For(key string) *http.Transport {
	if tr, ok := t.pool[key]; ok {
		return tr
	}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Streaming responses must not be buffered by the transport.
		DisableCompression: true,
	}
	t.pool[key] = tr
	return tr
}

// Close releases every pooled transport.
func (t *Transport) Close() {
	for _, tr := range t.pool {
		tr.CloseIdleConnections()
	}
}

// HTTPForwarder proxies client requests to a resolved upstream.
type HTTPForwarder struct {
	transports *Transport
}

// NewHTTPForwarder creates a forwarder with its own transport pool.
func NewHTTPForwarder() *HTTPForwarder {
	return &HTTPForwarder{transports: NewTransport()}
}

// Close releases idle upstream connections.
func (f *HTTPForwarder) Close() { f.transports.Close() }

// Plan describes a single resolved forwarding decision.
type Plan struct {
	// RouteName is the chosen route, for logging.
	RouteName string
	// Upstream is the fully resolved base URL of the final target.
	Upstream string
	// Service carries the policy that applies to this request.
	Service config.Service
}

// PolicyError describes a rejected request together with the status code the
// gateway should return, so callers do not have to guess a mapping.
type PolicyError struct {
	// Status is the HTTP status to send to the client.
	Status int
	// Detail explains the rejection and is safe to return to the client.
	Detail string
}

func (e *PolicyError) Error() string { return e.Detail }

// CheckRequest applies the service policy to an inbound request.
func CheckRequest(r *http.Request, svc config.Service) error {
	if !svc.Methods()[r.Method] {
		return &PolicyError{
			Status: http.StatusMethodNotAllowed,
			Detail: fmt.Sprintf("method %s is not allowed", r.Method),
		}
	}
	if len(svc.AllowPaths) > 0 && !pathAllowed(r.URL.Path, svc.AllowPaths) {
		return &PolicyError{
			Status: http.StatusForbidden,
			Detail: fmt.Sprintf("path %s is not allowed", r.URL.Path),
		}
	}
	if r.ContentLength > svc.BodyLimit() {
		return &PolicyError{
			Status: http.StatusRequestEntityTooLarge,
			Detail: fmt.Sprintf("request body %d exceeds limit %d", r.ContentLength, svc.BodyLimit()),
		}
	}
	return nil
}

// pathAllowed reports whether a request path satisfies a whitelist entry.
//
// Supported forms:
//   - "/v1/messages"  exact match
//   - "/v1/*"         prefix match on a path boundary
//   - "/v1/**"        recursive match, equivalent to an unbounded prefix
func pathAllowed(path string, allow []string) bool {
	for _, pattern := range allow {
		p := strings.TrimSpace(pattern)
		if p == "" {
			continue
		}
		switch {
		case p == "/**" || p == "*":
			return true
		case strings.HasSuffix(p, "**"):
			if strings.HasPrefix(path, strings.TrimSuffix(p, "**")) {
				return true
			}
		case strings.HasSuffix(p, "*"):
			// A single star matches only within the same path segment.
			prefix := strings.TrimSuffix(p, "*")
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			rest := strings.TrimPrefix(path, prefix)
			if !strings.Contains(rest, "/") {
				return true
			}
		default:
			if path == p {
				return true
			}
		}
	}
	return false
}

// IsWebSocketUpgrade reports whether the request asks for a protocol upgrade.
func IsWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, tok := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
			return true
		}
	}
	return false
}

// ServeHTTP forwards an inbound request according to the supplied plan.
//
// The plan is resolved by the caller so that route selection, stickiness and
// failover stay in one place. This function only deals with the mechanics of
// relaying one request to one upstream.
func (f *HTTPForwarder) ServeHTTP(w http.ResponseWriter, r *http.Request, plan Plan) error {
	target, err := joinUpstream(plan.Upstream, r.URL, plan.Service)
	if err != nil {
		return err
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = target
			if plan.Service.HostOverride != "" {
				req.Host = plan.Service.HostOverride
			}
		},
		Transport:     f.transports.For(plan.RouteName),
		FlushInterval: flushInterval(plan.Service),
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
				status = http.StatusGatewayTimeout
			}
			http.Error(rw, fmt.Sprintf("upstream error: %v", err), status)
		},
	}
	proxy.ServeHTTP(w, r)
	return nil
}

// flushInterval returns -1 for streaming services so that the proxy writes
// each chunk immediately, and a small finite interval otherwise.
func flushInterval(svc config.Service) time.Duration {
	if svc.SupportsSSE {
		return -1
	}
	return 100 * time.Millisecond
}

// joinUpstream resolves the final absolute URL for a request.
func joinUpstream(base string, in *url.URL, svc config.Service) (*url.URL, error) {
	up, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream %q: %w", base, err)
	}
	if up.Host == "" {
		return nil, fmt.Errorf("upstream %q has no host", base)
	}

	basePath := strings.TrimSuffix(up.Path, "/")
	reqPath := in.Path
	if svc.StripPrefix && svc.PathPrefix != "" {
		reqPath = strings.TrimPrefix(reqPath, strings.TrimSuffix(svc.PathPrefix, "/"))
	}
	if reqPath == "" {
		reqPath = "/"
	}
	if basePath != "" && !strings.HasPrefix(reqPath, basePath) {
		reqPath = basePath + reqPath
	}

	out := *up
	out.Path = reqPath
	out.RawQuery = in.RawQuery
	out.Fragment = ""
	return &out, nil
}

// CopyBidirectional relays bytes in both directions until either side closes.
func CopyBidirectional(a, b net.Conn, idle time.Duration) {
	done := make(chan struct{}, 2)

	copyOne := func(dst, src net.Conn) {
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

	go copyOne(a, b)
	go copyOne(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
}

// DialTarget opens a TCP connection to host:port.
func DialTarget(ctx context.Context, target string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", target)
}

// ReadAllLimited reads at most limit bytes from r, returning an error when the
// limit is exceeded. Used for bounded request handling.
func ReadAllLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds limit %d", limit)
	}
	return data, nil
}
