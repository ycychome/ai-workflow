package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ycychome/ai-workflow/internal/domain"
)

// Set AGENT_QUEUE_TEST_DATABASE_URL to a disposable PostgreSQL database.
// Each parent test uses a private schema; subprocesses share that schema.
func agentQueueTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("AGENT_QUEUE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGENT_QUEUE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	schema := os.Getenv("AGENT_QUEUE_TEST_SCHEMA")
	if schema == "" {
		admin, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
		if err != nil {
			t.Fatal("connect test database:", err)
		}
		schema = fmt.Sprintf("agent_queue_%d", time.Now().UnixNano())
		if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Setenv("AGENT_QUEUE_TEST_SCHEMA", schema)
		t.Cleanup(func() {
			_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			admin.Close()
		})
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("connect test database:", err)
	}
	s := &Store{DB: pool}
	t.Cleanup(s.Close)
	if os.Getenv("AGENT_QUEUE_CHILD") != "1" {
		if err = s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err = s.MigrateAgentQueue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func seedAgentWorkflow(t *testing.T, s *Store, id string, issue int) {
	t.Helper()
	if err := s.Create(context.Background(), domain.Workflow{ID: id, RepositoryID: 1, Issue: issue, Control: "ACTIVE", Phase: "DEVELOPING"}); err != nil {
		t.Fatal(err)
	}
}
func enqueueTestAgent(t *testing.T, s *Store, id, wf string) {
	t.Helper()
	if err := s.EnqueueAgent(context.Background(), domain.AgentRun{ID: id, WorkflowID: wf, TaskID: id, Role: "builder", Generation: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentQueueLeaseExpiryKeepsCapacity(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "w", 1)
	enqueueTestAgent(t, s, "a", "w")
	enqueueTestAgent(t, s, "b", "w")
	r, err := s.ClaimAgent(ctx, "a", "owner", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE agent_runs SET lease_until=now()-interval '1 second' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if err = s.RenewAgent(ctx, r.ID, r.Owner, r.LeaseEpoch); !errors.Is(err, ErrStale) {
		t.Fatalf("expired renewal: %v", err)
	}
	if err = s.CheckAgentLease(ctx, r.ID, r.Owner, r.LeaseEpoch); !errors.Is(err, ErrStale) {
		t.Fatalf("expired write fence: %v", err)
	}
	orphans, err := s.OrphanAgentRuns(ctx)
	if err != nil || len(orphans) != 1 {
		t.Fatalf("orphans=%d err=%v", len(orphans), err)
	}
	if _, err = s.ClaimAgent(ctx, "b", "other", 1, 1); !errors.Is(err, ErrAgentQueued) {
		t.Fatalf("expired process released capacity: %v", err)
	}
	if err = s.FinishAgent(ctx, r.ID, "wrong-owner", r.LeaseEpoch); !errors.Is(err, ErrStale) {
		t.Fatalf("owner fence: %v", err)
	}
	if err = s.FinishAgent(ctx, r.ID, r.Owner, r.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimAgent(ctx, "b", "other", 1, 1); err != nil {
		t.Fatal(err)
	}
}

func TestAgentQueueFairnessAndWorkflowFence(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "w1", 1)
	seedAgentWorkflow(t, s, "w2", 2)
	enqueueTestAgent(t, s, "a", "w1")
	enqueueTestAgent(t, s, "b", "w1")
	enqueueTestAgent(t, s, "c", "w2")
	r, err := s.ClaimAgent(ctx, "a", "owner", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimAgent(ctx, "b", "owner", 2, 2); !errors.Is(err, ErrAgentQueued) {
		t.Fatalf("workflow monopolized fair turn: %v", err)
	}
	if _, err = s.ClaimAgent(ctx, "c", "owner", 2, 2); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAgent(ctx, r.ID, r.Owner, r.LeaseEpoch); !errors.Is(err, ErrStale) {
		t.Fatalf("finish without stopping: %v", err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE workflows SET generation=2 WHERE id='w1'`); err != nil {
		t.Fatal(err)
	}
	if err = s.RenewAgent(ctx, r.ID, r.Owner, r.LeaseEpoch); !errors.Is(err, ErrStale) {
		t.Fatalf("generation renewal: %v", err)
	}
	if _, err = s.OrphanAgentRuns(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListAgentRuns(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Status != domain.AgentStopping || runs[1].Status != domain.AgentObsolete {
		t.Fatalf("stale generation states: %+v", runs)
	}
}

// This helper executes in distinct OS processes against the same PG schema.
func TestAgentQueueProcessWorker(t *testing.T) {
	if os.Getenv("AGENT_QUEUE_CHILD") != "1" {
		t.Skip("subprocess only")
	}
	s := agentQueueTestStore(t)
	id := os.Getenv("AGENT_QUEUE_CHILD_ID")
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_, err := s.ClaimAgent(context.Background(), id, "process-"+id, 3, 2)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrAgentQueued) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func TestAgentQueueConcurrentProcessesRespectLimits(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "w1", 1)
	seedAgentWorkflow(t, s, "w2", 2)
	cmds := make([]*exec.Cmd, 8)
	for i := range cmds {
		id := "agent-" + strconv.Itoa(i)
		wf := "w1"
		if i%2 == 1 {
			wf = "w2"
		}
		enqueueTestAgent(t, s, id, wf)
		cmds[i] = exec.Command(os.Args[0], "-test.run=^TestAgentQueueProcessWorker$")
		cmds[i].Env = append(os.Environ(), "AGENT_QUEUE_CHILD=1", "AGENT_QUEUE_CHILD_ID="+id)
	}
	for _, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal("queue child failed:", err)
		}
	}
	runs, err := s.ListAgentRuns(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	per := map[string]int{}
	for _, r := range runs {
		if r.Status == domain.AgentActive {
			total++
			per[r.WorkflowID]++
		} else if !r.StartedAt.IsZero() {
			t.Fatalf("queued session has execution start: %s", r.ID)
		}
	}
	if total != 3 || per["w1"] > 2 || per["w2"] > 2 || per["w1"] == 0 || per["w2"] == 0 {
		t.Fatalf("capacity/fairness: total=%d per=%v", total, per)
	}
	var version int64
	var raw string
	if err = s.DB.QueryRow(ctx, `SELECT version,data::text FROM workflows WHERE id='w1'`).Scan(&version, &raw); err != nil {
		t.Fatal(err)
	}
	if version != 1 || strings.Contains(raw, "agent_runs") {
		t.Fatal("queue admission modified workflow document")
	}
}

func TestAgentQueuePromptAuditSurvivesMigrationAndAdmission(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "w", 1)
	run := domain.AgentRun{ID: "audit-run", WorkflowID: "w", TaskID: "task", Role: "builder", Generation: 1, PromptVersion: "builder-v4", StartReason: "依赖已集成，启动开发任务"}
	if err := s.EnqueueAgent(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateAgentQueue(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimAgent(ctx, run.ID, "worker", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.PromptVersion != run.PromptVersion || claimed.StartReason != run.StartReason {
		t.Fatalf("lost prompt audit: %+v", claimed)
	}
	runs, err := s.ListAgentRuns(ctx, "w")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%d err=%v", len(runs), err)
	}
	if runs[0].PromptVersion != run.PromptVersion || runs[0].StartReason != run.StartReason {
		t.Fatal("prompt audit not persisted")
	}
	var migrated bool
	if err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=5)`).Scan(&migrated); err != nil || !migrated {
		t.Fatalf("migration 5: %t %v", migrated, err)
	}
}
