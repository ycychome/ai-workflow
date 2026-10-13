package orchestration

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/ycychome/ai-workflow/internal/sandbox"
)

func TestPlannerSessionRetainsManagerAfterConfigurationChange(t *testing.T) {
	if os.Getenv("AI_WORKFLOW_FORCE_DOCKER") != "1" {
		t.Skip("set AI_WORKFLOW_FORCE_DOCKER=1 with the runner image available")
	}
	e, ctx := safetyRegressionDB(t)
	original, err := sandbox.NewManager(sandbox.Options{RunnerImage: e.Config().Runner.Image, WorkspaceBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	e.Sandbox = original
	w := alphaClaimWorkflow(t, e, ctx, "PLANNING", nil)
	session, cleanup, err := e.plannerSession(ctx, &w, "plan")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = original.Destroy(context.Background(), session.workspace) })
	if session.manager != original {
		t.Fatal("session did not capture its workspace owner")
	}
	cfg := e.Config()
	cfg.Runner.Image = "replacement"
	// Exercise pointer reads concurrently with a real configuration replacement.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 1000 {
			_ = e.sandboxManager()
		}
	}()
	err = e.SetConfig(ctx, cfg)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if e.sandboxManager() == original {
		t.Fatal("configuration did not replace the manager")
	}
	if _, err := e.sandboxManager().ReadFile(ctx, session.workspace, "calculator.go"); err == nil {
		t.Fatal("新管理器错误接管旧工作区")
	}
	if _, err := session.manager.ReadFile(ctx, session.workspace, "calculator.go"); err != nil {
		t.Fatalf("既有会话丢失原管理器工作区：%v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := original.ReadFile(ctx, session.workspace, "calculator.go"); err == nil {
		t.Fatal("planner 清理后原工作区仍可使用")
	}
	// Destroy/Cancel 现为幂等停止确认；重复调用成功表示真实资源仍为空。
	if err := original.Cancel(ctx, session.workspace); err != nil {
		t.Fatal(err)
	}
}
