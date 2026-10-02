package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the panel database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}
	// WAL keeps readers from blocking on the writer, which matters because the
	// agents poll while an operator edits configuration.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// migrate creates the schema and seeds defaults.
func (s *Store) migrate(ctx context.Context) error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			routing TEXT NOT NULL,
			sticky TEXT NOT NULL,
			probe TEXT NOT NULL,
			limits TEXT NOT NULL,
			audit_path TEXT NOT NULL DEFAULT '',
			log_level TEXT NOT NULL DEFAULT 'info',
			relay_port_start INTEGER NOT NULL DEFAULT 3001,
			relay_port_end INTEGER NOT NULL DEFAULT 3100,
			version INTEGER NOT NULL DEFAULT 1,
			entry_listen TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS nodes (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			role TEXT NOT NULL,
			address TEXT NOT NULL DEFAULT '',
			remark TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			relay_port INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL DEFAULT '',
			agent_ok INTEGER NOT NULL DEFAULT 0,
			agent_error TEXT NOT NULL DEFAULT '',
			config_hash TEXT NOT NULL DEFAULT '',
			active_conns INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS services (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			upstream TEXT NOT NULL,
			host_override TEXT NOT NULL DEFAULT '',
			health_path TEXT NOT NULL DEFAULT '/health',
			path_prefix TEXT NOT NULL DEFAULT '/',
			strip_prefix INTEGER NOT NULL DEFAULT 0,
			allow_paths TEXT NOT NULL DEFAULT '[]',
			allow_methods TEXT NOT NULL DEFAULT '[]',
			max_body_mb INTEGER NOT NULL DEFAULT 100,
			read_timeout_seconds INTEGER NOT NULL DEFAULT 3600,
			supports_sse INTEGER NOT NULL DEFAULT 1,
			supports_websocket INTEGER NOT NULL DEFAULT 1,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS routes (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			service TEXT NOT NULL,
			node_names TEXT NOT NULL DEFAULT '[]',
			port INTEGER NOT NULL DEFAULT 0,
			weight INTEGER NOT NULL DEFAULT 100,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id TEXT PRIMARY KEY,
			label TEXT NOT NULL DEFAULT '',
			token TEXT NOT NULL UNIQUE,
			node_name TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
	}

	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}

	// Additive migrations for databases created by an earlier build. SQLite has
	// no "ADD COLUMN IF NOT EXISTS", so the duplicate-column error is expected
	// and ignored.
	for _, stmt := range []string{
		`ALTER TABLE settings ADD COLUMN entry_listen TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil &&
			!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings`).Scan(&count); err != nil {
		return fmt.Errorf("count settings: %w", err)
	}
	if count == 0 {
		def := DefaultSettings()
		if err := s.saveSettings(ctx, def); err != nil {
			return err
		}
	}
	return nil
}

// Settings returns the site configuration.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	const q = `SELECT routing, sticky, probe, limits, audit_path, log_level,
		relay_port_start, relay_port_end, entry_listen, version, updated_at FROM settings WHERE id = 1`
	var (
		routing, sticky, probe, limits string
		updatedAt                      string
		out                            Settings
	)
	// Column order must match the SELECT above.
	err := s.db.QueryRowContext(ctx, q).Scan(
		&routing, &sticky, &probe, &limits, &out.AuditPath, &out.LogLevel,
		&out.RelayPortStart, &out.RelayPortEnd, &out.EntryListen, &out.Version, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DefaultSettings(), nil
		}
		return out, fmt.Errorf("read settings: %w", err)
	}
	if err := json.Unmarshal([]byte(routing), &out.Routing); err != nil {
		return out, fmt.Errorf("decode routing: %w", err)
	}
	if err := json.Unmarshal([]byte(sticky), &out.Sticky); err != nil {
		return out, fmt.Errorf("decode sticky: %w", err)
	}
	if err := json.Unmarshal([]byte(probe), &out.Probe); err != nil {
		return out, fmt.Errorf("decode probe: %w", err)
	}
	if err := json.Unmarshal([]byte(limits), &out.Limits); err != nil {
		return out, fmt.Errorf("decode limits: %w", err)
	}
	out.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	if out.Routing.Weights == nil {
		out.Routing.Weights = map[string]int{}
	}
	return out, nil
}

// SaveSettings writes the site configuration and bumps the version.
func (s *Store) SaveSettings(ctx context.Context, in Settings) (Settings, error) {
	if err := validateSettings(&in); err != nil {
		return in, err
	}
	cur, err := s.Settings(ctx)
	if err != nil {
		return in, err
	}
	in.Version = cur.Version + 1
	if err := s.saveSettings(ctx, in); err != nil {
		return in, err
	}
	return in, nil
}

func (s *Store) saveSettings(ctx context.Context, in Settings) error {
	if in.Routing.Weights == nil {
		in.Routing.Weights = map[string]int{}
	}
	routing, _ := json.Marshal(in.Routing)
	sticky, _ := json.Marshal(in.Sticky)
	probe, _ := json.Marshal(in.Probe)
	limits, _ := json.Marshal(in.Limits)

	const q = `INSERT INTO settings
		(id, routing, sticky, probe, limits, audit_path, log_level,
		 relay_port_start, relay_port_end, entry_listen, version, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			routing = excluded.routing,
			sticky = excluded.sticky,
			probe = excluded.probe,
			limits = excluded.limits,
			audit_path = excluded.audit_path,
			log_level = excluded.log_level,
			relay_port_start = excluded.relay_port_start,
			relay_port_end = excluded.relay_port_end,
			entry_listen = excluded.entry_listen,
			version = excluded.version,
			updated_at = excluded.updated_at`
	_, err := s.db.ExecContext(ctx, q,
		string(routing), string(sticky), string(probe), string(limits),
		in.AuditPath, in.LogLevel, in.RelayPortStart, in.RelayPortEnd,
		in.EntryListen, in.Version, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

func validateSettings(in *Settings) error {
	switch in.Routing.Mode {
	case "static", "primary_backup", "weighted":
	case "":
		in.Routing.Mode = "primary_backup"
	default:
		return fmt.Errorf("unknown routing mode %q", in.Routing.Mode)
	}
	// The entry bind address is optional, but a value that is not host:port
	// would make every entry node fail to start.
	if in.EntryListen != "" {
		if _, _, err := net.SplitHostPort(in.EntryListen); err != nil {
			return fmt.Errorf("entry listen address %q is not host:port", in.EntryListen)
		}
	}
	if in.Sticky.HoldMinutes < 0 {
		return errors.New("sticky hold minutes must not be negative")
	}
	if in.Sticky.FailThreshold < 0 {
		return errors.New("sticky fail threshold must not be negative")
	}
	if in.Probe.IntervalSeconds < 0 {
		return errors.New("probe interval must not be negative")
	}
	if in.Probe.Samples < 0 {
		return errors.New("probe samples must not be negative")
	}
	if in.Limits.MaxConcurrent < 0 || in.Limits.RatePerMinute < 0 {
		return errors.New("limits must not be negative")
	}
	if in.RelayPortStart <= 0 || in.RelayPortEnd <= 0 {
		return errors.New("relay port range must be positive")
	}
	if in.RelayPortStart > in.RelayPortEnd {
		return errors.New("relay port range start must not exceed end")
	}
	if in.RelayPortEnd > 65535 {
		return errors.New("relay port range end must not exceed 65535")
	}
	if in.LogLevel == "" {
		in.LogLevel = "info"
	}
	return nil
}

// Nodes returns every node ordered by name.
func (s *Store) Nodes(ctx context.Context) ([]Node, error) {
	const q = `SELECT id, name, role, address, remark, enabled, relay_port,
		created_at, updated_at, last_seen_at, agent_ok, agent_error,
		config_hash, active_conns FROM nodes ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Node returns one node by id.
func (s *Store) Node(ctx context.Context, id string) (Node, error) {
	const q = `SELECT id, name, role, address, remark, enabled, relay_port,
		created_at, updated_at, last_seen_at, agent_ok, agent_error,
		config_hash, active_conns FROM nodes WHERE id = ?`
	row := s.db.QueryRowContext(ctx, q, id)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

// NodeByName looks a node up by its unique name.
func (s *Store) NodeByName(ctx context.Context, name string) (Node, error) {
	const q = `SELECT id, name, role, address, remark, enabled, relay_port,
		created_at, updated_at, last_seen_at, agent_ok, agent_error,
		config_hash, active_conns FROM nodes WHERE name = ?`
	row := s.db.QueryRowContext(ctx, q, name)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

type scanner interface{ Scan(dest ...any) error }

func scanNode(sc scanner) (Node, error) {
	var (
		n                            Node
		enabled, agentOK             int
		createdAt, updatedAt, seenAt string
	)
	if err := sc.Scan(&n.ID, &n.Name, &n.Role, &n.Address, &n.Remark, &enabled,
		&n.RelayPort, &createdAt, &updatedAt, &seenAt, &agentOK,
		&n.AgentError, &n.ConfigHash, &n.ActiveConns); err != nil {
		return n, err
	}
	n.Enabled = enabled != 0
	n.AgentOK = agentOK != 0
	n.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	n.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	n.LastSeenAt, _ = time.Parse(time.RFC3339Nano, seenAt)
	return n, nil
}

// SaveNode creates or updates a node.
func (s *Store) SaveNode(ctx context.Context, n Node) (Node, error) {
	if err := ValidateNodeName(n.Name); err != nil {
		return n, err
	}
	switch n.Role {
	case RoleEntry, RoleRelay:
	case "":
		return n, errors.New("node role is required")
	default:
		return n, fmt.Errorf("unknown node role %q", n.Role)
	}
	if n.Role == RoleRelay {
		if err := ValidateAddress(n.Address); err != nil {
			return n, err
		}
	}
	if n.ID == "" {
		n.ID = NewID()
		n.CreatedAt = time.Now().UTC()
	}
	n.UpdatedAt = time.Now().UTC()

	const q = `INSERT INTO nodes
		(id, name, role, address, remark, enabled, relay_port, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			role = excluded.role,
			address = excluded.address,
			remark = excluded.remark,
			enabled = excluded.enabled,
			relay_port = excluded.relay_port,
			updated_at = excluded.updated_at`
	_, err := s.db.ExecContext(ctx, q, n.ID, n.Name, string(n.Role), n.Address,
		n.Remark, boolToInt(n.Enabled), n.RelayPort,
		n.CreatedAt.Format(time.RFC3339Nano), n.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return n, fmt.Errorf("a node named %q already exists", n.Name)
		}
		return n, fmt.Errorf("write node: %w", err)
	}
	return s.Node(ctx, n.ID)
}

// DeleteNode removes a node by id.
func (s *Store) DeleteNode(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete node: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateNodeStatus records a heartbeat from an agent.
func (s *Store) UpdateNodeStatus(ctx context.Context, name string, ok bool, errMsg string, conns int64, hash string) error {
	const q = `UPDATE nodes SET last_seen_at = ?, agent_ok = ?, agent_error = ?,
		active_conns = ?, config_hash = ? WHERE name = ?`
	_, err := s.db.ExecContext(ctx, q, time.Now().UTC().Format(time.RFC3339Nano),
		boolToInt(ok), errMsg, conns, hash, name)
	if err != nil {
		return fmt.Errorf("update node status: %w", err)
	}
	return nil
}

// Services returns every service ordered by name.
func (s *Store) Services(ctx context.Context) ([]Service, error) {
	const q = `SELECT id, name, upstream, host_override, health_path, path_prefix,
		strip_prefix, allow_paths, allow_methods, max_body_mb, read_timeout_seconds,
		supports_sse, supports_websocket, enabled, created_at, updated_at
		FROM services ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Service{}
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// Service returns one service by id.
func (s *Store) Service(ctx context.Context, id string) (Service, error) {
	const q = `SELECT id, name, upstream, host_override, health_path, path_prefix,
		strip_prefix, allow_paths, allow_methods, max_body_mb, read_timeout_seconds,
		supports_sse, supports_websocket, enabled, created_at, updated_at
		FROM services WHERE id = ?`
	svc, err := scanService(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return svc, ErrNotFound
	}
	return svc, err
}

// ServiceByName looks a service up by its unique name.
func (s *Store) ServiceByName(ctx context.Context, name string) (Service, error) {
	const q = `SELECT id, name, upstream, host_override, health_path, path_prefix,
		strip_prefix, allow_paths, allow_methods, max_body_mb, read_timeout_seconds,
		supports_sse, supports_websocket, enabled, created_at, updated_at
		FROM services WHERE name = ?`
	svc, err := scanService(s.db.QueryRowContext(ctx, q, name))
	if errors.Is(err, sql.ErrNoRows) {
		return svc, ErrNotFound
	}
	return svc, err
}

func scanService(sc scanner) (Service, error) {
	var (
		svc                           Service
		stripPrefix, sse, ws, enabled int
		allowPaths, allowMethods      string
		createdAt, updatedAt          string
	)
	if err := sc.Scan(&svc.ID, &svc.Name, &svc.Upstream, &svc.HostOverride,
		&svc.HealthPath, &svc.PathPrefix, &stripPrefix, &allowPaths, &allowMethods,
		&svc.MaxBodyMB, &svc.ReadTimeoutSeconds, &sse, &ws, &enabled,
		&createdAt, &updatedAt); err != nil {
		return svc, err
	}
	svc.StripPrefix = stripPrefix != 0
	svc.SupportsSSE = sse != 0
	svc.SupportsWebSocket = ws != 0
	svc.Enabled = enabled != 0
	_ = json.Unmarshal([]byte(allowPaths), &svc.AllowPaths)
	_ = json.Unmarshal([]byte(allowMethods), &svc.AllowMethods)
	svc.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	svc.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return svc, nil
}

// SaveService creates or updates a service.
func (s *Store) SaveService(ctx context.Context, svc Service) (Service, error) {
	if strings.TrimSpace(svc.Name) == "" {
		return svc, errors.New("service name is required")
	}
	if err := ValidateUpstream(svc.Upstream); err != nil {
		return svc, err
	}
	if svc.HealthPath == "" {
		svc.HealthPath = "/health"
	}
	if !strings.HasPrefix(svc.HealthPath, "/") {
		svc.HealthPath = "/" + svc.HealthPath
	}
	if svc.PathPrefix == "" {
		svc.PathPrefix = "/"
	}
	if !strings.HasPrefix(svc.PathPrefix, "/") {
		svc.PathPrefix = "/" + svc.PathPrefix
	}
	if svc.MaxBodyMB <= 0 {
		svc.MaxBodyMB = 100
	}
	if svc.ReadTimeoutSeconds < 0 {
		return svc, errors.New("read timeout must not be negative")
	}
	if svc.ID == "" {
		svc.ID = NewID()
		svc.CreatedAt = time.Now().UTC()
	}
	svc.UpdatedAt = time.Now().UTC()

	allowPaths, _ := json.Marshal(orEmpty(svc.AllowPaths))
	allowMethods, _ := json.Marshal(orEmpty(svc.AllowMethods))

	const q = `INSERT INTO services
		(id, name, upstream, host_override, health_path, path_prefix, strip_prefix,
		 allow_paths, allow_methods, max_body_mb, read_timeout_seconds,
		 supports_sse, supports_websocket, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			upstream = excluded.upstream,
			host_override = excluded.host_override,
			health_path = excluded.health_path,
			path_prefix = excluded.path_prefix,
			strip_prefix = excluded.strip_prefix,
			allow_paths = excluded.allow_paths,
			allow_methods = excluded.allow_methods,
			max_body_mb = excluded.max_body_mb,
			read_timeout_seconds = excluded.read_timeout_seconds,
			supports_sse = excluded.supports_sse,
			supports_websocket = excluded.supports_websocket,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at`
	_, err := s.db.ExecContext(ctx, q, svc.ID, svc.Name, svc.Upstream,
		svc.HostOverride, svc.HealthPath, svc.PathPrefix, boolToInt(svc.StripPrefix),
		string(allowPaths), string(allowMethods), svc.MaxBodyMB,
		svc.ReadTimeoutSeconds, boolToInt(svc.SupportsSSE),
		boolToInt(svc.SupportsWebSocket), boolToInt(svc.Enabled),
		svc.CreatedAt.Format(time.RFC3339Nano), svc.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return svc, fmt.Errorf("a service named %q already exists", svc.Name)
		}
		return svc, fmt.Errorf("write service: %w", err)
	}
	return s.Service(ctx, svc.ID)
}

// DeleteService removes a service by id.
func (s *Store) DeleteService(ctx context.Context, id string) error {
	svc, err := s.Service(ctx, id)
	if err != nil {
		return err
	}
	routes, err := s.Routes(ctx)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if r.Service == svc.Name {
			return fmt.Errorf("service %q is still used by route %q", svc.Name, r.Name)
		}
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM services WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Routes returns every route ordered by name.
func (s *Store) Routes(ctx context.Context) ([]Route, error) {
	const q = `SELECT id, name, service, node_names, port, weight, enabled,
		created_at, updated_at FROM routes ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Route{}
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Route returns one route by id.
func (s *Store) Route(ctx context.Context, id string) (Route, error) {
	const q = `SELECT id, name, service, node_names, port, weight, enabled,
		created_at, updated_at FROM routes WHERE id = ?`
	r, err := scanRoute(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func scanRoute(sc scanner) (Route, error) {
	var (
		r         Route
		nodeNames string
		enabled   int
		createdAt string
		updatedAt string
	)
	if err := sc.Scan(&r.ID, &r.Name, &r.Service, &nodeNames, &r.Port, &r.Weight,
		&enabled, &createdAt, &updatedAt); err != nil {
		return r, err
	}
	r.Enabled = enabled != 0
	_ = json.Unmarshal([]byte(nodeNames), &r.NodeNames)
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return r, nil
}

// SaveRoute creates or updates a route.
func (s *Store) SaveRoute(ctx context.Context, r Route) (Route, error) {
	if strings.TrimSpace(r.Name) == "" {
		return r, errors.New("route name is required")
	}
	if strings.TrimSpace(r.Service) == "" {
		return r, errors.New("route must reference a service")
	}
	if _, err := s.ServiceByName(ctx, r.Service); err != nil {
		return r, fmt.Errorf("route references unknown service %q", r.Service)
	}
	for _, name := range r.NodeNames {
		n, err := s.NodeByName(ctx, name)
		if err != nil {
			return r, fmt.Errorf("route references unknown node %q", name)
		}
		if n.Role != RoleRelay {
			return r, fmt.Errorf("node %q is an entry node and cannot be a route hop", name)
		}
	}
	if r.Weight <= 0 {
		r.Weight = 100
	}
	if r.ID == "" {
		r.ID = NewID()
		r.CreatedAt = time.Now().UTC()
	}
	r.UpdatedAt = time.Now().UTC()

	nodeNames, _ := json.Marshal(orEmpty(r.NodeNames))

	const q = `INSERT INTO routes
		(id, name, service, node_names, port, weight, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			service = excluded.service,
			node_names = excluded.node_names,
			port = excluded.port,
			weight = excluded.weight,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at`
	_, err := s.db.ExecContext(ctx, q, r.ID, r.Name, r.Service, string(nodeNames),
		r.Port, r.Weight, boolToInt(r.Enabled),
		r.CreatedAt.Format(time.RFC3339Nano), r.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return r, fmt.Errorf("a route named %q already exists", r.Name)
		}
		return r, fmt.Errorf("write route: %w", err)
	}
	return s.Route(ctx, r.ID)
}

// DeleteRoute removes a route by id.
func (s *Store) DeleteRoute(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM routes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete route: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AllocatePort picks a free relay port from the configured pool, avoiding
// ports already used by an existing route.
func (s *Store) AllocatePort(ctx context.Context) (int, error) {
	settings, err := s.Settings(ctx)
	if err != nil {
		return 0, err
	}
	routes, err := s.Routes(ctx)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, r := range routes {
		if r.Port > 0 {
			used[r.Port] = true
		}
	}
	for port := settings.RelayPortStart; port <= settings.RelayPortEnd; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free relay port in range %d-%d", settings.RelayPortStart, settings.RelayPortEnd)
}

// Token is a credential an agent uses to authenticate to the panel.
type Token struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Token     string    `json:"token"`
	NodeName  string    `json:"node_name"`
	CreatedAt time.Time `json:"created_at"`
}

// SaveToken stores a new agent token.
func (s *Store) SaveToken(ctx context.Context, t Token) (Token, error) {
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.Token == "" {
		t.Token = "sg_" + randomToken(24)
	}
	t.CreatedAt = time.Now().UTC()
	const q = `INSERT INTO tokens (id, label, token, node_name, created_at)
		VALUES (?, ?, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, q, t.ID, t.Label, t.Token, t.NodeName,
		t.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return t, fmt.Errorf("write token: %w", err)
	}
	return t, nil
}

// Tokens returns every agent token ordered by creation time.
func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, label, token, node_name, created_at FROM tokens ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Token{}
	for rows.Next() {
		var (
			t         Token
			createdAt string
		)
		if err := rows.Scan(&t.ID, &t.Label, &t.Token, &t.NodeName, &createdAt); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ValidToken reports whether a token string is known.
func (s *Store) ValidToken(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens WHERE token = ?`, token).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check token: %w", err)
	}
	return n > 0, nil
}

// DeleteToken removes a token by id.
func (s *Store) DeleteToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// SortedNames returns a stable copy of a name list.
func SortedNames(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
