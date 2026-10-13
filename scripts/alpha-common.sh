#!/bin/sh
# Sourced by alpha commands. No defaults point at the legacy secrets/ directory.
set -eu
ALPHA_REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
: "${ALPHA_STATE_DIR:?Set ALPHA_STATE_DIR to an absolute isolated state directory}"
case "$ALPHA_STATE_DIR" in /*) ;; *) echo 'ALPHA_STATE_DIR must be absolute' >&2; exit 2;; esac
ALPHA_PROJECT=${ALPHA_PROJECT:-aiwf-alpha}
case "$ALPHA_PROJECT" in aiwf-alpha|aiwf-alpha-*) ;; *) echo 'Use an aiwf-alpha or aiwf-alpha-* project name' >&2; exit 2;; esac
export ALPHA_STATE_DIR ALPHA_PROJECT
alpha_compose() {
  if docker compose version >/dev/null 2>&1; then
    docker compose -p "$ALPHA_PROJECT" -f "$ALPHA_REPO_ROOT/deploy/compose.alpha.yaml" "$@"
  elif command -v docker-compose >/dev/null 2>&1; then
    docker-compose -p "$ALPHA_PROJECT" -f "$ALPHA_REPO_ROOT/deploy/compose.alpha.yaml" "$@"
  else
    echo 'Docker Compose is required (docker compose or docker-compose).' >&2
    return 1
  fi
}
alpha_active_count() {
  exists=$(alpha_compose exec -T postgres sh -ceu 'psql -X -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT to_regclass('"'"'public.agent_runs'"'"') IS NOT NULL"')
  if [ "$exists" = f ]; then printf '0\n'; return; fi
  alpha_compose exec -T postgres sh -ceu 'psql -X -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) FROM agent_runs WHERE status IN ('"'"'ACTIVE'"'"','"'"'STOPPING'"'"')"'
}
