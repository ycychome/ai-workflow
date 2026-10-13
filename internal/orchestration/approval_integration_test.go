package orchestration

import (
	"context"
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

// This exercises AI approval transitions against an isolated PostgreSQL schema.
// The fake triage path does not call a model, and Engine.Run is never started.
func TestApprovalWorkflowPostgresIntegration(t *testing.T) {
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

	schema := fmt.Sprintf("approval_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create temporary test schema: %v", err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop owned temporary test schema: %v", err)
		}
	}()
	dbURL, err := approvalSchemaURL(cfg.DatabaseURL, schema)
	if err != nil {
		t.Fatalf("configure isolated database search path: %v", err)
	}
	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open isolated test store: %v", err)
	}
	defer s.Close()

	cfg.Runner.Mode = "fake"
	cfg.Repositories = nil
	cfg.DataDir = t.TempDir()
	defaultCfg := cfg
	defaultCfg.Runner.RequireAcceptForDevelopment = false
	defaultEngine, err := New(s, defaultCfg)
	if err != nil {
		t.Fatalf("create default-mode engine: %v", err)
	}

	// Default behavior accepts the valid AI triage proposal and starts planning.
	defaultW := approvalFixture(882341, 1, "TRIAGE", "ACTIVE", false)
	if err := s.Create(ctx, defaultW); err != nil {
		t.Fatalf("create default triage workflow: %v", err)
	}
	claimed, err := s.Claim(ctx, defaultEngine.owner)
	if err != nil || claimed.ID != defaultW.ID {
		t.Fatalf("claim default triage fixture: id=%s err=%v", claimed.ID, err)
	}
	if err := defaultEngine.triage(ctx, &claimed); err != nil {
		t.Fatalf("fake default triage: %v", err)
	}
	s.Release(ctx, claimed)
	accepted, err := s.Get(ctx, defaultW.ID)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Phase != "PLANNING" || !accepted.Spec.Approved || accepted.Spec.ApprovedBy != "ai:dev-triage" || accepted.ProposalSpec != nil {
		t.Fatalf("default AI triage did not approve and enter planning: phase=%s approved=%v actor=%s", accepted.Phase, accepted.Spec.Approved, accepted.Spec.ApprovedBy)
	}
	assertApprovalAuditCount(t, ctx, s, defaultW.ID, 1)
	if err := s.Update(ctx, defaultW.ID, func(w *domain.Workflow) ([]store.Action, error) {
		w.Control = "PAUSED"
		return nil, nil
	}); err != nil {
		t.Fatalf("pause completed default fixture: %v", err)
	}

	// With the manual gate enabled, triage only proposes; /ai accept is the
	// transition that approves the spec and attributes approval to the maintainer.
	manualCfg := defaultCfg
	manualCfg.Runner.RequireAcceptForDevelopment = true
	manualEngine, err := New(s, manualCfg)
	if err != nil {
		t.Fatalf("create manual-gate engine: %v", err)
	}
	manualW := approvalFixture(882341, 2, "TRIAGE", "ACTIVE", false)
	if err := s.Create(ctx, manualW); err != nil {
		t.Fatalf("create manual-gate workflow: %v", err)
	}
	claimed, err = s.Claim(ctx, manualEngine.owner)
	if err != nil || claimed.ID != manualW.ID {
		t.Fatalf("claim manual-gate fixture: id=%s err=%v", claimed.ID, err)
	}
	if err := manualEngine.triage(ctx, &claimed); err != nil {
		t.Fatalf("fake manual-gate triage: %v", err)
	}
	proposed, err := s.Get(ctx, manualW.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proposed.Phase != "SPEC_READY" || proposed.ProposalSpec == nil || proposed.Spec.Approved {
		t.Fatalf("manual-gate triage should wait in SPEC_READY: phase=%s proposal=%v approved=%v", proposed.Phase, proposed.ProposalSpec != nil, proposed.Spec.Approved)
	}
	if err := manualEngine.Command(ctx, manualW.ID, "maintainer", "/ai accept", true); err != nil {
		t.Fatalf("manual accept proposal: %v", err)
	}
	s.Release(ctx, claimed)
	manuallyAccepted, err := s.Get(ctx, manualW.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manuallyAccepted.Phase != "PLANNING" || !manuallyAccepted.Spec.Approved || manuallyAccepted.Spec.ApprovedBy != "maintainer" || manuallyAccepted.ProposalSpec != nil {
		t.Fatalf("manual accept did not enter planning under maintainer approval: phase=%s actor=%s", manuallyAccepted.Phase, manuallyAccepted.Spec.ApprovedBy)
	}
	if err := s.Update(ctx, manualW.ID, func(w *domain.Workflow) ([]store.Action, error) {
		w.Control = "PAUSED"
		return nil, nil
	}); err != nil {
		t.Fatalf("pause completed manual fixture: %v", err)
	}

	// Turning off the manual gate releases only active, current proposals.
	releaseIDs := []string{
		domain.ID(882341, 3), domain.ID(882341, 4), domain.ID(882341, 5), domain.ID(882341, 6),
	}
	proposals := []domain.Workflow{
		approvalFixture(882341, 3, "SPEC_READY", "ACTIVE", true),
		approvalFixture(882341, 4, "SPEC_READY", "PAUSED", true),
		approvalFixture(882341, 5, "SPEC_READY", "CANCELLED", true),
		approvalFixture(882341, 6, "SPEC_READY", "ACTIVE", true),
	}
	proposals[3].ProposalSpec.SourceHash = normalizedIssueSourceHash(proposals[3].IssueTitle, proposals[3].IssueBody+" changed")
	proposals[3].ProposalSpec.Digest = domain.SpecDigest(*proposals[3].ProposalSpec)
	for _, w := range proposals {
		if err := s.Create(ctx, w); err != nil {
			t.Fatalf("create release fixture %s: %v", w.ID, err)
		}
	}
	if err := manualEngine.SetConfig(ctx, defaultCfg); err != nil {
		t.Fatalf("disable manual approval gate: %v", err)
	}
	released, err := s.Get(ctx, releaseIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if released.Phase != "PLANNING" || !released.Spec.Approved || released.Spec.ApprovedBy != "ai:dev-triage" || released.ProposalSpec != nil {
		t.Fatalf("active current proposal was not released: phase=%s actor=%s", released.Phase, released.Spec.ApprovedBy)
	}
	for _, id := range releaseIDs[1:] {
		w, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if w.Phase != "SPEC_READY" || w.ProposalSpec == nil || w.Spec.Approved {
			t.Errorf("paused/cancelled/stale proposal %s was incorrectly released: phase=%s approved=%v", id, w.Phase, w.Spec.Approved)
		}
	}
	assertApprovalAuditCount(t, ctx, s, releaseIDs[0], 1)
	if err := s.Update(ctx, releaseIDs[0], func(w *domain.Workflow) ([]store.Action, error) {
		w.Control = "PAUSED"
		return nil, nil
	}); err != nil {
		t.Fatalf("pause released proposal fixture: %v", err)
	}

	// A triage result based on an expired lease or old generation cannot commit,
	// and its AI approval audit must remain absent.
	for i, mode := range []string{"expired-lease", "stale-generation"} {
		issue := 10 + i
		w := approvalFixture(882341, issue, "TRIAGE", "ACTIVE", false)
		if err := s.Create(ctx, w); err != nil {
			t.Fatalf("create stale triage fixture: %v", err)
		}
		oldLease, err := s.Claim(ctx, "approval-stale-"+mode)
		if err != nil || oldLease.ID != w.ID {
			t.Fatalf("claim %s fixture: id=%s err=%v", mode, oldLease.ID, err)
		}
		if mode == "expired-lease" {
			if _, err := s.DB.Exec(ctx, "UPDATE workflows SET lease_until=now()-interval '1 second' WHERE id=$1", w.ID); err != nil {
				t.Fatalf("expire fixture lease: %v", err)
			}
		} else {
			if err := s.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
				x.Generation++
				return nil, nil
			}); err != nil {
				t.Fatalf("advance fixture generation: %v", err)
			}
		}
		if err := defaultEngine.triage(ctx, &oldLease); !errors.Is(err, store.ErrStale) {
			t.Errorf("AI triage with %s should be fenced by Save, got %v", mode, err)
		}
		s.Release(ctx, oldLease)
		assertApprovalAuditCount(t, ctx, s, w.ID, 0)
		stored, err := s.Get(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Spec.Approved || stored.Phase != "TRIAGE" {
			t.Errorf("stale AI report changed persisted workflow for %s: phase=%s approved=%v", mode, stored.Phase, stored.Spec.Approved)
		}
		if err := s.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			x.Control = "PAUSED"
			return nil, nil
		}); err != nil {
			t.Fatalf("pause completed stale fixture: %v", err)
		}
	}
}

func approvalFixture(repoID int64, issue int, phase, control string, proposal bool) domain.Workflow {
	w := domain.Workflow{
		ID: domain.ID(repoID, issue), RepositoryID: repoID, Repository: "local/pilot", Issue: issue,
		IssueTitle: "Fixture: repair Add", IssueBody: fmt.Sprintf("Issue fixture %d", issue), Phase: phase, Control: control,
	}
	if proposal {
		p := domain.Spec{Revision: 1, Title: "Approved fixture", Description: "Implement the complete request.", Criteria: []domain.Criterion{{ID: "AC-1", Description: "The request is implemented."}}, SourceHash: normalizedIssueSourceHash(w.IssueTitle, w.IssueBody)}
		p.Digest = domain.SpecDigest(p)
		w.ProposalSpec = &p
	}
	return w
}

func approvalSchemaURL(dbURL, schema string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("database URL must be a postgres URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func assertApprovalAuditCount(t *testing.T, ctx context.Context, s *store.Store, workflowID string, want int) {
	t.Helper()
	var got int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM audit WHERE workflow_id=$1 AND kind='ai_spec_approved'`, workflowID).Scan(&got); err != nil {
		t.Fatalf("count AI approval audit for %s: %v", workflowID, err)
	}
	if got != want {
		t.Fatalf("AI approval audit count for %s = %d, want %d", workflowID, got, want)
	}
}
