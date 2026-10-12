#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
mkdir -p secrets data
umask 077

if [ ! -f secrets/config.yaml ]; then
  cp configs/config.example.yaml secrets/config.yaml
  chmod 600 secrets/config.yaml
  echo "Created secrets/config.yaml from the example; fill in local GitHub/model settings as needed."
fi

if [ ! -f secrets/postgres.env ]; then
  password="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"
  cat > secrets/postgres.env <<EOF
POSTGRES_DB=aiworkflow
POSTGRES_USER=aiworkflow
POSTGRES_PASSWORD=$password
EOF
  chmod 600 secrets/postgres.env
fi

if [ ! -f secrets/control.env ]; then
  password="$(sed -n 's/^POSTGRES_PASSWORD=//p' secrets/postgres.env | head -n 1)"
  cat > secrets/control.env <<EOF
DATABASE_URL=postgres://aiworkflow:$password@postgres:5432/aiworkflow?sslmode=disable
EOF
  chmod 600 secrets/control.env
fi

if [ ! -f secrets/dashboard-token ]; then
  python3 -c 'import secrets; print(secrets.token_hex(32), end="")' > secrets/dashboard-token
  chmod 600 secrets/dashboard-token
fi

if [ -z "${DOCKER_SOCKET:-}" ]; then
  # Bind paths are resolved by the Docker daemon host (the Colima VM on
  # macOS), not by the host CLI. Use the daemon-side socket; callers may
  # explicitly override it for a nonstandard Docker Engine installation.
  DOCKER_SOCKET=/var/run/docker.sock
fi
context="${DOCKER_CONTEXT:-$(docker context show 2>/dev/null || true)}"
if [ -n "$context" ]; then DOCKER_CONTEXT="$context"; fi
export DOCKER_SOCKET
export DOCKER_CONTEXT

compose="docker-compose --env-file secrets/compose.env"
{
  printf 'DOCKER_SOCKET=%s\n' "$DOCKER_SOCKET"
  if [ -n "${DOCKER_CONTEXT:-}" ]; then printf 'DOCKER_CONTEXT=%s\n' "$DOCKER_CONTEXT"; fi
} > secrets/compose.env
chmod 600 secrets/compose.env

# Prebuild the toolchain image so ordinary attempts never need a dependency
# download connection. The profile builds the image without starting a runner.
$compose --profile images build runner-image
$compose build control
$compose up -d postgres

container_id="$(DOCKER_CONTEXT="$DOCKER_CONTEXT" docker-compose --env-file secrets/compose.env ps -q postgres)"
if [ -z "$container_id" ]; then
  echo "Postgres container was not created." >&2
  exit 1
fi
attempt=0
while [ "$attempt" -lt 60 ]; do
  health="$(DOCKER_CONTEXT="$DOCKER_CONTEXT" docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$container_id" 2>/dev/null || true)"
  if [ "$health" = healthy ]; then
    break
  fi
  if [ "$health" = unhealthy ]; then
    docker-compose --env-file secrets/compose.env logs --tail=80 postgres >&2
    echo "Postgres failed its health check." >&2
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep 2
done
if [ "$health" != healthy ]; then
  docker-compose --env-file secrets/compose.env logs --tail=80 postgres >&2
  echo "Timed out waiting for Postgres health." >&2
  exit 1
fi

$compose up -d control
echo "Dashboard: http://127.0.0.1:8081/setup; GitHub webhook endpoint: http://127.0.0.1:8080/webhooks/github."
