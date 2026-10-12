CREATE TABLE IF NOT EXISTS inbox (
 delivery_id text PRIMARY KEY, event text NOT NULL, payload bytea NOT NULL,
 status text NOT NULL DEFAULT 'received', epoch bigint NOT NULL DEFAULT 0,
 attempts integer NOT NULL DEFAULT 0, lease_until timestamptz,
 available_at timestamptz NOT NULL DEFAULT now(), last_error text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS workflows (
 id text PRIMARY KEY, repository_id bigint NOT NULL, data jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS outbox (
 action_id text PRIMARY KEY, workflow_id text NOT NULL REFERENCES workflows(id),
 generation bigint NOT NULL, epoch bigint NOT NULL, kind text NOT NULL,
 data jsonb NOT NULL, status text NOT NULL DEFAULT 'pending', receipt jsonb,
 attempts integer NOT NULL DEFAULT 0, lease_until timestamptz,
 available_at timestamptz NOT NULL DEFAULT now(), last_error text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS inbox_pending ON inbox(status,available_at);
CREATE INDEX IF NOT EXISTS outbox_pending ON outbox(status,available_at);
CREATE INDEX IF NOT EXISTS workflow_repository ON workflows(repository_id);
CREATE TABLE IF NOT EXISTS run_evidence (
 digest text PRIMARY KEY, workflow_id text NOT NULL REFERENCES workflows(id),
 attempt_id text NOT NULL, generation bigint NOT NULL, epoch bigint NOT NULL,
 path text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
