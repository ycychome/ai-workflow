package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
)

func intakeTestComment(w domain.Workflow) Action {
	return NewAction(w, "comment", map[string]any{"title": w.IssueTitle})
}

func TestIntakeAtomicRollbackReplayAndConcurrentDelivery(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	w := domain.Workflow{ID: "atomic-intake", RepositoryID: 1, Issue: 901, IssueTitle: "original"}
	if _, err := s.CreateIntake(ctx, w, func(w domain.Workflow) Action { a := intakeTestComment(w); a.Data = []byte("invalid json"); return a }); err == nil {
		t.Fatal("invalid action committed workflow")
	}
	if _, err := s.Get(ctx, w.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("workflow survived rollback: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.CreateIntake(ctx, w, intakeTestComment); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id=$1 AND kind='comment'`, w.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("initial comments=%d err=%v", count, err)
	}
	// An old process committed CreateIfAbsent then died before creating the action.
	legacy := domain.Workflow{ID: "legacy-intake", RepositoryID: 1, Issue: 902, IssueTitle: "persisted snapshot"}
	if _, err := s.CreateIfAbsent(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	legacy.IssueTitle = "stale webhook"
	if created, err := s.CreateIntake(ctx, legacy, intakeTestComment); err != nil || created {
		t.Fatalf("legacy repair created=%v err=%v", created, err)
	}
	var title string
	if err := s.DB.QueryRow(ctx, `SELECT data->>'title' FROM outbox WHERE workflow_id=$1 AND kind='comment'`, legacy.ID).Scan(&title); err != nil || title != "persisted snapshot" {
		t.Fatalf("repair title=%q err=%v", title, err)
	}
}

func TestRepositoryRetirementFencesOldEpochAndPreservesHistory(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	w := domain.Workflow{ID: "old-epoch", RepositoryID: 1, Issue: 911}
	if _, err := s.CreateIntake(ctx, w, intakeTestComment); err != nil {
		t.Fatal(err)
	}
	enqueueTestAgent(t, s, "retirement-live", w.ID)
	run, err := s.ClaimAgent(ctx, "retirement-live", "controller", 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	plan := RepositoryMaintenance{RepositoryID: 1, Token: "0123456789abcdef0123456789abcdef", OldEpoch: "legacy", NewEpoch: "reset-1", PlanDigest: "reviewed-reset-plan"}
	if _, err = s.AcquireRepositoryMaintenance(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err = s.RetireRepositoryMaintenance(ctx, plan); !errors.Is(err, ErrRepositoryBusy) {
		t.Fatalf("active retirement=%v", err)
	}
	if err = s.MarkAgentStopping(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err = s.RetireRepositoryMaintenance(ctx, plan); !errors.Is(err, ErrRepositoryBusy) {
		t.Fatalf("unacked retirement=%v", err)
	}
	if err = s.FinishAgent(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "unknown"} {
		if _, err = s.DB.Exec(ctx, `UPDATE outbox SET state=$2 WHERE workflow_id=$1 AND kind='comment'`, w.ID, state); err != nil {
			t.Fatal(err)
		}
		if err = s.RetireRepositoryMaintenance(ctx, plan); !errors.Is(err, ErrRepositoryBusy) {
			t.Fatalf("%s retirement=%v", state, err)
		}
	}
	if _, err = s.DB.Exec(ctx, `UPDATE outbox SET state='pending' WHERE workflow_id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RetireRepositoryMaintenance(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseRepositoryMaintenance(ctx, plan, false); err != nil {
		t.Fatal(err)
	}
	retired, err := s.IsRetiredWorkflow(ctx, w.ID)
	if err != nil || !retired {
		t.Fatalf("retired=%v err=%v", retired, err)
	}
	old, err := s.Get(ctx, w.ID)
	if err != nil || old.Control != "CANCELLED" || old.Generation != 2 {
		t.Fatalf("history=%+v err=%v", old, err)
	}
	if _, err = s.CreateIntake(ctx, w, intakeTestComment); !errors.Is(err, ErrStale) {
		t.Fatalf("old webhook=%v", err)
	}
	w.RepositoryEpoch = "reset-1"
	if _, err = s.CreateIntake(ctx, w, intakeTestComment); !errors.Is(err, ErrStale) {
		t.Fatalf("retired ID revival=%v", err)
	}
	w.ID = "fresh-epoch"
	w.Issue++
	if created, err := s.CreateIntake(ctx, w, intakeTestComment); err != nil || !created {
		t.Fatalf("new epoch created=%v err=%v", created, err)
	}
	if err = s.RetireRepositoryMaintenance(ctx, plan); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Get(ctx, w.ID)
	if err != nil || fresh.Control != "ACTIVE" {
		t.Fatalf("idempotent reset changed new workflow: %+v %v", fresh, err)
	}
	var obsolete int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id='old-epoch' AND state<>'obsolete'`).Scan(&obsolete); err != nil || obsolete != 0 {
		t.Fatalf("old actionable writes=%d %v", obsolete, err)
	}
}

func TestQueuedControllerRestartCannotBlockFairQueue(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "restart", 921)
	if _, err := s.DB.Exec(ctx, `UPDATE workflows SET lease_owner='dead-controller',lease_until=clock_timestamp()+interval '90 seconds' WHERE id='restart'`); err != nil {
		t.Fatal(err)
	}
	old := domain.AgentRun{ID: "old-head", WorkflowID: "restart", TaskID: "task-old", Role: "builder", Generation: 1, Owner: "dead-controller"}
	if err := s.EnqueueAgent(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAgent(ctx, old.ID, "other-controller", 4, 2); !errors.Is(err, ErrStale) {
		t.Fatalf("stolen queue ownership=%v", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE workflows SET lease_owner='new-controller' WHERE id='restart'`); err != nil {
		t.Fatal(err)
	}
	next := old
	next.ID = "new-head"
	next.TaskID = "task-new"
	next.Owner = "new-controller"
	if err := s.EnqueueAgent(ctx, next); err != nil {
		t.Fatal(err)
	}
	run, err := s.ClaimAgent(ctx, next.ID, next.Owner, 4, 2)
	if err != nil {
		t.Fatalf("dead queue head blocked new owner: %v", err)
	}
	if _, err = s.OrphanAgentRuns(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = s.DB.QueryRow(ctx, `SELECT status FROM agent_runs WHERE id=$1`, old.ID).Scan(&state); err != nil || state != "OBSOLETE" {
		t.Fatalf("old queue=%s %v", state, err)
	}
	// Expired workflow ownership cancels only queued consumers, never live capacity.
	third := next
	third.ID = "expired-consumer"
	third.TaskID = "third"
	if err = s.EnqueueAgent(ctx, third); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE workflows SET lease_until=clock_timestamp()-interval '1 second' WHERE id='restart'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.OrphanAgentRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT status FROM agent_runs WHERE id=$1`, third.ID).Scan(&state); err != nil || state != "OBSOLETE" {
		t.Fatalf("expired queue=%s %v", state, err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT status FROM agent_runs WHERE id=$1`, run.ID).Scan(&state); err != nil || state != "ACTIVE" {
		t.Fatalf("live capacity changed without ack=%s %v", state, err)
	}
}
