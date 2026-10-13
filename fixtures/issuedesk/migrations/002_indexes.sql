CREATE INDEX tickets_page_idx ON tickets(tenant_id,created_at DESC,id DESC) WHERE deleted_at IS NULL;
CREATE INDEX tickets_creator_idx ON tickets(tenant_id,creator_id) WHERE deleted_at IS NULL;
CREATE INDEX comments_ticket_idx ON comments(tenant_id,ticket_id,id);
