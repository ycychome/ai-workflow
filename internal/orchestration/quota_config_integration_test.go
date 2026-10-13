package orchestration

import (
	"context"
	"testing"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestWorkspaceCapacityChangeReplacesManagerAndInvalidatesApproval(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	originalManager := e.sandboxManager()
	cfg := e.Config()
	if cfg.Runner.WorkspaceBytes != config.DefaultWorkspaceBytes {
		t.Fatalf("engine did not normalize default: %d", cfg.Runner.WorkspaceBytes)
	}
	w := domain.Workflow{ID: "quota-config", RepositoryID: 998901, Issue: 1, Repository: "local/pilot", Control: "ACTIVE", Phase: "AWAITING_MAINTAINER", PR: 1, HeadSHA: "head", BaseSHA: "base", Review: domain.Review{Verdict: "PASS"}}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	w, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := e.key(ctx, w)
	stopped := false
	e.active[w.ID] = func() { stopped = true }
	cfg.Runner.WorkspaceBytes = 64 << 20
	if err := e.SetConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e.sandboxManager() == originalManager || !stopped || got.Generation != w.Generation+1 || got.Control != "PAUSING" || got.Review.Verdict != "OBSOLETE" {
		t.Fatalf("capacity change failed to replace/fence: stopped=%v gen=%d control=%s review=%s", stopped, got.Generation, got.Control, got.Review.Verdict)
	}
	if e.key(ctx, w) == oldKey {
		t.Fatal("quota was omitted from verification identity")
	}
	var gates int
	if err := e.Store.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id=$1 AND kind='gate' AND data->>'pass'='false'`, w.ID).Scan(&gates); err != nil || gates != 1 {
		t.Fatalf("quota change did not revoke approval: gates=%d err=%v", gates, err)
	}
	manager := e.sandboxManager()
	if err := e.SetConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := e.Store.Get(ctx, w.ID)
	if err != nil || again.Generation != got.Generation || e.sandboxManager() != manager {
		t.Fatalf("same capacity replaced manager/invalidated twice: gen=%d err=%v", again.Generation, err)
	}
	for _, invalid := range []int64{-1, config.MaxWorkspaceBytes + 1} {
		bad := cfg
		bad.Runner.WorkspaceBytes = invalid
		if err := e.SetConfig(ctx, bad); err == nil {
			t.Fatalf("invalid capacity %d accepted", invalid)
		}
		if e.Config().Runner.WorkspaceBytes != cfg.Runner.WorkspaceBytes || e.sandboxManager() != manager {
			t.Fatal("rejected capacity modified engine")
		}
	}
}
func TestWorkspaceCapacityDefaultEquivalentDoesNotInvalidate(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	initial := e.sandboxManager()
	cfg := e.Config()
	cfg.Runner.WorkspaceBytes = 0
	if err := e.SetConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if e.sandboxManager() != initial || e.Config().Runner.WorkspaceBytes != config.DefaultWorkspaceBytes {
		t.Fatal("implicit default not equivalent to explicit default")
	}
}
func TestEngineRejectsInvalidProgrammaticWorkspaceCapacity(t *testing.T) {
	for _, quota := range []int64{-1, config.MaxWorkspaceBytes + 1} {
		cfg := config.Config{}
		cfg.Runner.WorkspaceBytes = quota
		if _, err := New(nil, cfg); err == nil {
			t.Fatalf("New accepted invalid quota %d", quota)
		}
		e := &Engine{cfg: config.Config{}}
		if err := e.SetConfig(context.Background(), cfg); err == nil {
			t.Fatalf("SetConfig accepted invalid quota %d", quota)
		}
	}
}
