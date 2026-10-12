#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

if ! command -v cloudflared >/dev/null 2>&1; then
  echo "cloudflared is required; run scripts/bootstrap-macos.sh first." >&2
  exit 1
fi
python3 scripts/webhook_proxy.py &
proxy_pid=$!
cleanup() {
  kill "$proxy_pid" 2>/dev/null || true
  wait "$proxy_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
sleep 1
echo "Only POST /webhooks/github is forwarded. Setup and health endpoints remain local."
cloudflared tunnel --url http://127.0.0.1:8082
