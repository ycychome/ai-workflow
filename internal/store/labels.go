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
 AND NOT EXISTS(SELECT 1 FROM retired_workflow_ids r WHERE r.workflow_id=w.id)
 AND NOT EXISTS(SELECT 1 FROM repository_maintenance m WHERE m.repository_id=w.repository_id AND (m.held OR m.intake_stopped))
 AND COALESCE((w.data->>'installation_id')::bigint,0)>0 AND w.issue>0
 AND NOT EXISTS(SELECT 1 FROM outbox o WHERE o.workflow_id=w.id AND o.kind='labels' AND o.state IN('pending','retry_wait','unknown','running'))
 ON CONFLICT DO NOTHING`)
	return err
}

func (s *Store) ClaimLabelAction(ctx context.Context) (Action, error) {
	return s.claimRepositoryAction(ctx, true)
}
