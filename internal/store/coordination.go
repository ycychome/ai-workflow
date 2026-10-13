package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
)

// UpdateCoordination atomically persists controller-owned coordination ledgers
// and their audit event. It deliberately creates no external publication action.
// Callers must not change workflow identity, phase, control or generation here.
func (s *Store) UpdateCoordination(ctx context.Context, id, kind string, fn func(*domain.Workflow) (any, error)) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	w, err := scan(tx.QueryRow(ctx, workflowSelect+" WHERE w.id=$1 FOR UPDATE OF w", id))
	if err != nil {
		return err
	}
	if err = checkWorkflowNotRetired(ctx, tx, id); err != nil {
		return err
	}
	if fence, ok := AgentFenceFromContext(ctx); ok && fence.WorkflowID != id {
		return ErrStale
	}
	if err := checkAgentFence(ctx, tx); err != nil {
		return err
	}
	if err := checkCoordinationRun(ctx, tx, id); err != nil {
		return err
	}
	if expected, ok := ctx.Value(expectedStateContextKey{}).(expectedWorkflowState); ok && (w.Generation != expected.Generation || w.HeadSHA != expected.HeadSHA) {
		return ErrStale
	}
	if key := commandKey(ctx); key != "" {
		tag, err := tx.Exec(ctx, `INSERT INTO processed_commands(command_id,workflow_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, key, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
	}
	before := w
	audit, err := fn(&w)
	if errors.Is(err, ErrNoChange) {
		return nil
	}
	if err != nil {
		return err
	}
	if w.ID != before.ID || w.Generation != before.Generation || w.Phase != before.Phase || w.Control != before.Control || w.HeadSHA != before.HeadSHA || w.BaseSHA != before.BaseSHA || w.Spec.Digest != before.Spec.Digest {
		return errors.New("协调记录不得修改工作流身份或控制状态")
	}
	w.Version++
	w.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(w)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE workflows SET data=$2,version=$3,updated_at=now() WHERE id=$1`, id, encoded, w.Version); err != nil {
		return err
	}
	payload, err := json.Marshal(audit)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit(workflow_id,kind,data) VALUES($1,$2,$3)`, id, kind, payload); err != nil {
		return err
	}
	if err := checkAgentFence(ctx, tx); err != nil {
		return err
	}
	if err := checkCoordinationRun(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type coordinationRunContextKey struct{}

// WithCoordinationRun carries the identity of the producer whose result changes
// a ledger. Finished runs need a previously accepted attempt receipt; live runs
// retain their ordinary lease fence through the commit.
func WithCoordinationRun(ctx context.Context, run domain.AgentRun) context.Context {
	return context.WithValue(ctx, coordinationRunContextKey{}, run)
}

func acceptedCoordinationStates(role string) []string {
	if role == "reviewer" || role == "skeptic" {
		return []string{"ACCEPTED"}
	}
	return []string{"OUTPUT_READY", "ACCEPTED"}
}
func (s *Store) HasAcceptedAgentAttempt(ctx context.Context, run domain.AgentRun) (bool, error) {
	var accepted bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attempts WHERE id=$1 AND workflow_id=$2 AND intent_id=$3 AND generation=$4 AND state=ANY($5::text[]) AND report IS NOT NULL AND report<>'null'::jsonb)`, run.ID, run.WorkflowID, run.TaskID, run.Generation, acceptedCoordinationStates(run.Role)).Scan(&accepted)
	return accepted, err
}
func checkCoordinationRun(ctx context.Context, tx pgx.Tx, workflowID string) error {
	run, ok := ctx.Value(coordinationRunContextKey{}).(domain.AgentRun)
	if !ok {
		return nil
	}
	if run.WorkflowID != workflowID {
		return ErrStale
	}
	if run.Status == domain.AgentActive {
		return checkAgentFence(WithAgentFence(ctx, run), tx)
	}
	if run.Status != domain.AgentFinished {
		return ErrStale
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM agent_runs WHERE id=$1 AND workflow_id=$2 AND generation=$3 AND task_id=$4 AND role=$5 FOR SHARE`, run.ID, workflowID, run.Generation, run.TaskID, run.Role).Scan(&status); err != nil {
		return err
	}
	if status != domain.AgentFinished {
		return ErrStale
	}
	var receiptID string
	err := tx.QueryRow(ctx, `SELECT id FROM attempts WHERE id=$1 AND workflow_id=$2 AND intent_id=$3 AND generation=$4 AND state=ANY($5::text[]) AND report IS NOT NULL AND report<>'null'::jsonb FOR SHARE`, run.ID, workflowID, run.TaskID, run.Generation, acceptedCoordinationStates(run.Role)).Scan(&receiptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStale
	}
	return err
}
