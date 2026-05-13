#!/usr/bin/env bash
# buildAndUpRun.sh — cross-compile argus-app for OpenWrt aarch64,
# upload to the router, and restart it. Equivalent to the manual
# build+deploy chain used during development.
#
# Required: sshpass, /usr/local/go/bin/go (or any Go 1.25+ in PATH).
#
# Configurable via env:
#   ROUTER_HOST  default 192.168.1.1
#   ROUTER_USER  default root
#   ROUTER_PASS  default xxxxxx
#   ROUTER_PORT  default 22
#   LISTEN_ADDR  default 0.0.0.0:9099
#   GO_BIN       default /usr/local/go/bin/go (falls back to "go" on PATH)

set -euo pipefail

cd "$(dirname "$0")"

ROUTER_HOST="${ROUTER_HOST:-192.168.1.1}"
ROUTER_USER="${ROUTER_USER:-root}"
ROUTER_PASS="${ROUTER_PASS:-xxxxxxx}"
ROUTER_PORT="${ROUTER_PORT:-22}"
LISTEN_ADDR="${LISTEN_ADDR:-0.0.0.0:9099}"
GO_BIN="${GO_BIN:-/usr/local/go/bin/go}"
if ! [ -x "$GO_BIN" ]; then
  GO_BIN="$(command -v go || true)"
fi
if [ -z "$GO_BIN" ]; then
  echo "go not found; install Go 1.25+ or set GO_BIN" >&2
  exit 1
fi
if ! command -v sshpass >/dev/null 2>&1; then
  echo "sshpass not found; install via 'brew install hudochenkov/sshpass/sshpass'" >&2
  exit 1
fi

BIN=/tmp/argus-app
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p "$ROUTER_PORT")
SCP_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -P "$ROUTER_PORT")

step() { printf '\n\033[36m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*"; }

step "cross-compile linux/arm64"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO_BIN" build \
  -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/app
ls -lh "$BIN"

step "stop existing process on router"
sshpass -p "$ROUTER_PASS" ssh "${SSH_OPTS[@]}" "$ROUTER_USER@$ROUTER_HOST" \
  'killall argus-app 2>/dev/null; sleep 1; rm -f /tmp/argus-app'

step "upload binary"
sshpass -p "$ROUTER_PASS" scp "${SCP_OPTS[@]}" "$BIN" \
  "$ROUTER_USER@$ROUTER_HOST:/tmp/argus-app"

step "start argus-app -listen=$LISTEN_ADDR"
sshpass -p "$ROUTER_PASS" ssh "${SSH_OPTS[@]}" "$ROUTER_USER@$ROUTER_HOST" \
  "chmod +x /tmp/argus-app && nohup /tmp/argus-app -listen=$LISTEN_ADDR </dev/null >/tmp/argus-app.log 2>&1 & sleep 2; pidof argus-app"

step "done — http://$ROUTER_HOST:9099"
