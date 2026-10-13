#!/bin/sh
set -eu
. "$(dirname "$0")/alpha-common.sh"
test -f "$ALPHA_STATE_DIR/secrets/config.yaml"
# Require prepared images. This command never silently downloads dependencies.
docker image inspect postgres:16-bookworm >/dev/null
docker image inspect "${ALPHA_RUNNER_IMAGE:-ai-workflow-runner:alpha}" >/dev/null
docker image inspect "${ALPHA_CONTROL_IMAGE:-ai-workflow-control:alpha}" >/dev/null
alpha_compose up -d --no-build --pull never --wait postgres control
printf 'Alpha is available on loopback ports %s and %s.\n' "${ALPHA_HTTP_PORT:-18080}" "${ALPHA_SETUP_PORT:-18081}"
