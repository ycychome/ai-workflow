#!/bin/sh
set -eu
. "$(dirname "$0")/alpha-common.sh"
if [ "$#" -ne 1 ]; then echo 'usage: alpha-backup.sh NEW_BACKUP_DIRECTORY' >&2; exit 2; fi
umask 077
mkdir "$1"
backup=$(CDPATH= cd -- "$1" && pwd)
# Quiesce control before taking the DB and artifact snapshot. Leave it stopped:
# upgrade/rollback is a deliberate next action, not a trap-triggered restart.
alpha_compose stop -t 120 control
active=$(alpha_active_count)
if [ "$active" != 0 ]; then
  echo 'Agent capacity is still occupied. Resume the same controller and confirm targeted recovery, then retry into a new backup directory.' >&2
  exit 1
fi
alpha_compose exec -T postgres sh -ceu 'pg_dump -Fc -U "$POSTGRES_USER" "$POSTGRES_DB"' > "$backup/postgres.dump"
alpha_compose run --rm -T --no-deps --entrypoint /bin/sh control -ceu 'tar -C /data -czf - .' > "$backup/data.tar.gz"
tar -C "$ALPHA_STATE_DIR" -czf "$backup/secrets.tar.gz" secrets
alpha_compose exec -T postgres sh -ceu 'psql -X -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT version FROM schema_migrations ORDER BY version"' > "$backup/schema-versions.txt"
control_id=$(alpha_compose ps -a -q control)
if [ -z "$control_id" ]; then echo "No control container found to bind the backup image" >&2; exit 1; fi
docker inspect --format '{{.Image}}' "$control_id" > "$backup/control-image-id.txt"
docker image inspect --format '{{.Id}}' "${ALPHA_RUNNER_IMAGE:-ai-workflow-runner:alpha}" > "$backup/runner-image-id.txt"
python3 - "$backup" <<'PY'
import hashlib,json,pathlib,sys
p=pathlib.Path(sys.argv[1])
manifest={f.name:hashlib.sha256(f.read_bytes()).hexdigest() for f in sorted(p.iterdir()) if f.is_file()}
(p/'sha256.json').write_text(json.dumps(manifest,indent=2)+'\n')
PY
printf 'Backup complete: %s. Controller remains stopped.\n' "$backup"
