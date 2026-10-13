package orchestration

import (
	"context"
	"errors"
	"github.com/ycychome/ai-workflow/internal/domain"
	"path/filepath"
	"testing"
	"time"
)

func TestTrustedRuntimeChargesOncePerExecution(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", nil)
	for _, seconds := range []int64{7, 40} {
		if err := e.chargeTrustedRuntime(ctx, w.ID, "ci-executor-first", seconds); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.chargeTrustedRuntime(ctx, w.ID, "evaluation-judge-second", 3); err != nil {
		t.Fatal(err)
	}
	got, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil || got != 10 {
		t.Fatalf("同一执行重复计时或不同执行漏计：%d %v", got, err)
	}
}

func TestTrustedRuntimeMissingWorkflowRollsBackReceipt(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	if err := e.chargeTrustedRuntime(ctx, "missing", "ci-executor-missing", 4); err == nil {
		t.Fatal("不存在的工作流不能计时成功")
	}
	var count int
	if err := e.Store.DB.QueryRow(ctx, `SELECT count(*) FROM processed_commands WHERE command_id=$1`, "runtime:ci-executor-missing").Scan(&count); err != nil || count != 0 {
		t.Fatalf("失败事务留下幂等回执：%d %v", count, err)
	}
}

func TestCIExecutorRecoveredRuntimePreservesStopTime(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", nil)
	start := time.Now().Add(-time.Minute)
	journal := ciExecutorJournal{WorkflowID: w.ID, RunID: "ci-executor-recovered", StartedAt: start, StoppedAt: start.Add(2100 * time.Millisecond)}
	if err := e.chargeStoppedCIExecutor(ctx, &journal); err != nil {
		t.Fatal(err)
	}
	if err := e.chargeStoppedCIExecutor(ctx, &journal); err != nil {
		t.Fatal(err)
	}
	got, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil || got != 3 {
		t.Fatalf("恢复没有采用已确认停止时间或重复扣减：%d %v", got, err)
	}
	if err := e.chargeStoppedCIExecutor(ctx, &ciExecutorJournal{WorkflowID: w.ID, RunID: "ci-executor-unknown"}); err == nil {
		t.Fatal("缺失起始时间不能静默漏计")
	}
}

func TestTrustedExecutorsShareRecoveryLock(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", nil)
	conn, err := e.Store.DB.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	key := trustedExecutionLockKey(w.ID)
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, key); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key)
	if err = e.RecoverTrustedCIExecutor(ctx, w.ID); !errors.Is(err, ErrCIExecutorBusy) {
		t.Fatalf("活跃外部判定锁未阻止CI恢复：%v", err)
	}
	// 路径故意不存在：拿不到相同锁时必须跳过，不能触碰另一个执行器资源。
	if err = e.recoverEvaluationJudge(ctx, filepath.Join(t.TempDir(), "missing.json"), evaluationJudgeJournal{ciExecutorJournal: ciExecutorJournal{WorkflowID: w.ID, RunID: "evaluation-judge-other"}}); err != nil {
		t.Fatalf("活跃CI锁未阻止判定恢复：%v", err)
	}
}

func TestCIExecutorRequiresUnleasedCheckpoint(t *testing.T) {
	for _, phase := range []string{"WAITING_CI", "AWAITING_MAINTAINER", "EVALUATION_COMPLETE"} {
		if !ciExecutorWorkflowReady(domain.Workflow{Phase: phase}) {
			t.Fatal(phase)
		}
	}
	for _, w := range []domain.Workflow{{Phase: "DEVELOPING"}, {Phase: "VERIFYING"}, {Phase: "WAITING_CI", LeaseUntil: time.Now().Add(time.Minute)}} {
		if ciExecutorWorkflowReady(w) {
			t.Fatalf("不安全的签发启动被允许：%+v", w)
		}
	}
}

func TestTrustedExecutionRejectsUnsettledOtherJournal(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", nil)
	if err := e.requireOtherTrustedExecutionSettled(ctx, w.ID, "evaluation-judges"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Config().DataDir, "evaluation-judges", domain.Digest(w.ID)+".json")
	journal := ciExecutorJournal{WorkflowID: w.ID, RunID: "evaluation-judge-crashed", Status: "PREPARING", StartedAt: time.Now().Add(-time.Second)}
	if err := writeCIExecutorJSON(path, journal); err != nil {
		t.Fatal(err)
	}
	if err := e.requireOtherTrustedExecutionSettled(ctx, w.ID, "evaluation-judges"); err == nil {
		t.Fatal("未停止的另一执行被忽略")
	}
	journal.Status = "STOPPED"
	journal.StoppedAt = time.Now()
	if err := writeCIExecutorJSON(path, journal); err != nil {
		t.Fatal(err)
	}
	if err := e.requireOtherTrustedExecutionSettled(ctx, w.ID, "evaluation-judges"); err == nil {
		t.Fatal("尚未结算的另一执行被忽略")
	}
	if err := e.chargeTrustedRuntime(ctx, w.ID, journal.RunID, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.requireOtherTrustedExecutionSettled(ctx, w.ID, "evaluation-judges"); err != nil {
		t.Fatal(err)
	}
}
