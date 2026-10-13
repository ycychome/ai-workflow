#!/bin/sh
# Dedicated disposable PG integration layer. Never use the deployment DB.
set -eu
if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then echo 'usage: alpha-postgres-check.sh NEW_EVIDENCE_DIRECTORY [postgres|all]' >&2; exit 2; fi
layer=${2:-postgres}
case "$layer" in
  postgres) ;;
  all) AI_WORKFLOW_FORCE_INTEGRATION=1; AI_WORKFLOW_FORCE_DOCKER=1; export AI_WORKFLOW_FORCE_INTEGRATION AI_WORKFLOW_FORCE_DOCKER ;;
  *) echo 'Supported layers: postgres, all' >&2; exit 2 ;;
esac
repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
umask 077
tmp=$(mktemp -d)
nonce=$(python3 -c 'import secrets; print(secrets.token_hex(12))')
name="aiwf-alpha-tests-$nonce"
# Docker's ephemeral port allocation occurs inside its VM. Two Colima profiles
# can otherwise publish the same host port and silently reach the older VM.
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if ! docker rm -f "$name" >/dev/null 2>&1; then
    # An unreachable daemon is not proof of process exit.
    echo "Unable to confirm disposable database removal: $name" >&2
    status=1
  fi
  rm -rf "$tmp"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
docker run -d --pull never --name "$name" --network bridge -p "127.0.0.1:$port:5432" \
  --label ai-workflow.acceptance=1 --memory 768m --memory-swap 768m --pids-limit 64 \
  --log-driver json-file --log-opt max-size=5m --log-opt max-file=1 \
  --tmpfs /var/lib/postgresql/data:rw,nosuid,nodev,size=512m \
  -e POSTGRES_USER=acceptance -e POSTGRES_DB=acceptance -e "POSTGRES_PASSWORD=$nonce" postgres:17 >/dev/null
attempt=0
until docker exec "$name" pg_isready -h 127.0.0.1 -U acceptance -d acceptance >/dev/null 2>&1; do
  attempt=$((attempt+1));if [ "$attempt" -ge 60 ]; then echo 'Acceptance DB did not become ready' >&2;exit 1;fi
  sleep 1
done
binding=$(docker port "$name" 5432/tcp)
port=${binding##*:}
case "$port" in ''|*[!0-9]*) echo 'Invalid acceptance port binding' >&2; exit 1;; esac
DATABASE_URL="postgres://acceptance:$nonce@127.0.0.1:$port/acceptance?sslmode=disable"
AGENT_QUEUE_TEST_DATABASE_URL=$DATABASE_URL
CONFIG_FILE="$tmp/config.yaml"
export DATABASE_URL AGENT_QUEUE_TEST_DATABASE_URL CONFIG_FILE
# Use the example's policy defaults but no real model/provider credentials.
python3 - "$repo" "$CONFIG_FILE" <<'PY'
from pathlib import Path
import sys
source=(Path(sys.argv[1])/'configs/config.example.yaml').read_text()
Path(sys.argv[2]).write_text(source.replace('mode: responses','mode: fake'))
PY
python3 "$repo/scripts/alpha-acceptance.py" "$layer" --output "$1"
