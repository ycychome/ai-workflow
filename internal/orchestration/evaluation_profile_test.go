package orchestration

import (
	"errors"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

func TestEvaluationAblationsCreateOneBuilderWithoutModelPlanner(t *testing.T) {
	for _, profile := range []string{"builder_ci", "single_review"} {
		t.Run(profile, func(t *testing.T) {
			e, ctx := safetyRegressionDB(t)
			w := alphaClaimWorkflow(t, e, ctx, "PLANNING", nil)
			w.Evaluation = &domain.EvaluationContext{Profile: profile, Kind: "development"}
			if err := e.Store.Save(ctx, &w); err != nil {
				t.Fatal(err)
			}
			if err := e.plan(ctx, &w); err != nil {
				t.Fatal(err)
			}
			if len(w.Tasks) != 1 || w.Tasks[0].Role != "builder" || w.Tasks[0].Kind != "code" {
				t.Fatalf("unexpected ablation plan: %+v", w.Tasks)
			}
			runs, err := e.Store.ListAgentRuns(ctx, w.ID)
			if err != nil || len(runs) != 0 {
				t.Fatalf("ablation invoked a model planner: %+v %v", runs, err)
			}
			if w.TasksCreated != 1 || w.Phase != "DEVELOPING" {
				t.Fatalf("invalid planning transition: %+v", w)
			}
		})
	}
}

func TestEvaluationCompletionCannotPublishGreenProductionGate(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := alphaClaimWorkflow(t, e, ctx, "VERIFYING", nil)
	w.Evaluation = &domain.EvaluationContext{Profile: "builder_ci", Kind: "development"}
	if err := e.Store.Save(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if err := e.finishEvaluation(ctx, &w, true, "可信测试已通过"); err != nil {
		t.Fatal(err)
	}
	if w.Phase != "EVALUATION_COMPLETE" || w.Review.Verdict != "PROFILE_PASS" || domain.Runnable(w) {
		t.Fatalf("invalid trial terminal: %+v", w)
	}
	var greens int
	if err := e.Store.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id=$1 AND kind='gate' AND data->>'pass'='true'`, w.ID).Scan(&greens); err != nil {
		t.Fatal(err)
	}
	if greens != 0 {
		t.Fatal("ablation marked production merge gate successful")
	}
}

func TestDailyBudgetCheckpointPreservesGenerationAttemptsAndUsage(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	task := alphaCodeTask()
	task.BudgetDeferred = true
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", []domain.Task{task})
	if _, err := e.Store.DB.Exec(ctx, `UPDATE workflow_metrics SET calls=2 WHERE workflow_id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	w, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation := w.Generation
	e.fail(ctx, &w, store.ErrDailyBudget)
	if w.Phase != "WAITING_DAILY_BUDGET" || w.ResumePhase != "DEVELOPING" || domain.Runnable(w) {
		t.Fatal("daily exhaustion did not checkpoint")
	}
	if err := e.ResumeDailyBudget(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	w, err = e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Generation != generation || w.ModelCalls != 2 || w.Tasks[0].Attempts != task.Attempts || !w.Tasks[0].BudgetDeferred || w.Phase != "DEVELOPING" {
		t.Fatalf("resume reset cumulative identity or budget: %+v", w)
	}
	e.fail(ctx, &w, store.ErrIssueBudget)
	if w.Phase != "BLOCKED" {
		t.Fatal("per-issue budget must not become daily wait")
	}
	if err := e.ResumeDailyBudget(ctx, w.ID); err == nil || errors.Is(err, store.ErrDailyBudget) {
		t.Fatalf("issue exhaustion can be resumed as daily: %v", err)
	}
}

func TestCompletedTaskEventWakesPlannerOnceWithoutReplacingWork(t *testing.T) {
	w := domain.Workflow{Generation: 1, Spec: domain.Spec{Approved: true, Digest: "approved"}, HeadSHA: "head", PlanRevision: 1, TasksCreated: 1, Tasks: []domain.Task{{ID: "done", Kind: "code", Description: "已经完成的源码任务", Paths: []string{"app.go"}, Status: "QUEUED"}}}
	w.PlannedStateDigest = developmentStateDigest(w)
	w.Tasks[0].Status = "INTEGRATED"
	w.Tasks[0].IntegratedSHA = "new-head"
	w.HeadSHA = "new-head"
	if w.PlannedStateDigest == developmentStateDigest(w) {
		t.Fatal("completion did not wake planner")
	}
	report := developmentPlanReport{ExpectedRevision: 1, Reason: "检查完成事件后没有新增任务"}
	if err := applyDevelopmentPlan(&w, report, 64); err != nil {
		t.Fatal(err)
	}
	w.PlannedStateDigest = developmentStateDigest(w)
	if w.TasksCreated != 1 || len(w.Tasks) != 1 || w.Tasks[0].Status != "INTEGRATED" {
		t.Fatal("no-op replan replaced accepted work")
	}
	if w.PlannedStateDigest != developmentStateDigest(w) {
		t.Fatal("stable event repeatedly wakes planner")
	}
}

func TestWorkerBatchSnapshotDoesNotAliasControllerLedger(t *testing.T) {
	w := domain.Workflow{Tasks: []domain.Task{{ID: "builder", Paths: []string{"app.go"}, Report: []byte(`{"summary":"原始实现报告"}`)}}, Review: domain.Review{Tasks: []domain.Task{{ID: "reviewer", ObligationIDs: []string{"AC1"}}}, Findings: []domain.Finding{{ID: "F1", History: []string{"原始记录"}}}}}
	snapshot, err := cloneWorkflow(w)
	if err != nil {
		t.Fatal(err)
	}
	w.Tasks[0].Paths[0] = "changed.go"
	w.Tasks[0].Report[2] = 'X'
	w.Review.Tasks[0].ObligationIDs[0] = "AC2"
	w.Review.Findings[0].History[0] = "修改后的记录"
	if snapshot.Tasks[0].Paths[0] != "app.go" || string(snapshot.Tasks[0].Report) != `{"summary":"原始实现报告"}` || snapshot.Review.Tasks[0].ObligationIDs[0] != "AC1" || snapshot.Review.Findings[0].History[0] != "原始记录" {
		t.Fatal("immutable batch snapshot shares nested mutable storage")
	}
}
