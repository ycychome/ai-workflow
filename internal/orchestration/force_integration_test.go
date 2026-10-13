package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

// This test uses only a temporary PostgreSQL schema and the local/pilot
// fixture. It never starts Engine.Run, which would execute outbox effects.
func TestForceCommandsPostgresIntegration(t *testing.T) {
	if os.Getenv("AI_WORKFLOW_FORCE_INTEGRATION") != "1" {
		t.Skip("set AI_WORKFLOW_FORCE_INTEGRATION=1 in the controlled integration environment")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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

	schema := fmt.Sprintf("force_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create temporary test schema: %v", err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop owned temporary test schema: %v", err)
		}
	}()
	dbURL, err := schemaURL(cfg.DatabaseURL, schema)
	if err != nil {
		t.Fatalf("configure isolated database search path: %v", err)
	}
	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open isolated test store: %v", err)
	}
	defer s.Close()

	cfg.Runner.Mode = "fake"
	cfg.DataDir = t.TempDir()
	cfg.Repositories = []config.Repository{{Name: "local/pilot", ID: 991337, BaseBranch: "main", Maintainers: []string{"maintainer"}}}
	e, err := New(s, cfg)
	if err != nil {
		t.Fatalf("create test engine: %v", err)
	}

	// /ai accept on intake is the documented alias for requesting analysis.
	intake := domain.Workflow{ID: domain.ID(991337, 1), RepositoryID: 991337, Repository: "local/pilot", Issue: 1, IssueTitle: "fixture", IssueBody: "full request body", Phase: "INTAKE", Control: "ACTIVE"}
	if err := s.Create(ctx, intake); err != nil {
		t.Fatalf("create intake fixture: %v", err)
	}
	if err := e.Command(ctx, intake.ID, "maintainer", "/ai accept", true); err != nil {
		t.Fatalf("accept intake alias: %v", err)
	}
	got, err := s.Get(ctx, intake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "TRIAGE" || got.Spec.Approved || got.ModelCalls != 0 {
		t.Fatalf("intake accept must request analysis without approving a spec: phase=%s approved=%v calls=%d", got.Phase, got.Spec.Approved, got.ModelCalls)
	}

	// Force development preserves the complete source request and existing constraints.
	dev := domain.Workflow{ID: domain.ID(991337, 2), RepositoryID: 991337, Repository: "local/pilot", Issue: 2, IssueTitle: "unreviewed request", IssueBody: "line one\nline two\nall requested details", Phase: "TRIAGE", Control: "ACTIVE", Discussion: []string{"prior answer"}, Spec: domain.Spec{Revision: 2, Criteria: []domain.Criterion{{ID: "OLD", Description: "preserve this constraint"}}}}
	if err := s.Create(ctx, dev); err != nil {
		t.Fatalf("create development fixture: %v", err)
	}
	if err := e.Command(ctx, dev.ID, "maintainer", "/ai forceaccept dev", true); err != nil {
		t.Fatalf("force accept development: %v", err)
	}
	got, err = s.Get(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "PLANNING" || !got.Spec.Approved || got.Spec.SourceHash != normalizedIssueHash(dev.IssueBody) || !strings.Contains(got.Spec.Description, dev.IssueBody) || !strings.Contains(got.Spec.Description, "preserve this constraint") || got.ForceApproval == nil || got.ForceApproval.Kind != "issue_development" {
		t.Fatalf("forceaccept dev did not preserve and approve the full request: phase=%s", got.Phase)
	}

	// Force merge approval remains a manual override: the normal domain gate
	// stays false, while the gate outbox record explicitly carries the override.
	finding := domain.Finding{ID: "F-1", Severity: "high", Description: "unresolved fixture finding", State: "OPEN"}
	pr := domain.Workflow{ID: domain.ID(991337, 3), RepositoryID: 991337, Repository: "local/pilot", Issue: 3, IssueTitle: "PR fixture", IssueBody: "request", Phase: "BLOCKED", Control: "ACTIVE", PR: 33, BaseSHA: "base-sha", HeadSHA: "head-sha", Spec: domain.Spec{Title: "fixture", Description: "fixture spec", Criteria: []domain.Criterion{{ID: "AC1", Description: "verified"}}, Approved: true, ApprovedBy: "maintainer"}, Review: domain.Review{VerificationPassed: false, SkepticComplete: false, Verdict: "BLOCKED", Obligations: []domain.Obligation{{ID: "O1", CriterionID: "AC1", Result: "unverified"}}, Findings: []domain.Finding{finding}}}
	pr.Spec.Digest = domain.SpecDigest(pr.Spec)
	if err := s.Create(ctx, pr); err != nil {
		t.Fatalf("create PR fixture: %v", err)
	}
	if err := e.Command(ctx, pr.ID, "maintainer", "/ai forceaccept merge", true); err != nil {
		t.Fatalf("force accept merge gate: %v", err)
	}
	got, err = s.Get(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "AWAITING_MAINTAINER" || got.ForceApproval == nil || got.ForceApproval.Kind != "pr_merge_gate" || got.Review.Verdict != "FORCED" || got.Review.VerificationPassed || got.Review.SkepticComplete || len(got.Review.Findings) != 1 || got.Review.Findings[0].State != finding.State {
		t.Fatalf("force approval must preserve the existing unresolved verification state: phase=%s", got.Phase)
	}
	if ok, _ := domain.Gate(got, got.Review.Key); ok {
		t.Fatal("manual force approval must not make the ordinary domain gate pass")
	}
	if !e.ForceApprovalCurrent(ctx, got) {
		t.Fatal("approval should match the stored PR identity")
	}
	changedHead := got
	changedHead.HeadSHA = "changed-head"
	if e.ForceApprovalCurrent(ctx, changedHead) {
		t.Fatal("changed PR head must invalidate the current force approval")
	}
	var gateData []byte
	if err := s.DB.QueryRow(ctx, `SELECT data FROM outbox WHERE workflow_id=$1 AND kind='gate' AND state='pending' ORDER BY created_at DESC LIMIT 1`, pr.ID).Scan(&gateData); err != nil {
		t.Fatalf("read pending gate action: %v", err)
	}
	var gate struct {
		Pass           bool   `json:"pass"`
		ManualOverride bool   `json:"manual_override"`
		Reason         string `json:"reason"`
	}
	if err := json.Unmarshal(gateData, &gate); err != nil || !gate.Pass || !gate.ManualOverride || !strings.Contains(gate.Reason, "MANUAL OVERRIDE") {
		t.Fatalf("gate outbox action must carry explicit manual override: %v", err)
	}
	for _, row := range []struct{ id, kind string }{{dev.ID, "force_accept_issue_development"}, {pr.ID, "force_accept_pr_gate"}} {
		var count int
		if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM audit WHERE workflow_id=$1 AND kind=$2 AND data->>'actor'='maintainer'", row.id, row.kind).Scan(&count); err != nil || count != 1 {
			t.Fatalf("force decision must record its operator exactly once: count=%d error=%v", count, err)
		}
	}

	var auditsBefore int
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM audit WHERE workflow_id=$1", pr.ID).Scan(&auditsBefore); err != nil {
		t.Fatal(err)
	}
	staleCtx := store.WithExpectedState(ctx, got.Generation, "old-head")
	err = s.Update(staleCtx, pr.ID, func(x *domain.Workflow) ([]store.Action, error) {
		x.LastError = "must not commit"
		return nil, nil
	})
	if !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale expected head should be rejected while row is locked, got %v", err)
	}
	after, err := s.Get(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	var auditsAfter int
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM audit WHERE workflow_id=$1", pr.ID).Scan(&auditsAfter); err != nil {
		t.Fatal(err)
	}
	if after.Version != got.Version || after.LastError != got.LastError || auditsAfter != auditsBefore {
		t.Fatal("rejected stale update changed workflow state or audit history")
	}
}

func schemaURL(dbURL, schema string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("database URL must be a postgres URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
