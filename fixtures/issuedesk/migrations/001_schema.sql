CREATE TABLE tenants (id BIGINT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE users (
 id BIGINT PRIMARY KEY, tenant_id BIGINT NOT NULL REFERENCES tenants(id),
 name TEXT NOT NULL, role TEXT NOT NULL CHECK (role IN ('requester','agent','admin')),
 token_hash TEXT UNIQUE NOT NULL, UNIQUE (tenant_id,id)
);
CREATE TABLE tickets (
 id BIGSERIAL PRIMARY KEY, tenant_id BIGINT NOT NULL REFERENCES tenants(id),
 title TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
 body TEXT NOT NULL CHECK (length(body)<=20000),
 status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','in_progress','resolved','closed','cancelled')),
 creator_id BIGINT NOT NULL, assignee_id BIGINT,
 version BIGINT NOT NULL DEFAULT 1 CHECK (version>0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(), deleted_at TIMESTAMPTZ,
 UNIQUE (tenant_id,id),
 FOREIGN KEY (tenant_id,creator_id) REFERENCES users(tenant_id,id),
 FOREIGN KEY (tenant_id,assignee_id) REFERENCES users(tenant_id,id)
);
CREATE TABLE comments (
 id BIGSERIAL PRIMARY KEY, tenant_id BIGINT NOT NULL, ticket_id BIGINT NOT NULL,
 author_id BIGINT NOT NULL, body TEXT NOT NULL CHECK (length(body) BETWEEN 1 AND 10000),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY (tenant_id,ticket_id) REFERENCES tickets(tenant_id,id),
 FOREIGN KEY (tenant_id,author_id) REFERENCES users(tenant_id,id)
);
CREATE TABLE idempotency_keys (
 tenant_id BIGINT NOT NULL, actor_id BIGINT NOT NULL, key TEXT NOT NULL,
 request_hash TEXT NOT NULL, status INTEGER NOT NULL, response JSONB NOT NULL,
 PRIMARY KEY (tenant_id,actor_id,key),
 FOREIGN KEY (tenant_id,actor_id) REFERENCES users(tenant_id,id)
);
