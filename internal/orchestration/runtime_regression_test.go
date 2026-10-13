package orchestration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

// No sandbox/model is started by this helper. Explicit stop acknowledgements
// are therefore valid without consulting Docker resource state.
func regressionClaimRun(t *testing.T, e *Engine, ctx context.Context, w domain.Workflow, task domain.Task, role agent.RoleID) domain.AgentRun {
	t.Helper()
	run := domain.AgentRun{ID: "runtime-regression-run", WorkflowID: w.ID, TaskID: task.ID, Role: string(role), Owner: e.owner, Generation: w.Generation, PlanRevision: planRevision(w, frozenReview(role, task)), PromptVersion: agent.PromptVersion}
	if err := e.Store.EnqueueAgent(ctx, run); err != nil {
		t.Fatal(err)
	}
	var err error
	run, err = e.Store.ClaimAgent(ctx, run.ID, e.owner, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	run.ContextHash = executionHash(w, task, role)
	if err = e.Store.BindAgentContext(ctx, run.ID, run.Owner, run.LeaseEpoch, run.ContextHash); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRuntimeResultRejectsAdvancedTaskEpochBeforeReplacementAdmission(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	task := alphaCodeTask()
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", []domain.Task{task})
	run := regressionClaimRun(t, e, ctx, w, task, agent.RoleBuilder)
	// A resumed scheduling pass advances the task before its replacement can
	// claim capacity. There is deliberately no newer attempts row to fence on.
	if err := e.Store.Update(ctx, w.ID, func(current *domain.Workflow) ([]store.Action, error) {
		current.Tasks[0].LeaseEpoch++
		current.Tasks[0].Attempts++
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	err := e.finishTaskAttempt(store.WithAgentFence(ctx, run), run.ID, w, task, "OUTPUT_READY", map[string]string{"summary": "旧任务迟到的成功报告"}, nil, nil)
	if !errors.Is(err, store.ErrStale) {
		t.Fatalf("old task epoch result was accepted after replacement queued: %v", err)
	}
	var count int
	if err = e.Store.DB.QueryRow(ctx, `SELECT count(*) FROM attempts WHERE id=$1 AND state IN ('OUTPUT_READY','ACCEPTED')`, run.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale accepted receipts=%d err=%v", count, err)
	}
}

func TestRepeatedFinishCannotHideFailedStopAcknowledgement(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	task := alphaCodeTask()
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", []domain.Task{task})
	run := regressionClaimRun(t, e, ctx, w, task, agent.RoleBuilder)
	stale := run
	stale.LeaseEpoch++
	x := &agentExecution{e: e, run: stale, cancel: func() {}}
	first := x.finish()
	second := x.finish()
	if !errors.Is(first, store.ErrStale) || !errors.Is(second, store.ErrStale) {
		t.Fatalf("repeated cleanup hid a failure: %v / %v", first, second)
	}
	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != domain.AgentActive {
		t.Fatalf("stale cleanup released real capacity: %+v %v", runs, err)
	}
	if err := e.Store.MarkAgentStopping(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.FinishAgent(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCompletedEvidenceRequestRecoversTaskReceiptAfterCrash(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := alphaClaimWorkflow(t, e, ctx, "REVIEWING", nil)
	w.TestedSHA = w.HeadSHA
	w.TestedTreeSHA = contentDigest(localFiles())
	w.Review.PlanRevision = 1
	w.Review.VerificationPassed = true
	w.Review.Key = e.key(ctx, w)
	task := domain.Task{ID: "evidence-ER-crash", Role: "verifier", Kind: "verification", Description: "验证已完成证据的重启恢复", Status: "QUEUED", LeaseEpoch: 7, Attempts: 1, PlanRevision: 1}
	w.Review.Tasks = []domain.Task{task}
	w.TasksCreated = 1
	request := domain.EvidenceRequest{ID: "ER-crash", TaskID: task.ID, TargetRole: "verifier", Question: "检查加法", VerificationKey: w.Review.Key, Status: "RUNNING", Attempt: 1, UpdatedAt: time.Now().UTC().Add(-time.Second)}
	w.EvidenceRequests = []domain.EvidenceRequest{request}
	if err := e.Store.Save(ctx, &w); err != nil {
		t.Fatal(err)
	}
	run := regressionClaimRun(t, e, ctx, w, task, agent.RoleVerifier)
	fenced := store.WithAgentFence(ctx, run)
	if err := e.BindEvidenceRequestRun(fenced, w.ID, w.Review.Key, request.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	ev, err := e.artifact(ctx, w, "crash-proof", "TOOL_VERIFIED", map[string]any{"exit_code": 0, "stdout": "实际检查记录"})
	if err != nil {
		t.Fatal(err)
	}
	ev.AgentRunID = run.ID
	ev.ExitCode = 0
	ev.Command = []string{"go", "test", "./..."}
	if err = e.finishTaskAttempt(fenced, run.ID, w, task, "ACCEPTED", map[string]string{"summary": "检查完成"}, []domain.Evidence{ev}, nil); err != nil {
		t.Fatal(err)
	}
	if err = e.CompleteEvidenceRequest(fenced, w.ID, w.Review.Key, request.ID, run.ID, []domain.Evidence{ev}); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.MarkAgentStopping(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.FinishAgent(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	// Simulate process loss before executeEvidenceIntent's final task Save.
	w, err = e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.EvidenceRequests[0].Status != "COMPLETED" {
		t.Fatal("completed request was not durable before simulated restart")
	}
	if err = e.executeEvidenceRequests(ctx, &w); err != nil {
		t.Fatal(err)
	}
	w, err = e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Review.Tasks[0].Status != "ACCEPTED" || w.Review.Tasks[0].AgentRunID != run.ID {
		t.Fatalf("completed receipt did not repair task: %+v", w.Review.Tasks[0])
	}
	if w.EvidenceRequests[0].Attempt != 1 {
		t.Fatal("recovery repeated a completed request instead of consuming its durable receipt")
	}
}

func TestRuntimeCleanupWaitsForOwnCallbacksOnly(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	task := alphaCodeTask()
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", []domain.Task{task})
	run := regressionClaimRun(t, e, ctx, w, task, agent.RoleBuilder)
	callbackCtx, callbackCancel := context.WithCancel(ctx)
	defer callbackCancel()
	e.mu.Lock()
	e.agentActive = map[string]context.CancelFunc{run.ID: callbackCancel, "other-workflow-run": func() { t.Error("cleanup cancelled another workflow") }}
	e.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- e.waitWorkflowAgentCleanup(ctx, w.ID) }()
	select {
	case <-callbackCtx.Done():
	case <-ctx.Done():
		t.Fatal("cleanup never cancelled target callback")
	}
	select {
	case err := <-done:
		t.Fatalf("cleanup returned before callback acknowledgement: %v", err)
	default:
	}
	// Simulate callback completion after its resource-cleanup attempt. It did
	// not confirm resource absence: STOPPING must retain its capacity charge.
	if err := e.Store.MarkAgentStopping(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	delete(e.agentActive, run.ID)
	e.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("cleanup ignored completed callback")
	}
	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != domain.AgentStopping {
		t.Fatalf("cleanup incorrectly acknowledged resources: %+v %v", runs, err)
	}
}

func TestRuntimeBudgetSurvivesRestartBeforeNextModelAdmission(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	e.cfg.Budget.IssueMinutes = 1
	w := alphaClaimWorkflow(t, e, ctx, "TRIAGE", nil)
	if err := e.Store.EnqueueAgent(ctx, domain.AgentRun{ID: "prior-controller-session", WorkflowID: w.ID, TaskID: "triage", Role: "triage", Owner: e.owner, Generation: w.Generation}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB.Exec(ctx, `UPDATE agent_runs SET status='FINISHED',started_at='2025-01-01 00:00:00+00',stopped_at='2025-01-01 00:02:00+00' WHERE id='prior-controller-session'`); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.execute(runCtx, w, cancel)
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Phase != "BLOCKED" || current.RuntimeSeconds < 120 || current.ModelCalls != 0 {
		t.Fatalf("restart bypassed durable budget: phase=%s runtime=%d calls=%d", current.Phase, current.RuntimeSeconds, current.ModelCalls)
	}
	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("exhausted workflow admitted fresh session: %+v %v", runs, err)
	}
}

func TestEvidenceDailyBudgetResumeKeepsAttemptCount(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := alphaClaimWorkflow(t, e, ctx, "REVIEWING", nil)
	w.TestedSHA = w.HeadSHA
	w.TestedTreeSHA = contentDigest(localFiles())
	w.Review.Key = e.key(ctx, w)
	task := domain.Task{ID: "evidence-ER-daily", Role: "verifier", Kind: "verification", Status: "QUEUED", Attempts: 3, LeaseEpoch: 9, PlanRevision: 1, BudgetDeferred: true}
	w.Review.Tasks = []domain.Task{task}
	w.Review.PlanRevision = 1
	w.EvidenceRequests = []domain.EvidenceRequest{{ID: "ER-daily", TaskID: task.ID, TargetRole: "verifier", Question: "次日继续检查", VerificationKey: w.Review.Key, Status: "QUEUED", Attempt: 3}}
	if err := e.Store.Save(ctx, &w); err != nil {
		t.Fatal(err)
	}
	intent, err := e.ClaimEvidenceRequest(ctx, w.ID, w.Review.Key)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Request.Attempt != 3 || intent.Task.Attempts != 3 {
		t.Fatalf("daily checkpoint consumed another attempt: %+v", intent)
	}
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Review.Tasks[0].LeaseEpoch != 9 || current.ModelCalls != w.ModelCalls || current.RuntimeSeconds != w.RuntimeSeconds {
		t.Fatal("claim reset accumulated budget or rewrote task epoch before admission")
	}
}
