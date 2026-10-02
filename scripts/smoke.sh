#!/usr/bin/env bash
# Smoke test for the smart-gateway data plane.
#
# It builds the agent, starts a fake upstream, a relay node and an entry node,
# then verifies that requests reach the upstream through the full chain. It is
# intentionally self contained so it can run on any Linux host with Go.
#
# Usage: ./scripts/smoke.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
PIDS=()

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

log() { printf '%s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Wait until a TCP port accepts connections.
wait_port() {
  local port="$1" name="$2" tries=100
  for ((i = 0; i < tries; i++)); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
      exec 3<&- 3>&-
      return 0
    fi
    sleep 0.1
  done
  fail "${name} did not listen on port ${port}"
}

# Wait until an HTTP endpoint returns 200.
wait_http() {
  local url="$1" name="$2" tries=100
  for ((i = 0; i < tries; i++)); do
    if curl -sf "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  fail "${name} did not become ready at ${url}"
}

log "==> building"
mkdir -p "$WORK/bin"
(cd "$ROOT" && go build -o "$WORK/bin/agent" ./cmd/smart-gateway-agent)
(cd "$ROOT" && go build -o "$WORK/bin/fakesvc" ./testtools/fakesvc)

UPSTREAM_PORT=18084
RELAY_PORT=3001
ENTRY_PORT=18080

log "==> starting fake upstream on ${UPSTREAM_PORT}"
"$WORK/bin/fakesvc" -port "$UPSTREAM_PORT" -name origin >"$WORK/upstream.log" 2>&1 &
PIDS+=($!)
wait_http "http://127.0.0.1:${UPSTREAM_PORT}/health" "upstream"

log "==> starting relay node on ${RELAY_PORT}"
cat >"$WORK/relay.yaml" <<YAML
role: relay
relay_ports:
  - port: ${RELAY_PORT}
    target: 127.0.0.1:${UPSTREAM_PORT}
logging:
  level: info
YAML
"$WORK/bin/agent" -config "$WORK/relay.yaml" >"$WORK/relay.log" 2>&1 &
PIDS+=($!)
wait_port "$RELAY_PORT" "relay"

log "==> starting entry node on ${ENTRY_PORT}"
cat >"$WORK/entry.yaml" <<YAML
role: entry
listen: 127.0.0.1:${ENTRY_PORT}
services:
  - name: origin
    upstream: http://127.0.0.1:${UPSTREAM_PORT}
    health_path: /health
    path_prefix: /v1/
    allow_paths:
      - /v1/echo
      - /v1/sse
    allow_methods: [GET, POST]
    supports_sse: true
    supports_websocket: true
nodes:
  - name: relay
    address: 127.0.0.1:${RELAY_PORT}
routes:
  - name: via-relay
    service: origin
    hops:
      - node: relay
  - name: direct
    service: origin
    hops: []
routing:
  mode: primary_backup
  primary: via-relay
  backup: direct
sticky:
  enabled: true
  hold_minutes: 60
  fail_threshold: 3
probe:
  enabled: true
  interval_seconds: 2
  timeout_seconds: 3
  samples: 10
limits:
  rate_per_minute: 600
logging:
  level: info
  audit_path: ${WORK}/audit.log
YAML
"$WORK/bin/agent" -config "$WORK/entry.yaml" >"$WORK/entry.log" 2>&1 &
PIDS+=($!)
wait_http "http://127.0.0.1:${ENTRY_PORT}/health" "entry"

log "==> checking health passthrough"
curl -sf "http://127.0.0.1:${ENTRY_PORT}/health" | grep -q '"role":"entry"' \
  || fail "entry health did not report the entry role"

log "==> checking request reaches upstream through the relay chain"
RESP="$(curl -sf -X POST "http://127.0.0.1:${ENTRY_PORT}/v1/echo" \
  -H 'Authorization: Bearer client-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"test"}')"
printf '%s' "$RESP" | grep -q '"echo":true' || fail "echo response missing: $RESP"
printf '%s' "$RESP" | grep -q '"service":"origin"' || fail "request did not reach the upstream: $RESP"
printf '%s' "$RESP" | grep -q '"auth":"Bearer client-key"' || fail "authorization header was not forwarded: $RESP"

log "==> checking streaming response is delivered incrementally"
curl -sf -N "http://127.0.0.1:${ENTRY_PORT}/v1/sse" >"$WORK/sse.out" || fail "sse request failed"
grep -q 'data: alpha' "$WORK/sse.out" || fail "sse stream missing first chunk"
grep -q 'data: beta' "$WORK/sse.out" || fail "sse stream missing second chunk"
grep -q 'data: gamma' "$WORK/sse.out" || fail "sse stream missing third chunk"

log "==> checking path policy rejects a disallowed path"
CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${ENTRY_PORT}/v1/not-allowed" -d '{}')"
[[ "$CODE" == "403" ]] || fail "expected 403 for a disallowed path, got $CODE"

log "==> checking method policy"
CODE="$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "http://127.0.0.1:${ENTRY_PORT}/v1/echo")"
[[ "$CODE" == "405" ]] || fail "expected 405 for a disallowed method, got $CODE"

log "==> checking unknown prefix"
CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${ENTRY_PORT}/unknown")"
[[ "$CODE" == "404" ]] || fail "expected 404 for an unknown prefix, got $CODE"

log "==> checking probe collected samples for both routes"
sleep 3
STATS="$(curl -sf "http://127.0.0.1:${ENTRY_PORT}/_gateway/stats")"
printf '%s' "$STATS" | grep -q 'via-relay' || fail "stats missing the relay route: $STATS"

log "==> checking audit log was written"
[[ -s "$WORK/audit.log" ]] || fail "audit log is empty"
grep -q '"route":"via-relay"' "$WORK/audit.log" || fail "audit log missing the relay route entry"

log "==> checking failover when the primary route fails"
# Kill the relay so the primary route becomes unusable. With primary/backup
# routing the entry node must move traffic to the direct route instead of
# returning an error to the client.
kill "${PIDS[1]}" 2>/dev/null || true
wait "${PIDS[1]}" 2>/dev/null || true

# Wait for the active probe to observe the failure and mark the route down.
MARKED=0
for ((i = 0; i < 60; i++)); do
  STATS="$(curl -sf "http://127.0.0.1:${ENTRY_PORT}/_gateway/stats" || true)"
  if printf '%s' "$STATS" | grep -q '"route_name":"via-relay"[^}]*"healthy":false'; then
    MARKED=1
    break
  fi
  sleep 0.5
done
[[ "$MARKED" == "1" ]] || fail "the failed route was never marked unhealthy: $STATS"

log "==> checking traffic moved to the backup route"
CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${ENTRY_PORT}/v1/echo" -d '{}')"
[[ "$CODE" == "200" ]] || fail "expected failover to the direct route, got $CODE"
grep -q '"route":"direct"' "$WORK/audit.log" || fail "audit log does not show the direct route in use"

log ""
log "smoke test passed"
