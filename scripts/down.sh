#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
set -a
. secrets/compose.env
set +a
docker-compose --env-file secrets/compose.env down
