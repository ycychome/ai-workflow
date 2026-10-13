package orchestration

import (
	"context"
	"testing"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

func failedIntegrationCheckpoint(t *testing.T, e *Engine, ctx context.Context) domain.Workflow {
	t.Helper()
	task := alphaCodeTask()
	queued := domain.Task{ID: "regression", Kind: "test", Description: "回归验证", Status: "QUEUED", PlanRevision: 1}
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", []domain.Task{task, queued})
	w.IssueTitle = "受控集成恢复"
	w.IssueBody = "验证基础设施失败后复用已接受来源。"
	w.Spec.SourceHash = normalizedIssueSourceHash(w.IssueTitle, w.IssueBody)
	w.Spec.Digest = domain.SpecDigest(w.Spec)
	task = w.Tasks[0]
	run := regressionClaimRun(t, e, ctx, w, task, agent.RoleBuilder)
	task.AgentRunID = run.ID
	task.Status = "RUNNING"
	w.Tasks[0] = task
	if err := e.Store.Save(ctx, &w); err != nil {
		t.Fatal(err)
	}
	changes := []sandbox.Change{{Path: "calculator.go", Status: "M", Mode: 0644, Data: []byte("package pilot\n\nfunc Add(a,b int) int { return a + b }\n")}}
	ev, err := e.artifact(ctx, w, "accepted-source", "SOURCE_ARTIFACT", sourceArtifactPayload{Generation: w.Generation, LeaseEpoch: task.LeaseEpoch, BaseSHA: w.HeadSHA, Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	ev.AgentRunID = run.ID
	if err = e.finishTaskAttempt(store.WithAgentFence(ctx, run), run.ID, w, task, "OUTPUT_READY", map[string]string{"summary": "已接受的 builder 来源"}, []domain.Evidence{ev}, nil); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.MarkAgentStopping(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.FinishAgent(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	task.Status = "VALIDATED"
	w.Tasks[0] = task
	w.Evidence = []domain.Evidence{ev}
	artifact := domain.SourceArtifact{ID: "source-" + domain.Digest([]any{run.ID, ev.Hash})[:24], TaskID: task.ID, AgentRunID: run.ID, Generation: w.Generation, PlanRevision: task.PlanRevision, SpecDigest: w.Spec.Digest, ParentSHA: w.HeadSHA, ChangesDigest: domain.Digest(changes), EvidenceID: ev.ID, CreatedAt: ev.CreatedAt}
	w.SourceArtifacts = []domain.SourceArtifact{artifact}
	inputs := []IntegrationInput{{Task: task, Artifact: artifact, Changes: changes}}
	record := PendingIntegration(w, inputs)
	record.Status = "FAILED"
	record.Reason = "Git tree preparation failed before publish"
	w.IntegrationRecords = []domain.IntegrationRecord{record}
	w.Phase = "BLOCKED"
	w.Control = "ACTIVE"
	w.LastError = record.Reason
	w.ModelCalls = 62
	w.InputTokens = 41000
	w.OutputTokens = 12000
	w.RuntimeSeconds = 905
	if err = e.Store.Save(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.DB.Exec(ctx, `UPDATE workflow_metrics SET calls=$2,input_tokens=$3,output_tokens=$4,runtime_seconds=$5 WHERE workflow_id=$1`, w.ID, w.ModelCalls, w.InputTokens, w.OutputTokens, w.RuntimeSeconds); err != nil {
		t.Fatal(err)
	}
	w, err = e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestResumeIntegrationInfrastructureFailureKeepsExactCheckpoint(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := failedIntegrationCheckpoint(t, e, ctx)
	if err := e.Command(ctx, w.ID, "maintainer", "resume", true); err != nil {
		t.Fatal(err)
	}
	got, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != w.Generation || got.Phase != "DEVELOPING" || got.Control != "ACTIVE" || got.LastError != "" {
		t.Fatalf("resume did not preserve and reopen the exact checkpoint: generation=%d phase=%s control=%s error=%q", got.Generation, got.Phase, got.Control, got.LastError)
	}
	if got.ModelCalls != w.ModelCalls || got.InputTokens != w.InputTokens || got.OutputTokens != w.OutputTokens || got.RuntimeSeconds != w.RuntimeSeconds {
		t.Fatalf("resume changed cumulative budget accounting: before=%+v after=%+v", w, got)
	}
	if got.IntegrationRecords[0].Status != "FAILED" || got.IntegrationRecords[0].ID != w.IntegrationRecords[0].ID || got.Tasks[0].Status != "VALIDATED" || got.Tasks[1].Status != "QUEUED" {
		t.Fatalf("resume rewrote the accepted source or queued work: records=%+v tasks=%+v", got.IntegrationRecords, got.Tasks)
	}
	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != domain.AgentFinished {
		t.Fatalf("resume unexpectedly started work before the normal pipeline: runs=%+v err=%v", runs, err)
	}
}

func TestIntegrationCheckpointRejectsUnsafeRecoveryStates(t *testing.T) {
	t.Run("paused control", func(t *testing.T) {
		e, ctx := safetyRegressionDB(t)
		w := failedIntegrationCheckpoint(t, e, ctx)
		w.Control = "PAUSED"
		if _, ready, err := e.integrationCheckpointReady(ctx, w); err != nil || ready {
			t.Fatalf("paused workflow reused integration checkpoint: ready=%v err=%v", ready, err)
		}
	})
	t.Run("paused command replans generation", func(t *testing.T) {
		e, ctx := safetyRegressionDB(t)
		w := failedIntegrationCheckpoint(t, e, ctx)
		if err := e.Store.Update(ctx, w.ID, func(current *domain.Workflow) ([]store.Action, error) {
			current.Control = "PAUSED"
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.Command(ctx, w.ID, "maintainer", "resume", true); err != nil {
			t.Fatal(err)
		}
		got, err := e.Store.Get(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Generation != w.Generation+1 || got.Phase != "DEVELOPING" || got.ModelCalls != w.ModelCalls || got.IntegrationRecords[0].Status != "FAILED" {
			t.Fatalf("paused resume must invalidate the old checkpoint without resetting its budget: generation=%d phase=%s calls=%d record=%s", got.Generation, got.Phase, got.ModelCalls, got.IntegrationRecords[0].Status)
		}
	})
	t.Run("candidate drift", func(t *testing.T) {
		e, ctx := safetyRegressionDB(t)
		w := failedIntegrationCheckpoint(t, e, ctx)
		w.HeadSHA = "changed-head"
		if _, ready, err := e.integrationCheckpointReady(ctx, w); err != nil || ready {
			t.Fatalf("changed candidate reused integration checkpoint: ready=%v err=%v", ready, err)
		}
	})
	t.Run("corrupt evidence", func(t *testing.T) {
		e, ctx := safetyRegressionDB(t)
		w := failedIntegrationCheckpoint(t, e, ctx)
		w.Evidence[0].Hash = "tampered"
		if _, ready, err := e.integrationCheckpointReady(ctx, w); err != nil || ready {
			t.Fatalf("tampered evidence reused integration checkpoint: ready=%v err=%v", ready, err)
		}
	})
	t.Run("agent not confirmed stopped", func(t *testing.T) {
		e, ctx := safetyRegressionDB(t)
		w := failedIntegrationCheckpoint(t, e, ctx)
		if _, err := e.Store.DB.Exec(ctx, `UPDATE agent_runs SET status='STOPPING',stopped_at=NULL WHERE workflow_id=$1 AND generation=$2`, w.ID, w.Generation); err != nil {
			t.Fatal(err)
		}
		if _, ready, err := e.integrationCheckpointReady(ctx, w); err != nil || ready {
			t.Fatalf("unconfirmed agent stop reused integration checkpoint: ready=%v err=%v", ready, err)
		}
	})
	t.Run("uncertain publish", func(t *testing.T) {
		e, ctx := safetyRegressionDB(t)
		w := failedIntegrationCheckpoint(t, e, ctx)
		if _, err := e.Store.DB.Exec(ctx, `INSERT INTO outbox(id,workflow_id,generation,kind,data,state) VALUES($1,$2,$3,'publish','{}'::jsonb,'unknown')`, w.ID+":uncertain", w.ID, w.Generation); err != nil {
			t.Fatal(err)
		}
		if _, ready, err := e.integrationCheckpointReady(ctx, w); err != nil || ready {
			t.Fatalf("uncertain publish reused integration checkpoint: ready=%v err=%v", ready, err)
		}
	})
}
