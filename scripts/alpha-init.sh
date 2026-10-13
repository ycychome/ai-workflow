#!/bin/sh
set -eu
. "$(dirname "$0")/alpha-common.sh"
umask 077
# Exclusive mkdir is intentional: initialization must never replace live config.
mkdir "$ALPHA_STATE_DIR"
mkdir "$ALPHA_STATE_DIR/secrets" "$ALPHA_STATE_DIR/backups"
python3 - "$ALPHA_REPO_ROOT" "$ALPHA_STATE_DIR" <<'PY'
import os, pathlib, secrets, sys
root,state=map(pathlib.Path,sys.argv[1:])
password=secrets.token_hex(32)
def write(name,text):
    p=state/'secrets'/name
    with p.open('x') as f:f.write(text)
    p.chmod(0o600)
write('postgres.env',f'POSTGRES_DB=aiworkflow\nPOSTGRES_USER=aiworkflow\nPOSTGRES_PASSWORD={password}\n')
write('control.env',f'DATABASE_URL=postgres://aiworkflow:{password}@postgres:5432/aiworkflow?sslmode=disable\n')
write('dashboard-token',secrets.token_hex(32))
text=(root/'configs/config.example.yaml').read_text()
text=text.replace('ai-workflow-runner:local',os.environ.get('ALPHA_RUNNER_IMAGE','ai-workflow-runner:alpha'))
write('config.yaml',text)
PY
printf 'Created isolated configuration in %s/secrets. Configure it before alpha-up.\n' "$ALPHA_STATE_DIR"
