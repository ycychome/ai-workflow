package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
)

// Acceptance tests use the same opt-in disposable-schema harness as the queue
// suite. The matrix runner treats skipped prerequisites as incomplete.
func TestAcceptanceT05ThreeProcessesAndStoppingCapacity(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	for n := 0; n < 3; n++ {
		seedAgentWorkflow(t, s, fmt.Sprintf("w%d", n), n+1)
	}
	for n := 0; n < 20; n++ {
		enqueueTestAgent(t, s, fmt.Sprintf("run-%02d", n), fmt.Sprintf("w%d", n%3))
	}
	commands := make([]*exec.Cmd, 3)
	for n := range commands {
		commands[n] = exec.Command(os.Args[0], "-test.run=^TestAlphaAcceptanceQueueProcess$")
		commands[n].Env = append(os.Environ(), "ALPHA_ACCEPTANCE_QUEUE_CHILD=1", fmt.Sprintf("ALPHA_ACCEPTANCE_OWNER=process-%d", n), "AGENT_QUEUE_CHILD=1")
		if err := commands[n].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("queue process: %v", err)
		}
	}
	runs, err := s.ListAgentRuns(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	active := []domain.AgentRun{}
	per := map[string]int{}
	for _, r := range runs {
		if r.Status == domain.AgentActive {
			active = append(active, r)
			per[r.WorkflowID]++
		} else if !r.StartedAt.IsZero() {
			t.Fatal("queued time counted as execution")
		}
	}
	if len(active) != 4 || len(per) != 3 {
		t.Fatalf("global/fairness: active=%d per=%v", len(active), per)
	}
	for wf, n := range per {
		if n > 2 {
			t.Fatalf("workflow %s active=%d", wf, n)
		}
	}
	expired := active[0]
	if _, err = s.DB.Exec(ctx, `UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	orphan, err := s.OrphanAgentRuns(ctx)
	if err != nil || len(orphan) != 1 {
		t.Fatalf("orphan %d %v", len(orphan), err)
	}
	for _, r := range runs {
		if r.Status == domain.AgentQueued {
			if _, err = s.ClaimAgent(ctx, r.ID, "recovery", 4, 2); !errors.Is(err, ErrAgentQueued) {
				t.Fatalf("expiry released actual capacity: %v", err)
			}
		}
	}
	if err = s.FinishAgent(ctx, expired.ID, expired.Owner, expired.LeaseEpoch+1); !errors.Is(err, ErrStale) {
		t.Fatal("stale stop acknowledgement accepted")
	}
	if err = s.FinishAgent(ctx, expired.ID, expired.Owner, expired.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	admitted := 0
	for _, r := range runs {
		if r.Status == domain.AgentQueued {
			_, err = s.ClaimAgent(ctx, r.ID, "recovery", 4, 2)
			if err == nil {
				admitted++
			} else if !errors.Is(err, ErrAgentQueued) {
				t.Fatal(err)
			}
		}
	}
	if admitted != 1 {
		t.Fatalf("stop acknowledgement freed %d slots, want 1", admitted)
	}
}

func TestAlphaAcceptanceQueueProcess(t *testing.T) {
	if os.Getenv("ALPHA_ACCEPTANCE_QUEUE_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	s := agentQueueTestStore(t)
	ctx := context.Background()
	for round := 0; round < 8; round++ {
		for n := 0; n < 20; n++ {
			_, err := s.ClaimAgent(ctx, fmt.Sprintf("run-%02d", n), os.Getenv("ALPHA_ACCEPTANCE_OWNER"), 4, 2)
			if err != nil && !errors.Is(err, ErrAgentQueued) && !errors.Is(err, ErrStale) {
				t.Fatal(err)
			}
			var count int
			if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE status IN ('ACTIVE','STOPPING')`).Scan(&count); err != nil || count > 4 {
				t.Fatalf("cross-process cap: %d %v", count, err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAcceptanceT08ExpiredAndWrongSessionCannotPersist(t *testing.T) {
	TestAgentFenceRejectsMismatchedAndExpiredReceipts(t)
}

func TestAcceptanceT26BudgetPersistsAcrossGenerationAndNewWorkers(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "budget", 1)
	if err := s.ReserveRequest(ctx, "budget", 1, 2, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "budget", func(w *domain.Workflow) ([]Action, error) { w.Generation++; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveRequest(ctx, "budget", 2, 2, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveRequest(ctx, "budget", 2, 2, 10); err == nil {
		t.Fatal("generation reset issue budget")
	}
	seedAgentWorkflow(t, s, "another", 2)
	if err := s.ReserveRequest(ctx, "another", 1, 2, 2); err == nil {
		t.Fatal("new workflow reset daily budget")
	}
	var calls int
	if err := s.DB.QueryRow(ctx, `SELECT calls FROM workflow_metrics WHERE workflow_id='budget'`).Scan(&calls); err != nil || calls != 2 {
		t.Fatalf("budget=%d %v", calls, err)
	}
}
