#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
set -a
. secrets/compose.env
set +a

if [ "$#" -ne 2 ] || [ "$1" != "--yes" ]; then
  echo "usage: $0 --yes backup-directory" >&2
  echo "This replaces the current PostgreSQL database and /data contents." >&2
  exit 2
fi
backup="$2"
if [ ! -f "$backup/postgres.dump" ] || [ ! -f "$backup/data.tar.gz" ]; then
  echo "backup directory must contain postgres.dump and data.tar.gz" >&2
  exit 2
fi

docker-compose --env-file secrets/compose.env stop control
docker-compose --env-file secrets/compose.env exec -T postgres sh -c \
	'dropdb --maintenance-db=postgres --if-exists -U "$POSTGRES_USER" "$POSTGRES_DB" && createdb -U "$POSTGRES_USER" "$POSTGRES_DB"'
docker-compose --env-file secrets/compose.env exec -T postgres sh -c \
  'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB"' < "$backup/postgres.dump"
docker-compose --env-file secrets/compose.env run --rm -T --no-deps \
  --entrypoint /bin/sh control -c \
  'find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} + && tar -xzf - -C /data' < "$backup/data.tar.gz"
docker-compose --env-file secrets/compose.env up -d control
echo "Restored database and /data from $backup"
