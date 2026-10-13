package orchestration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/store"
)

func TestIssueSourceHashBindsTitleAndBody(t *testing.T) {
	a := normalizedIssueSourceHash("Use ascending order", "Sort the input")
	if a == normalizedIssueSourceHash("Use descending order", "Sort the input") {
		t.Fatal("title-only change retained source identity")
	}
	if a == normalizedIssueSourceHash("Use ascending order", "Keep original order") {
		t.Fatal("body-only change retained source identity")
	}
	if a == normalizedIssueSourceHash(" Use ascending\norder ", " Sort  the input ") {
		t.Fatal("semantic whitespace change retained source identity")
	}
}

// All effects stay in an owned PostgreSQL schema or a localhost mock GitHub.
func TestIssueDiscussionAndTitlePostgresRegression(t *testing.T) {
	if os.Getenv("AI_WORKFLOW_FORCE_INTEGRATION") != "1" {
		t.Skip("opt-in isolated PostgreSQL regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg, err := config.Load(os.Getenv("CONFIG_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("issue_state_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	isolated, err := approvalSchemaURL(cfg.DatabaseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, isolated)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.Runner.Mode = "fake"
	cfg.DataDir = t.TempDir()
	cfg.Repositories = nil
	e, err := New(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := domain.Workflow{ID: domain.ID(991911, 1), RepositoryID: 991911, Repository: "local/pilot", Issue: 1, IssueTitle: "Fixture: repair Add", IssueBody: "request", Phase: "DEVELOPING", Control: "ACTIVE", Tasks: []domain.Task{{ID: "build", Kind: "code", Status: "RUNNING"}}}
	if err = s.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	worker, err := s.Claim(ctx, "discussion-worker")
	if err != nil {
		t.Fatal(err)
	}
	for _, comment := range []struct {
		id                int64
		actor, kind, body string
	}{{11, "user", "User", "Additional context"}, {12, "bot", "Bot", "/ai status"}, {11, "user", "User", "Additional context"}} {
		if err = e.issueComment(ctx, worker, comment.id, comment.actor, comment.kind, comment.body); err != nil {
			t.Fatal(err)
		}
	}
	discussed, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if discussed.Version != worker.Version || discussed.Generation != worker.Generation || len(discussed.Discussion) != 2 {
		t.Fatalf("discussion changed execution fence or duplicated comment: %+v", discussed)
	}
	worker.Tasks[0].Status = "INTEGRATED"
	worker.Phase = "VERIFYING"
	if err = s.Save(ctx, &worker); err != nil {
		t.Fatalf("ordinary discussion discarded worker result: %v", err)
	}
	saved, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Tasks[0].Status != "INTEGRATED" || saved.Phase != "VERIFYING" || len(saved.Discussion) != 2 || len(worker.Discussion) != 2 {
		t.Fatal("result or concurrent discussion lost during save")
	}
	if err = e.Command(ctx, w.ID, "maintainer", "pause", true); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(ctx, &worker); !errors.Is(err, store.ErrStale) {
		t.Fatalf("pause must still reject stale result, got %v", err)
	}

	// Body-only legacy approvals bind to their original persisted title at upgrade.
	legacy := domain.Workflow{ID: domain.ID(991911, 2), RepositoryID: 991911, Repository: "local/pilot", Issue: 2, IssueTitle: "Use ascending order", IssueBody: "Sort input", Phase: "AWAITING_MAINTAINER", Control: "ACTIVE", PR: 9, HeadSHA: "head", BaseSHA: "base", BaseBranch: "main", Review: domain.Review{Verdict: "PASS", Key: "prior"}}
	legacy.Spec = domain.Spec{Title: "Sort task", Description: "Sort input", SourceHash: normalizedIssueHash(legacy.IssueBody), Approved: true, Criteria: []domain.Criterion{{ID: "AC1", Description: "Ascending"}}}
	legacy.Spec.Digest = domain.SpecDigest(legacy.Spec)
	if err = s.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, "UPDATE workflows SET lease_owner='pre-upgrade',lease_until=now()+interval '90 seconds' WHERE id=$1", legacy.ID); err != nil {
		t.Fatal(err)
	}
	preUpgrade, err := s.Get(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate upgrading an existing v1 database in this owned schema.
	if _, err = s.DB.Exec(ctx, "DELETE FROM schema_migrations WHERE version IN (2,3)"); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	legacy, err = s.Get(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceMatchesIssue(legacy, legacy.Spec.SourceHash) || legacy.Spec.Digest != domain.SpecDigest(legacy.Spec) || legacy.Control != "PAUSING" || legacy.Review.Verdict != "OBSOLETE" {
		t.Fatal("migration must invalidate lossy legacy identity while preserving the accepted spec")
	}
	if err = s.Save(ctx, &preUpgrade); !errors.Is(err, store.ErrStale) {
		t.Fatalf("pre-upgrade worker can overwrite source provenance: %v", err)
	}
	migratedVersion := legacy.Version
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	legacy, err = s.Get(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Version != migratedVersion {
		t.Fatal("repeated startup reran legacy migration")
	}
	oldSpec := legacy.Spec.Digest
	oldGen := legacy.Generation
	if err = e.issueEdited(ctx, legacy, &gh.Issue{Title: "Use descending order", Body: legacy.IssueBody}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.Get(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed.IssueTitle != "Use descending order" || changed.Generation != oldGen+1 || changed.Control != "PAUSING" || changed.Review.Verdict != "OBSOLETE" || sourceMatchesIssue(changed, changed.Spec.SourceHash) || changed.Spec.Digest != oldSpec {
		t.Fatal("title edit failed to invalidate prior identity while retaining accepted spec")
	}
	// Both normal acceptance and recovery must reject the stale source.
	proposal := changed.Spec
	proposal.Approved = false
	proposal.Digest = domain.SpecDigest(proposal)
	if validateProposalSpec(&changed, &proposal) == nil {
		t.Fatal("stale title proposal was accepted")
	}
	if !analyzeAllowed(domain.Workflow{Phase: "AWAITING_MAINTAINER", Control: "PAUSED", IssueTitle: changed.IssueTitle, IssueBody: changed.IssueBody, LegacyIssueTitle: changed.LegacyIssueTitle, Spec: changed.Spec}) {
		t.Fatal("edited title should require analysis before recovery")
	}

	// Reconciliation (not only the webhook) also detects a title-only edit.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	mock := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/1/access_tokens":
			json.NewEncoder(rw).Encode(map[string]any{"token": "mock-token", "expires_at": time.Now().Add(time.Hour)})
		case "/repos/mock/repo/issues/3":
			json.NewEncoder(rw).Encode(gh.Issue{Number: 3, Title: "Use descending order", Body: "Sort input", State: "open"})
		default:
			t.Errorf("unexpected mock API %s", r.URL.Path)
			http.NotFound(rw, r)
		}
	}))
	defer mock.Close()
	cfg.GitHub.APIURL = mock.URL
	cfg.GitHub.AppID = 1
	cfg.GitHub.InstallationID = 1
	cfg.GitHub.PrivateKeyFile = keyPath
	re, err := New(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	remote := domain.Workflow{ID: domain.ID(991911, 3), RepositoryID: 991911, Repository: "mock/repo", InstallationID: 1, Issue: 3, IssueTitle: "Use ascending order", IssueBody: "Sort input", Phase: "SPEC_READY", Control: "HUMAN_OWNED", Review: domain.Review{Verdict: "PASS"}}
	if err = s.Create(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err = re.reconcileOne(ctx, remote); err != nil {
		t.Fatal(err)
	}
	reconciled, err := s.Get(ctx, remote.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.IssueTitle != "Use descending order" || reconciled.Generation != 2 || reconciled.Control != "HUMAN_OWNED" || reconciled.Review.Verdict != "OBSOLETE" {
		t.Fatalf("reconciliation missed title edit or lost ownership: phase=%s control=%s generation=%d verdict=%s", reconciled.Phase, reconciled.Control, reconciled.Generation, reconciled.Review.Verdict)
	}
}
