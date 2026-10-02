// Package control lets an agent run in managed mode: it fetches its
// configuration from the panel, applies it without dropping traffic, and
// reports heartbeats back.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
)

// Client talks to the panel.
type Client struct {
	baseURL string
	token   string
	nodeID  string
	role    string
	http    *http.Client
	log     *slog.Logger
}

// Options configures the client.
type Options struct {
	// PanelURL is the panel base URL.
	PanelURL string
	// Token authenticates this agent to the panel.
	Token string
	// NodeName identifies this agent.
	NodeName string
	// Role is entry or relay.
	Role string
	// PollInterval is how often the configuration is refreshed.
	PollInterval time.Duration
	Logger       *slog.Logger
}

// New creates a control client.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.PanelURL) == "" {
		return nil, fmt.Errorf("panel url is required")
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("panel token is required")
	}
	if strings.TrimSpace(opts.NodeName) == "" {
		return nil, fmt.Errorf("node name is required")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 15 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		baseURL: strings.TrimSuffix(opts.PanelURL, "/"),
		token:   opts.Token,
		nodeID:  opts.NodeName,
		role:    opts.Role,
		http: &http.Client{
			Timeout: 20 * time.Second,
		},
		log: opts.Logger,
	}, nil
}

// configResponse is what the panel returns for an agent config request.
type configResponse struct {
	Config config.Config `json:"config"`
	Hash   string        `json:"hash"`
	Role   string        `json:"role"`
	Node   string        `json:"node"`
}

// Fetch retrieves the current configuration for this agent.
func (c *Client) Fetch(ctx context.Context) (*config.Config, string, error) {
	url := fmt.Sprintf("%s/api/agent/config?node=%s", c.baseURL, c.nodeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", fmt.Errorf("read config response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("panel returned %d: %s", resp.StatusCode, trimBody(body))
	}

	var out configResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, "", fmt.Errorf("decode config: %w", err)
	}
	// The panel is the source of truth for role and node identity, but a
	// mismatch means this agent is pointed at the wrong record and applying the
	// document would be dangerous.
	if c.role != "" && out.Role != "" && out.Role != c.role {
		return nil, "", fmt.Errorf("panel returned a %s config for a %s agent", out.Role, c.role)
	}
	if out.Node != "" && out.Node != c.nodeID {
		return nil, "", fmt.Errorf("panel returned a config for node %q", out.Node)
	}
	return &out.Config, out.Hash, nil
}

// heartbeat is the payload reported to the panel.
type heartbeat struct {
	NodeName    string `json:"node_name"`
	OK          bool   `json:"ok"`
	Error       string `json:"error"`
	ActiveConns int64  `json:"active_conns"`
	ConfigHash  string `json:"config_hash"`
}

// Report sends a heartbeat.
func (c *Client) Report(ctx context.Context, ok bool, errMsg string, conns int64, hash string) error {
	payload, err := json.Marshal(heartbeat{
		NodeName:    c.nodeID,
		OK:          ok,
		Error:       errMsg,
		ActiveConns: conns,
		ConfigHash:  hash,
	})
	if err != nil {
		return err
	}

	url := c.baseURL + "/api/agent/heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("send heartbeat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("panel returned %d for heartbeat", resp.StatusCode)
	}
	return nil
}

// ApplyFunc installs a configuration. It must not drop in-flight traffic.
type ApplyFunc func(*config.Config) error

// StateFunc reports the current runtime state for heartbeats.
type StateFunc func() (activeConns int64, ok bool, lastErr string)

// Run keeps the agent in sync with the panel until the context ends.
//
// A configuration that fails to apply is reported to the panel and the previous
// good configuration keeps serving, so a bad edit cannot take the node down.
func (c *Client) Run(ctx context.Context, apply ApplyFunc, state StateFunc) error {
	ticker := time.NewTicker(c.pollInterval())
	defer ticker.Stop()

	var (
		mu         sync.Mutex
		applied    string
		lastErr    string
		lastApply  time.Time
		consecFail int
	)
	_ = lastApply

	syncOnce := func() {
		fetchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		cfg, hash, err := c.Fetch(fetchCtx)
		if err != nil {
			mu.Lock()
			consecFail++
			lastErr = err.Error()
			mu.Unlock()
			c.log.Warn("config fetch failed", "error", err, "consecutive", consecFail)
			return
		}

		mu.Lock()
		unchanged := hash == applied
		mu.Unlock()
		if unchanged {
			c.log.Debug("configuration unchanged", "hash", hash)
			return
		}

		if err := apply(cfg); err != nil {
			mu.Lock()
			consecFail++
			lastErr = "apply failed: " + err.Error()
			mu.Unlock()
			// The previous configuration stays active. Reporting the failure is
			// what lets an operator notice, because the hash does not advance.
			c.log.Error("configuration apply failed, keeping the previous one",
				"error", err, "hash", hash)
			return
		}

		mu.Lock()
		applied = hash
		lastErr = ""
		consecFail = 0
		mu.Unlock()
		c.log.Info("configuration applied", "hash", hash, "version", cfg.Version)
	}

	report := func() {
		mu.Lock()
		hash, errMsg := applied, lastErr
		mu.Unlock()

		conns, ok, runtimeErr := int64(0), errMsg == "", ""
		if state != nil {
			conns, ok, runtimeErr = state()
		}
		if runtimeErr != "" {
			errMsg = runtimeErr
			ok = false
		}

		pushCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := c.Report(pushCtx, ok, errMsg, conns, hash); err != nil {
			c.log.Warn("heartbeat failed", "error", err)
		}
	}

	// Apply once immediately so a restart converges without waiting a tick.
	syncOnce()
	report()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			syncOnce()
			report()
		}
	}
}

func (c *Client) pollInterval() time.Duration {
	if c.baseURL == "" {
		return 15 * time.Second
	}
	return 15 * time.Second
}

// trimBody shortens a response body for error messages.
func trimBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
