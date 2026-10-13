package orchestration

import (
	"context"
	"encoding/json"
	"errors"
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

// This test exercises the PostgreSQL outbox only. It does not start a worker
// or construct a GitHub client, so all effects remain inside its temporary schema.
func TestLabelOutboxPostgresIntegration(t *testing.T) {
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
		t.Fatalf("load controlled config: %v", err)
	}
	admin, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("connect controlled database: %v", err)
	}
	defer admin.Close()

	schema := fmt.Sprintf("labels_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create temporary test schema: %v", err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop owned temporary test schema: %v", err)
		}
	}()
	dbURL, err := labelSchemaURL(cfg.DatabaseURL, schema)
	if err != nil {
		t.Fatalf("configure isolated database search path: %v", err)
	}
	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open isolated test store: %v", err)
	}
	defer s.Close()

	const repoID int64 = 771231
	workflow := domain.Workflow{ID: domain.ID(repoID, 1), RepositoryID: repoID, Repository: "example/pilot", InstallationID: 123, Issue: 1, IssueTitle: "test", IssueBody: "fixture", Phase: "INTAKE", Control: "ACTIVE"}
	if err := s.Create(ctx, workflow); err != nil {
		t.Fatalf("create label workflow: %v", err)
	}
	assertLabelActionCount(t, ctx, s, workflow.ID, 1)

	if err := s.Update(ctx, workflow.ID, func(w *domain.Workflow) ([]store.Action, error) {
		w.Phase, w.Control, w.PR = "REVIEWING", "PAUSED", 91
		return nil, nil
	}); err != nil {
		t.Fatalf("update phase/control/PR: %v", err)
	}
	assertLabelActionCount(t, ctx, s, workflow.ID, 2)
	var latestData []byte
	if err := s.DB.QueryRow(ctx, `SELECT data FROM outbox WHERE workflow_id=$1 AND kind='labels' ORDER BY created_at DESC,id DESC LIMIT 1`, workflow.ID).Scan(&latestData); err != nil {
		t.Fatal(err)
	}
	var desired struct {
		Phase   string `json:"phase"`
		Control string `json:"control"`
		PR      int    `json:"pr"`
	}
	if err := json.Unmarshal(latestData, &desired); err != nil || desired.Phase != "REVIEWING" || desired.Control != "PAUSED" || desired.PR != 91 {
		t.Fatalf("queued labels do not reflect phase/control/PR: %+v (%v)", desired, err)
	}
	if err := s.Update(ctx, workflow.ID, func(w *domain.Workflow) ([]store.Action, error) {
		w.LastError = "non-label change"
		return nil, nil
	}); err != nil {
		t.Fatalf("same-label update: %v", err)
	}
	assertLabelActionCount(t, ctx, s, workflow.ID, 2)

	// Claim both independently while labels are pending/running for the same workflow.
	labelAction, err := s.ClaimLabelAction(ctx)
	if err != nil || labelAction.Kind != "labels" || labelAction.WorkflowID != workflow.ID {
		t.Fatalf("label claimant must select only the expected label action: kind=%s workflow=%s err=%v", labelAction.Kind, labelAction.WorkflowID, err)
	}
	if err := s.FinishAction(ctx, labelAction, nil, nil, false); err != nil {
		t.Fatalf("finish first fixture label action: %v", err)
	}
	if err := s.Update(ctx, workflow.ID, func(w *domain.Workflow) ([]store.Action, error) {
		w.Control = "ACTIVE"
		return []store.Action{
			store.NewAction(*w, "comment", map[string]any{"body": "fixture"}),
			store.NewAction(*w, "publish", map[string]any{"fixture": true}),
		}, nil
	}); err != nil {
		t.Fatalf("queue ordinary actions: %v", err)
	}
	comment, err := s.ClaimAction(ctx)
	if err != nil || comment.Kind != "comment" {
		t.Fatalf("ordinary outbox claimant must skip label rows and claim comment: kind=%s err=%v", comment.Kind, err)
	}
	if err := s.FinishAction(ctx, comment, nil, nil, false); err != nil {
		t.Fatalf("finish fixture comment: %v", err)
	}
	publish, err := s.ClaimAction(ctx)
	if err != nil || publish.Kind != "publish" {
		t.Fatalf("ordinary outbox claimant must claim publish while labels remain queued: kind=%s err=%v", publish.Kind, err)
	}
	if err := s.FinishAction(ctx, publish, nil, nil, false); err != nil {
		t.Fatalf("finish fixture publish: %v", err)
	}

	if err := s.Update(ctx, workflow.ID, func(w *domain.Workflow) ([]store.Action, error) {
		w.Generation++
		w.Phase, w.Control = "BLOCKED", "CANCELLED"
		return nil, nil
	}); err != nil {
		t.Fatalf("cancel fixture workflow: %v", err)
	}
	labelAction, err = s.ClaimLabelAction(ctx)
	if err != nil || labelAction.Kind != "labels" {
		t.Fatalf("label claimant should continue to claim only labels: kind=%s err=%v", labelAction.Kind, err)
	}
	if err := s.StartAction(ctx, labelAction); err != nil {
		t.Fatalf("informational label action should start after cancellation: %v", err)
	}
	if err := s.FinishAction(ctx, labelAction, nil, nil, false); err != nil {
		t.Fatalf("finish cancelled workflow label action: %v", err)
	}

	// Save must enqueue changed labels while retaining a valid worker lease.
	saveWorkflow := domain.Workflow{ID: domain.ID(repoID, 2), RepositoryID: repoID, Repository: "example/pilot", InstallationID: 123, Issue: 2, Phase: "TRIAGE", Control: "ACTIVE"}
	if err := s.Create(ctx, saveWorkflow); err != nil {
		t.Fatalf("create Save fixture: %v", err)
	}
	claimed, err := s.Claim(ctx, "labels-integration-save")
	if err != nil || claimed.ID != saveWorkflow.ID {
		t.Fatalf("claim Save fixture workflow: got=%s err=%v", claimed.ID, err)
	}
	claimed.Phase = "PLANNING"
	if err := s.Save(ctx, &claimed); err != nil {
		t.Fatalf("Save changed phase: %v", err)
	}
	saved, err := s.Get(ctx, saveWorkflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Phase != "PLANNING" || saved.LeaseOwner != "labels-integration-save" {
		t.Fatalf("Save should update phase without losing its lease: phase=%s owner=%s", saved.Phase, saved.LeaseOwner)
	}
	assertLabelActionCount(t, ctx, s, saveWorkflow.ID, 2)

	// Seed terminal/existing workflows and a population beyond the Store.List cap.
	seedWorkflows := []domain.Workflow{
		{ID: domain.ID(repoID, 3), RepositoryID: repoID, Repository: "example/pilot", InstallationID: 123, Issue: 3, Phase: "INTAKE", Control: "ACTIVE"},
		{ID: domain.ID(repoID, 4), RepositoryID: repoID, Repository: "example/pilot", InstallationID: 123, Issue: 4, Phase: "BLOCKED", Control: "CANCELLED"},
		{ID: domain.ID(repoID, 5), RepositoryID: repoID, Repository: "example/pilot", InstallationID: 123, Issue: 5, Phase: "MERGED", Control: "COMPLETED", PR: 55},
	}
	for _, w := range seedWorkflows {
		if err := s.Create(ctx, w); err != nil {
			t.Fatalf("create repair seed %s: %v", w.ID, err)
		}
	}
	if _, err := s.DB.Exec(ctx, `UPDATE outbox SET state='done',lease_until=NULL WHERE kind='labels'`); err != nil {
		t.Fatalf("settle fixture label actions before repair: %v", err)
	}
	if _, err := s.DB.Exec(ctx, `
INSERT INTO workflows(id,repository_id,issue,phase,control,generation,version,data,lease_owner,lease_until)
SELECT 'bulk-label-'||g,771232,g,
 CASE WHEN g=2 THEN 'MERGED' WHEN g=1 THEN 'BLOCKED' ELSE 'INTAKE' END,
 CASE WHEN g=2 THEN 'COMPLETED' WHEN g=1 THEN 'CANCELLED' ELSE 'ACTIVE' END,
 1,1,jsonb_build_object('id','bulk-label-'||g,'repository','example/bulk','installation_id',123,'issue',g,'pr',g+1000,'phase',CASE WHEN g=2 THEN 'MERGED' WHEN g=1 THEN 'BLOCKED' ELSE 'INTAKE' END,'control',CASE WHEN g=2 THEN 'COMPLETED' WHEN g=1 THEN 'CANCELLED' ELSE 'ACTIVE' END),
	 'sentinel-owner',now()+interval '1 hour'
FROM generate_series(1,230) g`); err != nil {
		t.Fatalf("bulk seed repair population: %v", err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO workflow_metrics(workflow_id,calls,input_tokens,output_tokens,runtime_seconds) SELECT 'bulk-label-'||g,2,3,4,5 FROM generate_series(1,230) g`); err != nil {
		t.Fatalf("seed bulk metrics: %v", err)
	}
	before, err := labelWorkflowSnapshot(ctx, s)
	if err != nil {
		t.Fatalf("snapshot before repair: %v", err)
	}
	var eligible int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM workflows w WHERE w.data->>'repository'<>'local/pilot' AND COALESCE((w.data->>'installation_id')::bigint,0)>0 AND w.issue>0`).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	if eligible <= 200 {
		t.Fatalf("repair fixture must exceed the 200-row list cap, got %d", eligible)
	}
	if err := s.QueueLabelRepair(ctx); err != nil {
		t.Fatalf("queue label repair: %v", err)
	}
	var repaired int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind='labels' AND id LIKE '%:labels-repair:%'`).Scan(&repaired); err != nil {
		t.Fatal(err)
	}
	if repaired != eligible {
		t.Fatalf("repair should enqueue every eligible workflow: got %d want %d", repaired, eligible)
	}
	for _, id := range []string{seedWorkflows[0].ID, seedWorkflows[1].ID, seedWorkflows[2].ID, "bulk-label-1", "bulk-label-2"} {
		var count int
		if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id=$1 AND kind='labels' AND id LIKE '%:labels-repair:%'`, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("repair omitted or duplicated workflow %s: count=%d", id, count)
		}
	}
	if err := s.QueueLabelRepair(ctx); err != nil {
		t.Fatalf("repeat label repair in same bucket: %v", err)
	}
	var repairedAgain int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind='labels' AND id LIKE '%:labels-repair:%'`).Scan(&repairedAgain); err != nil {
		t.Fatal(err)
	}
	if repairedAgain != repaired {
		t.Fatalf("same repair bucket must not duplicate actions: before=%d after=%d", repaired, repairedAgain)
	}
	after, err := labelWorkflowSnapshot(ctx, s)
	if err != nil {
		t.Fatalf("snapshot after repair: %v", err)
	}
	if before != after {
		t.Fatal("label repair changed workflow status, lease, or metrics")
	}
}

func labelSchemaURL(dbURL, schema string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("database URL must be a postgres URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func assertLabelActionCount(t *testing.T, ctx context.Context, s *store.Store, workflowID string, want int) {
	t.Helper()
	var got int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id=$1 AND kind='labels'`, workflowID).Scan(&got); err != nil {
		t.Fatalf("count label actions for %s: %v", workflowID, err)
	}
	if got != want {
		t.Fatalf("label action count for %s = %d, want %d", workflowID, got, want)
	}
}

func labelWorkflowSnapshot(ctx context.Context, s *store.Store) (string, error) {
	var workflowState, metricState string
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(string_agg(w.id||':'||w.phase||':'||w.control||':'||w.generation::text||':'||w.version::text||':'||w.lease_owner||':'||COALESCE(w.lease_until::text,''), '|' ORDER BY w.id),'') FROM workflows w`).Scan(&workflowState)
	if err != nil {
		return "", err
	}
	err = s.DB.QueryRow(ctx, `SELECT COALESCE(string_agg(workflow_id||':'||calls||':'||input_tokens||':'||output_tokens||':'||runtime_seconds, '|' ORDER BY workflow_id),'') FROM workflow_metrics`).Scan(&metricState)
	if err != nil {
		return "", err
	}
	return workflowState + "\n" + metricState, nil
}
