package orchestration

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
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
	w := domain.Workflow{ID: "manager-swap", Repository: "local/pilot", Generation: 1, Control: "ACTIVE"}
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
	if err := session.manager.Cancel(ctx, session.workspace); err != nil {
		t.Fatalf("existing session lost access to its workspace: %v", err)
	}
	cleanup()
	if err := original.Cancel(ctx, session.workspace); err == nil {
		t.Fatal("planner cleanup left its original workspace registered")
	}
}
