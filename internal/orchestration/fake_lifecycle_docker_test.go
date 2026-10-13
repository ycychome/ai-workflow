package orchestration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
)

// 使用真实 PG、Docker、持久队列和发布 Outbox；仅模型报告与远端仓库使用本地 Fake。
func TestFakeWorkflowDockerCompletesTrustedLifecycle(t *testing.T) {
	alphaRequireDocker(t)
	e, ctx := safetyRegressionDB(t)
	e.cfg.Runner.RequireAcceptForDevelopment = false
	e.cfg.Runner.Reviewers = 2
	initial, err := e.Demo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	phases := map[string]bool{}
	var latest domain.Workflow
	for step := 0; step < 40; step++ {
		latest, err = e.Store.Get(ctx, initial.ID)
		if err != nil {
			t.Fatal(err)
		}
		phases[latest.Phase] = true
		t.Logf("步骤 %d：%s，计划修订 %d，任务 %d", step, latest.Phase, latest.PlanRevision, len(latest.Tasks))
		if latest.Phase == "BLOCKED" {
			t.Fatalf("完整流程阻塞：%s", latest.LastError)
		}
		if latest.Phase == "AWAITING_MAINTAINER" {
			break
		}
		claimed, err := e.Store.Claim(ctx, e.owner)
		if err != nil {
			t.Fatalf("调度不能继续（%s）：%v", latest.Phase, err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		e.execute(runCtx, claimed, cancel)
		for n := 0; n < 100; n++ {
			action, err := e.Store.ClaimAction(ctx)
			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			e.processOutboxAction(ctx, action)
		}
	}
	if latest.Phase != "AWAITING_MAINTAINER" {
		t.Fatalf("流程未收敛：%s %s", latest.Phase, latest.LastError)
	}
	for _, phase := range []string{"TRIAGE", "PLANNING", "DEVELOPING", "VERIFYING", "REVIEWING"} {
		if !phases[phase] {
			t.Errorf("未经过阶段 %s", phase)
		}
	}
	if !latest.Review.VerificationPassed || !latest.Review.SkepticComplete || latest.Review.Verdict != "PASS" {
		t.Fatalf("可信验证/独立审查未形成 PASS：%+v", latest.Review)
	}
	if ok, reason := domain.Gate(latest, e.key(ctx, latest)); !ok {
		t.Fatalf("最终真实门禁未通过：%s", reason)
	}
	runs, err := e.Store.ListAgentRuns(ctx, latest.ID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, run := range runs {
		roles[run.Role] = true
		if run.Status != domain.AgentFinished || run.StoppedAt.IsZero() {
			t.Errorf("运行未停止确认：%s %s", run.Role, run.Status)
		}
	}
	for _, role := range []string{"triage", "dev_planner", "builder", "verifier", "review_planner", "reviewer", "skeptic"} {
		if !roles[role] {
			t.Errorf("角色没有真实经过队列：%s", role)
		}
	}
	if len(latest.SourceArtifacts) == 0 || len(latest.IntegrationRecords) == 0 {
		t.Fatal("来源证据或集成记录缺失")
	}
	for _, record := range latest.IntegrationRecords {
		if record.Status != "PUBLISHED" || record.TreeSHA == "" || record.CommitSHA == "" {
			t.Errorf("集成没有精确发布回执：%+v", record)
		}
	}
	source, err := e.source(ctx, latest, latest.HeadSHA)
	if err != nil || !strings.Contains(string(source["calculator.go"].Data), "return a + b") {
		t.Fatalf("已发布修复丢失：%v", err)
	}
	var green int
	if err = e.Store.DB.QueryRow(ctx, `SELECT count(*) FROM audit WHERE workflow_id=$1 AND kind='fixture_gate' AND data->>'pass'='true'`, latest.ID).Scan(&green); err != nil {
		t.Fatal(err)
	}
	if green == 0 {
		t.Fatal("没有实际消费最终绿色门禁 Outbox")
	}
}
