package store

import (
	"context"
	"errors"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestCoordinationWriteRejectsExpiredAgentFence(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	run := claimedBoundAgent(t, s)
	if _, err := s.DB.Exec(ctx, `UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	changed := false
	err := s.UpdateCoordination(WithAgentFence(ctx, run), run.WorkflowID, "late_coordination", func(w *domain.Workflow) (any, error) { changed = true; return nil, nil })
	if !errors.Is(err, ErrStale) || changed {
		t.Fatalf("expired run changed ledger: changed=%v err=%v", changed, err)
	}
}
func TestFinishedCoordinationRunRequiresAcceptedReceipt(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	run := claimedBoundAgent(t, s)
	if _, err := s.DB.Exec(ctx, `UPDATE agent_runs SET status='FINISHED' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	run.Status = domain.AgentFinished
	update := func(w *domain.Workflow) (any, error) {
		w.EvidenceRequests = append(w.EvidenceRequests, domain.EvidenceRequest{ID: "record", Status: "QUEUED"})
		return map[string]string{"id": "record"}, nil
	}
	if err := s.UpdateCoordination(WithCoordinationRun(ctx, run), run.WorkflowID, "coordination_receipt", update); !errors.Is(err, ErrStale) {
		t.Fatalf("missing accepted receipt allowed: %v", err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO attempts(id,workflow_id,intent_id,generation,lease_epoch,state,report,evidence) VALUES($1,$2,$3,$4,1,'BLOCKED','{}','[]')`, run.ID, run.WorkflowID, run.TaskID, run.Generation); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateCoordination(WithCoordinationRun(ctx, run), run.WorkflowID, "coordination_receipt", update); !errors.Is(err, ErrStale) {
		t.Fatalf("blocked receipt allowed: %v", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE attempts SET state='ACCEPTED' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateCoordination(WithCoordinationRun(ctx, run), run.WorkflowID, "coordination_receipt", update); err != nil {
		t.Fatalf("accepted receipt rejected: %v", err)
	}
}
