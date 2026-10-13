#!/bin/sh
set -eu
. "$(dirname "$0")/alpha-common.sh"
if [ "$#" -ne 2 ] || [ "$1" != --replace-alpha ]; then
  echo 'usage: alpha-restore.sh --replace-alpha BACKUP_DIRECTORY' >&2; exit 2
fi
backup=$(CDPATH= cd -- "$2" && pwd)
python3 - "$backup" <<'PY'
import hashlib,json,pathlib,sys,tarfile,os
p=pathlib.Path(sys.argv[1]);manifest=json.loads((p/'sha256.json').read_text())
for name in ('postgres.dump','data.tar.gz','secrets.tar.gz','schema-versions.txt','control-image-id.txt','runner-image-id.txt'):
    if hashlib.sha256((p/name).read_bytes()).hexdigest()!=manifest[name]:raise SystemExit('Backup hash mismatch: '+name)
with tarfile.open(p/'secrets.tar.gz') as archive:
    saved=archive.extractfile('secrets/postgres.env').read()
    current=(pathlib.Path(os.environ['ALPHA_STATE_DIR'])/'secrets/postgres.env').read_bytes()
    if saved!=current:raise SystemExit('Backup DB credentials differ; restore to a separately provisioned deployment instead')
# Reject archives whose paths or links can escape the restore roots.
for name in ('data.tar.gz','secrets.tar.gz'):
    with tarfile.open(p/name) as archive:
        for member in archive.getmembers():
            path=pathlib.PurePosixPath(member.name)
            if path.is_absolute() or '..' in path.parts or not (member.isfile() or member.isdir()):raise SystemExit('Unsafe backup archive member')
PY
docker image inspect "$(cat "$backup/control-image-id.txt")" >/dev/null
docker image inspect "$(cat "$backup/runner-image-id.txt")" >/dev/null
alpha_compose stop -t 120 control
active=$(alpha_active_count)
if [ "$active" != 0 ]; then echo 'Refusing restore while run capacity remains occupied.' >&2; exit 1; fi
alpha_compose exec -T postgres sh -ceu 'dropdb --maintenance-db=postgres --if-exists -U "$POSTGRES_USER" "$POSTGRES_DB"; createdb -U "$POSTGRES_USER" "$POSTGRES_DB"'
alpha_compose exec -T postgres sh -ceu 'pg_restore --exit-on-error -U "$POSTGRES_USER" -d "$POSTGRES_DB"' < "$backup/postgres.dump"
alpha_compose run --rm -T --no-deps --entrypoint /bin/sh control -ceu 'find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} +; tar -xzf - -C /data' < "$backup/data.tar.gz"
# Keep a second copy of the pre-restore secrets for recovery; do not print them.
mv "$ALPHA_STATE_DIR/secrets" "$ALPHA_STATE_DIR/secrets.before-restore-$(date -u +%Y%m%dT%H%M%SZ)"
tar -C "$ALPHA_STATE_DIR" -xzf "$backup/secrets.tar.gz"
printf '%s\n' 'Restored DB, artifacts, and configuration. Controller remains stopped; select the backed-up image ID before alpha-up.'
