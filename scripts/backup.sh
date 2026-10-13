#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
set -a
. secrets/compose.env
set +a

if [ "$#" -gt 1 ]; then
  echo "usage: $0 [backup-root]" >&2
  exit 2
fi
root="${1:-data/backups}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
dest="$root/$stamp"
umask 077
mkdir -p "$dest"

docker-compose --env-file secrets/compose.env exec -T postgres \
  sh -c 'pg_dump -Fc -U "$POSTGRES_USER" "$POSTGRES_DB"' > "$dest/postgres.dump"
docker-compose --env-file secrets/compose.env exec -T control \
  tar -C /data -czf - . > "$dest/data.tar.gz"
echo "Backup written to $dest"
