package orchestration

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

// This opt-in test uses a temporary PostgreSQL schema and never starts the
// workflow engine or calls a model. It checks config invalidation while
// maintainers have already paused, cancelled, or taken over workflows.
func TestConfigInvalidationPreservesControlPostgresIntegration(t *testing.T) {
	if os.Getenv("AI_WORKFLOW_FORCE_INTEGRATION") != "1" {
		t.Skip("set AI_WORKFLOW_FORCE_INTEGRATION=1 in the controlled integration environment")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		t.Fatal("CONFIG_FILE must point to the controlled runtime config")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal("load controlled runtime config")
	}
	admin, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal("connect controlled PostgreSQL")
	}
	defer admin.Close()

	schema := fmt.Sprintf("control_state_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal("create temporary test schema")
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error("drop temporary test schema")
		}
	}()
	dbURL, err := controlTestSchemaURL(cfg.DatabaseURL, schema)
	if err != nil {
		t.Fatal("configure temporary test schema URL")
	}
	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal("open isolated control-state store")
	}
	defer s.Close()

	controls := []string{"ACTIVE", "CANCELLED", "CANCELLING", "HUMAN_OWNED", "TAKEOVER", "PAUSED", "PAUSING", "COMPLETED"}
	configChanges := []struct {
		name   string
		change func(*config.Config)
	}{
		{"model", func(c *config.Config) { c.Model.Dev = "new-dev-model" }},
		{"runner", func(c *config.Config) { c.Runner.Image = "runner:changed" }},
		{"workspace quota", func(c *config.Config) { c.Runner.WorkspaceBytes = 64 << 20 }},
		{"repository", func(c *config.Config) { c.Repositories = []config.Repository{{Name: "example/pilot", ID: 731991}} }},
	}
	baseRepo := int64(time.Now().UnixNano() % 1_000_000_000)
	for changeIndex, testCase := range configChanges {
		t.Run(testCase.name, func(t *testing.T) {
			oldConfig := config.Config{}
			oldConfig.Runner.Mode = "fake"
			oldConfig.Runner.Image = "runner:before"
			oldConfig.Runner.WorkflowConcurrency = 2
			newConfig := oldConfig
			testCase.change(&newConfig)
			engine := &Engine{Store: s, cfg: oldConfig, active: make(map[string]context.CancelFunc)}

			for i, control := range controls {
				workflow := domain.Workflow{
					ID: domain.ID(baseRepo+int64(changeIndex), i+1), RepositoryID: baseRepo + int64(changeIndex),
					Repository: "example/pilot", Issue: i + 1, Phase: "REVIEWING", Control: control,
					Review: domain.Review{Key: "old-key", Verdict: "PASS"},
				}
				if err := s.Create(ctx, workflow); err != nil {
					t.Fatal("create workflow fixture")
				}
			}

			if err := engine.SetConfig(ctx, newConfig); err != nil {
				t.Fatal("apply changed config")
			}
			for i, control := range controls {
				workflow, err := s.Get(ctx, domain.ID(baseRepo+int64(changeIndex), i+1))
				if err != nil {
					t.Fatal("read workflow fixture")
				}
				wantControl := control
				if control == "ACTIVE" {
					wantControl = "PAUSING"
				}
				if workflow.Control != wantControl || workflow.Generation != 2 || workflow.Review.Verdict != "OBSOLETE" {
					t.Errorf("control %s changed to control=%s generation=%d verdict=%s", control, workflow.Control, workflow.Generation, workflow.Review.Verdict)
				}
			}

			waitingBase := baseRepo + 100 + int64(changeIndex)
			for i, control := range controls {
				workflow := domain.Workflow{
					ID: domain.ID(waitingBase, i+1), RepositoryID: waitingBase,
					Repository: "example/pilot", Issue: i + 1, Phase: "WAITING_CONFIGURATION", Control: control,
					Review: domain.Review{Key: "old-key", Verdict: "BLOCKED"},
				}
				if err := s.Create(ctx, workflow); err != nil {
					t.Fatal("create waiting-configuration fixture")
				}
			}
			waitingEngine := &Engine{Store: s, cfg: oldConfig, active: make(map[string]context.CancelFunc)}
			if err := waitingEngine.SetConfig(ctx, newConfig); err != nil {
				t.Fatal("reapply changed config for waiting-configuration fixtures")
			}
			for i, control := range controls {
				workflow, err := s.Get(ctx, domain.ID(waitingBase, i+1))
				if err != nil {
					t.Fatal("read waiting-configuration fixture")
				}
				if control == "ACTIVE" {
					if workflow.Control != "ACTIVE" || workflow.Phase != "TRIAGE" || workflow.Generation != 2 {
						t.Errorf("active waiting workflow not released safely: control=%s phase=%s generation=%d", workflow.Control, workflow.Phase, workflow.Generation)
					}
				} else if workflow.Control != control || workflow.Phase != "WAITING_CONFIGURATION" || workflow.Generation != 2 || workflow.Review.Verdict != "OBSOLETE" {
					t.Errorf("stopped waiting workflow was revived or not invalidated safely: original=%s control=%s phase=%s generation=%d verdict=%s", control, workflow.Control, workflow.Phase, workflow.Generation, workflow.Review.Verdict)
				}
			}
		})
	}

	// Saving an incomplete Responses config must not release a workflow whose
	// active model backend is still unavailable.
	responsesRepo := baseRepo + 500
	responseOld := config.Config{}
	responseOld.Runner.Mode = "responses"
	responseNew := responseOld
	responseNew.Model.Dev = "partial-config"
	responseWaiting := domain.Workflow{
		ID: domain.ID(responsesRepo, 1), RepositoryID: responsesRepo,
		Repository: "example/pilot", Issue: 1, Phase: "WAITING_CONFIGURATION", Control: "ACTIVE",
		Review: domain.Review{Key: "old-key", Verdict: "BLOCKED"},
	}
	if err := s.Create(ctx, responseWaiting); err != nil {
		t.Fatal("create unready Responses fixture")
	}
	responseEngine := &Engine{Store: s, cfg: responseOld, active: make(map[string]context.CancelFunc)}
	if err := responseEngine.SetConfig(ctx, responseNew); err != nil {
		t.Fatal("apply incomplete Responses config")
	}
	responseGot, err := s.Get(ctx, responseWaiting.ID)
	if err != nil {
		t.Fatal("read unready Responses fixture")
	}
	if responseGot.Control != "ACTIVE" || responseGot.Phase != "WAITING_CONFIGURATION" || responseGot.Generation != 2 || responseGot.Review.Verdict != "OBSOLETE" {
		t.Fatalf("incomplete Responses config released waiting workflow: control=%s phase=%s generation=%d verdict=%s", responseGot.Control, responseGot.Phase, responseGot.Generation, responseGot.Review.Verdict)
	}

	// Invalidate follows the same rule: only ACTIVE enters PAUSING, while
	// every already-selected control state survives and the review key is stale.
	const invalidateRepo int64 = 789012
	for i, control := range controls {
		workflow := domain.Workflow{
			ID: domain.ID(invalidateRepo, i+1), RepositoryID: invalidateRepo,
			Repository: "example/pilot", Issue: i + 1, Phase: "REVIEWING", Control: control,
			Review: domain.Review{Key: "old-key", Verdict: "PASS"},
		}
		if err := s.Create(ctx, workflow); err != nil {
			t.Fatal("create invalidation fixture")
		}
	}
	engine := &Engine{Store: s, cfg: config.Config{}, active: make(map[string]context.CancelFunc)}
	if err := engine.Invalidate(ctx, "policy changed"); err != nil {
		t.Fatal("invalidate workflows")
	}
	for i, control := range controls {
		workflow, err := s.Get(ctx, domain.ID(invalidateRepo, i+1))
		if err != nil {
			t.Fatal("read invalidation fixture")
		}
		wantControl := control
		if control == "ACTIVE" {
			wantControl = "PAUSING"
		}
		if workflow.Control != wantControl || workflow.Generation != 2 || workflow.Review.Verdict != "OBSOLETE" {
			t.Errorf("Invalidate overwrote or failed to invalidate control %s: control=%s generation=%d verdict=%s", control, workflow.Control, workflow.Generation, workflow.Review.Verdict)
		}
	}

	// A WAITING_CONFIGURATION workflow is skipped by Invalidate.
	waiting := domain.Workflow{ID: domain.ID(invalidateRepo, 20), RepositoryID: invalidateRepo, Repository: "example/pilot", Issue: 20, Phase: "WAITING_CONFIGURATION", Control: "CANCELLED"}
	if err := s.Create(ctx, waiting); err != nil {
		t.Fatal("create invalidation waiting fixture")
	}
	if err := engine.Invalidate(ctx, "second policy change"); err != nil {
		t.Fatal("invalidate waiting workflow")
	}
	got, err := s.Get(ctx, waiting.ID)
	if err != nil {
		t.Fatal("read invalidation waiting fixture")
	}
	if got.Control != "CANCELLED" || got.Phase != "WAITING_CONFIGURATION" || got.Generation != 1 {
		t.Fatalf("Invalidate modified waiting workflow: control=%s phase=%s generation=%d", got.Control, got.Phase, got.Generation)
	}
}

func controlTestSchemaURL(dbURL, schema string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("database URL must be a postgres URL")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	return u.String(), nil
}
