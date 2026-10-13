package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func claimedBoundAgent(t *testing.T, s *Store) domain.AgentRun {
	t.Helper()
	ctx := context.Background()
	seedAgentWorkflow(t, s, "w", 1)
	task := domain.Task{ID: "a", Role: "builder", Kind: "code", Paths: []string{"main.go"}, LeaseEpoch: 1, PlanRevision: 1, Status: "QUEUED"}
	if err := s.Update(ctx, "w", func(w *domain.Workflow) ([]Action, error) {
		w.Tasks = []domain.Task{task}
		w.PlanRevision = 1
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueAgent(ctx, domain.AgentRun{ID: "a", WorkflowID: "w", TaskID: "a", Role: "builder", Generation: 1, PlanRevision: 1, PromptVersion: "test-prompt-v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE workflows SET lease_owner='controller',lease_until=clock_timestamp()+interval '1 hour' WHERE id='w'`); err != nil {
		t.Fatal(err)
	}
	r, err := s.ClaimAgent(ctx, "a", "worker", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Get(ctx, "w")
	if err != nil {
		t.Fatal(err)
	}
	r.ContextHash = domain.AgentExecutionHash(w, task, r.Role, r.PromptVersion)
	if err = s.BindAgentContext(ctx, r.ID, r.Owner, r.LeaseEpoch, r.ContextHash); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAgentFenceRejectsMismatchedAndExpiredReceipts(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	r := claimedBoundAgent(t, s)
	for _, tc := range []struct {
		name   string
		mutate func(*domain.AgentRun)
	}{
		{"owner", func(r *domain.AgentRun) { r.Owner = "other" }},
		{"epoch", func(r *domain.AgentRun) { r.LeaseEpoch++ }},
		{"generation", func(r *domain.AgentRun) { r.Generation++ }},
		{"snapshot", func(r *domain.AgentRun) { r.ContextHash = "changed" }},
		{"plan", func(r *domain.AgentRun) { r.PlanRevision++ }},
		{"task", func(r *domain.AgentRun) { r.TaskID = "other" }},
		{"role", func(r *domain.AgentRun) { r.Role = "reviewer" }},
		{"prompt", func(r *domain.AgentRun) { r.PromptVersion = "" }},
		{"unbound", func(r *domain.AgentRun) { r.ContextHash = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrong := r
			tc.mutate(&wrong)
			err := s.AttemptOwned(WithAgentFence(ctx, wrong), "receipt-"+tc.name, "w", "a", 1, 1, "controller", "SUCCEEDED", map[string]string{"result": "late"}, nil)
			if !errors.Is(err, ErrStale) {
				t.Fatalf("mismatched receipt accepted: %v", err)
			}
		})
	}
	if err := s.AttemptOwned(WithAgentFence(ctx, r), "receipt-good", "w", "a", 1, 1, "controller", "SUCCEEDED", map[string]string{"result": "current"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := s.AttemptOwned(WithAgentFence(ctx, r), "receipt-late", "w", "a", 1, 1, "controller", "SUCCEEDED", nil, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("expired receipt accepted: %v", err)
	}
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM attempts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stale results persisted: %d", count)
	}
}

func TestAgentFenceUsesCurrentClockNotTransactionStart(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	r := claimedBoundAgent(t, s)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Advance the lease to a time after transaction start but before this query.
	// now() would still wrongly consider this lease live throughout the tx.
	if _, err = tx.Exec(ctx, `SELECT id FROM workflows WHERE id='w' FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_runs SET lease_until=clock_timestamp() WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if err = checkAgentFence(WithAgentFence(ctx, r), tx); !errors.Is(err, ErrStale) {
		t.Fatalf("expired-in-transaction lease accepted: %v", err)
	}
}

func TestAgentContextBindingAndQueueCancellation(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	r := claimedBoundAgent(t, s)
	if err := s.BindAgentContext(ctx, r.ID, r.Owner, r.LeaseEpoch, r.ContextHash); err != nil {
		t.Fatal(err)
	}
	if err := s.BindAgentContext(ctx, r.ID, r.Owner, r.LeaseEpoch, "new-snapshot"); !errors.Is(err, ErrStale) {
		t.Fatalf("bound snapshot replaced: %v", err)
	}
	if err := s.CancelQueuedAgent(ctx, r.ID); !errors.Is(err, ErrStale) {
		t.Fatalf("active capacity cancelled as queued: %v", err)
	}
	enqueueTestAgent(t, s, "abandoned", "w")
	enqueueTestAgent(t, s, "next", "w")
	if err := s.CancelQueuedAgent(ctx, "abandoned"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAgent(ctx, "next", "other", 2, 2); err != nil {
		t.Fatalf("cancelled head blocks queue: %v", err)
	}
}

func TestAgentQueueReasonSurvivesAdmission(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "w", 1)
	enqueueTestAgent(t, s, "a", "w")
	enqueueTestAgent(t, s, "b", "w")
	a, err := s.ClaimAgent(ctx, "a", "worker", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimAgent(ctx, "b", "other", 1, 1); !errors.Is(err, ErrAgentQueued) {
		t.Fatal(err)
	}
	if err = s.MarkAgentStopping(ctx, a.ID, a.Owner, a.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAgent(ctx, a.ID, a.Owner, a.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	b, err := s.ClaimAgent(ctx, "b", "other", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.StartReason, "重新检查工作流状态") {
		t.Fatalf("missing post-queue revalidation reason: %q", b.StartReason)
	}
	if !strings.Contains(b.QueueReason, "全局并发") {
		t.Fatalf("queue cause lost at admission: %q", b.QueueReason)
	}
}

func TestAgentFenceRejectsCurrentTaskAndSnapshotDrift(t *testing.T) {
	for _, kind := range []string{"epoch", "plan", "head", "removed", "scope"} {
		t.Run(kind, func(t *testing.T) {
			s := agentQueueTestStore(t)
			ctx := context.Background()
			r := claimedBoundAgent(t, s)
			if err := s.Update(ctx, r.WorkflowID, func(w *domain.Workflow) ([]Action, error) {
				switch kind {
				case "epoch":
					w.Tasks[0].LeaseEpoch++
				case "plan":
					w.PlanRevision++
				case "head":
					w.HeadSHA = "different-source"
				case "removed":
					w.Tasks = nil
				case "scope":
					w.Tasks[0].Paths = []string{"other.go"}
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.AttemptOwned(WithAgentFence(ctx, r), "drifted", r.WorkflowID, r.TaskID, r.Generation, 1, "controller", "OUTPUT_READY", map[string]string{"summary": "stale"}, nil); !errors.Is(err, ErrStale) {
				t.Fatalf("%s drift accepted: %v", kind, err)
			}
		})
	}
}

// BeforeRequest's earlier read is insufficient: lease revocation can occur
// before budget reservation acquires its database locks.
func TestReserveRequestRejectsRevokedAgentWithoutCharging(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	run := claimedBoundAgent(t, s)
	if err := s.CheckAgentLease(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAgentStopping(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveRequest(WithAgentFence(ctx, run), run.WorkflowID, run.Generation, 10, 100); !errors.Is(err, ErrStale) {
		t.Fatalf("revoked model request reservation accepted: %v", err)
	}
	var calls, daily int
	if err := s.DB.QueryRow(ctx, `SELECT calls,(SELECT COALESCE(sum(calls),0) FROM daily_usage) FROM workflow_metrics WHERE workflow_id=$1`, run.WorkflowID).Scan(&calls, &daily); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || daily != 0 {
		t.Fatalf("rejected run consumed budget: issue=%d daily=%d", calls, daily)
	}
}

func TestUsageStillRecordsConsumedTokensAfterAgentRevocation(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	run := claimedBoundAgent(t, s)
	if err := s.MarkAgentStopping(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err := s.Usage(WithAgentFence(ctx, run), run.WorkflowID, 123, 45); err != nil {
		t.Fatal(err)
	}
	var in, out int64
	if err := s.DB.QueryRow(ctx, `SELECT input_tokens,output_tokens FROM workflow_metrics WHERE workflow_id=$1`, run.WorkflowID).Scan(&in, &out); err != nil {
		t.Fatal(err)
	}
	if in != 123 || out != 45 {
		t.Fatalf("actual stale-session usage lost: %d/%d", in, out)
	}
}
