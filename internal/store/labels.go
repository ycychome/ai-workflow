package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
)

// Labels are informational effects: they continue while paused/cancelled and
// are claimed separately so a GitHub label failure cannot hold up code or gates.
func queueLabelSync(ctx context.Context, tx pgx.Tx, w domain.Workflow) error {
	if w.Repository == "local/pilot" || w.InstallationID <= 0 || w.Issue <= 0 {
		return nil
	}
	a := NewAction(w, "labels", map[string]any{"phase": w.Phase, "control": w.Control, "pr": w.PR})
	_, err := tx.Exec(ctx, `INSERT INTO outbox(id,workflow_id,generation,kind,data)
 SELECT $1,$2,$3,'labels',$4::jsonb
 WHERE NOT EXISTS (
 SELECT 1 FROM (SELECT data,state FROM outbox WHERE workflow_id=$2 AND kind='labels' ORDER BY created_at DESC,id DESC LIMIT 1) last
 WHERE last.data=$4::jsonb AND last.state<>'dead_letter'
 ) ON CONFLICT DO NOTHING`, a.ID, a.WorkflowID, a.Generation, a.Data)
	return err
}

// QueueLabelRepair backfills existing workflows and repairs manually removed
// labels after outages. It does not change workflow state, leases or metrics.
func (s *Store) QueueLabelRepair(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO outbox(id,workflow_id,generation,kind,data)
 SELECT w.id||':labels-repair:'||floor(extract(epoch FROM now())/300)::bigint::text,w.id,w.generation,'labels',
 jsonb_build_object('phase',w.phase,'control',w.control,'pr',COALESCE((w.data->>'pr')::integer,0))
 FROM workflows w
 WHERE w.data->>'repository'<>'local/pilot'
 AND COALESCE((w.data->>'installation_id')::bigint,0)>0 AND w.issue>0
 AND NOT EXISTS(SELECT 1 FROM outbox o WHERE o.workflow_id=w.id AND o.kind='labels' AND o.state IN('pending','retry_wait','unknown','running'))
 ON CONFLICT DO NOTHING`)
	return err
}

func (s *Store) ClaimLabelAction(ctx context.Context) (Action, error) {
	var a Action
	err := s.DB.QueryRow(ctx, `WITH next AS (
 SELECT o.id FROM outbox o WHERE o.kind='labels'
 AND o.state IN('pending','retry_wait','unknown','running')
 AND o.next_attempt<=now() AND (o.lease_until IS NULL OR o.lease_until<now())
 AND NOT EXISTS(SELECT 1 FROM outbox earlier WHERE earlier.workflow_id=o.workflow_id AND earlier.kind='labels'
 AND (earlier.created_at<o.created_at OR (earlier.created_at=o.created_at AND earlier.id<o.id))
 AND earlier.state IN('pending','retry_wait','unknown','running'))
 ORDER BY o.created_at,o.id FOR UPDATE SKIP LOCKED LIMIT 1
 ) UPDATE outbox SET state='running',lease_until=now()+interval '90 seconds',attempts=attempts+1
 FROM next WHERE outbox.id=next.id
 RETURNING outbox.id,workflow_id,generation,kind,data,attempts`).Scan(&a.ID, &a.WorkflowID, &a.Generation, &a.Kind, &a.Data, &a.Attempts)
	return a, err
}
