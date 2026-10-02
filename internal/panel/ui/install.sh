#!/usr/bin/env bash
#
# Bootstrap a smart-gateway agent on a node.
#
# Usage:
#   curl -fsSL https://panel.example.com/install.sh | sudo bash -s -- \
#       --panel https://panel.example.com \
#       --node tokyo --role relay \
#       --token sg_xxx
#
# The script installs a single static binary and a systemd unit. Relay nodes
# need nothing else: no domain, no certificate, no reverse proxy. Entry nodes
# listen on 127.0.0.1 and are expected to sit behind an operator managed proxy.

set -euo pipefail

PANEL_URL=""
NODE_NAME=""
ROLE="relay"
TOKEN=""
LISTEN="127.0.0.1:8080"
RELEASE="latest"
BIN_DIR="/usr/local/bin"
ETC_DIR="/etc/smart-gateway"
SERVICE="smart-gateway-agent"

usage() {
  cat <<'EOF'
Options:
  --panel URL     panel base URL (required)
  --node NAME     node name as registered in the panel (required)
  --role ROLE     entry or relay (default: relay)
  --token TOKEN   agent token issued by the panel (required)
  --listen ADDR   entry listen address (default: 127.0.0.1:8080)
  --release TAG   release tag to download (default: latest)
EOF
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --panel)   PANEL_URL="${2:-}"; shift 2 ;;
    --node)    NODE_NAME="${2:-}"; shift 2 ;;
    --role)    ROLE="${2:-}"; shift 2 ;;
    --token)   TOKEN="${2:-}"; shift 2 ;;
    --listen)  LISTEN="${2:-}"; shift 2 ;;
    --release) RELEASE="${2:-}"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown option: $1" >&2; usage ;;
  esac
done

[[ -z "$PANEL_URL" || -z "$NODE_NAME" || -z "$TOKEN" ]] && usage
[[ "$ROLE" != "entry" && "$ROLE" != "relay" ]] && { echo "role must be entry or relay" >&2; exit 1; }
[[ "$(id -u)" -eq 0 ]] || { echo "run as root" >&2; exit 1; }

PANEL_URL="${PANEL_URL%/}"

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [[ "$RELEASE" == "latest" ]]; then
  DL="${PANEL_URL}/download/smart-gateway-agent-linux-${ARCH}"
else
  DL="${PANEL_URL}/download/${RELEASE}/smart-gateway-agent-linux-${ARCH}"
fi

echo "==> downloading agent for linux/${ARCH}"
TMP="$(mktemp)"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$DL" -o "$TMP"
else
  wget -qO "$TMP" "$DL"
fi
install -m 0755 "$TMP" "${BIN_DIR}/smart-gateway-agent"
rm -f "$TMP"

echo "==> writing ${ETC_DIR}/agent.env"
install -d -m 0755 "$ETC_DIR"
umask 077
cat > "${ETC_DIR}/agent.env" <<EOF
SG_PANEL_URL=${PANEL_URL}
SG_NODE_NAME=${NODE_NAME}
SG_ROLE=${ROLE}
SG_TOKEN=${TOKEN}
SG_LISTEN=${LISTEN}
EOF
chmod 0600 "${ETC_DIR}/agent.env"

echo "==> installing systemd unit"
cat > "/etc/systemd/system/${SERVICE}.service" <<EOF
[Unit]
Description=smart-gateway agent (${NODE_NAME}, ${ROLE})
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${ETC_DIR}/agent.env
ExecStart=${BIN_DIR}/smart-gateway-agent \\
  -panel \${SG_PANEL_URL} \\
  -node \${SG_NODE_NAME} \\
  -role \${SG_ROLE} \\
  -panel-token \${SG_TOKEN} \\
  -listen \${SG_LISTEN}
Restart=always
RestartSec=3
LimitNOFILE=1048576
# The agent only needs to open outbound connections and, for entry nodes,
# listen locally, so it can run with a hardened profile.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now "${SERVICE}"
sleep 2
systemctl --no-pager --lines=15 status "${SERVICE}" || true

echo
echo "Installed. The node should appear as online in the panel within one heartbeat."
if [[ "$ROLE" == "relay" ]]; then
  echo "Open the relay port range (3001-3100) to the other gateway nodes only."
else
  echo "Point your reverse proxy at ${LISTEN}."
fi
