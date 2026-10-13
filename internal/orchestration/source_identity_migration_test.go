package orchestration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/store"
)

func TestSourceIdentityMigrationInvalidatesLegacyApprovalsAndRecovers(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	var before []domain.Workflow
	for i, tc := range []struct {
		hash, phase, control string
		approved             bool
	}{
		{normalizedIssueHash("body"), "AWAITING_MAINTAINER", "ACTIVE", true},
		{"issue-v2:" + domain.Digest([]string{"title", "body"}), "AWAITING_MAINTAINER", "ACTIVE", true},
		{"issue-v2:" + domain.Digest([]string{"title", "body"}), "WAITING_CONFIGURATION", "ACTIVE", false},
		{"issue-v2:" + domain.Digest([]string{"title", "body"}), "BLOCKED", "CANCELLED", true},
		{"issue-v2:" + domain.Digest([]string{"title", "body"}), "BLOCKED", "HUMAN_OWNED", true},
	} {
		w := domain.Workflow{ID: fmt.Sprintf("migration-%d", i), RepositoryID: 990111, Issue: i + 1, Repository: "local/pilot", IssueTitle: "title", IssueBody: "body", Phase: tc.phase, Control: tc.control, HeadSHA: "head", BaseSHA: "base", Spec: domain.Spec{Title: "title", Description: "body", SourceHash: tc.hash, Approved: tc.approved, Criteria: []domain.Criterion{{ID: "AC1", Description: "do work"}}}, Review: domain.Review{Key: "old-key", Verdict: "PASS"}, ForceApproval: &domain.ForceApproval{Kind: "pr_merge_gate"}}
		if tc.approved {
			w.PR = i + 1
		}
		w.Spec.Digest = domain.SpecDigest(w.Spec)
		if err := e.Store.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			return []store.Action{gateAction(*x, true, "old approval")}, nil
		}); err != nil {
			t.Fatal(err)
		}
		w, err := e.Store.Get(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, w)
	}
	current := domain.Workflow{ID: "migration-current", RepositoryID: 990111, Issue: 100, Repository: "local/pilot", IssueTitle: "title", IssueBody: "body", Phase: "AWAITING_MAINTAINER", Control: "ACTIVE", Spec: domain.Spec{SourceHash: normalizedIssueSourceHash("title", "body"), Approved: true}}
	if err := e.Store.Create(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB.Exec(ctx, "DELETE FROM schema_migrations WHERE version IN (2,3)"); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, old := range before {
		got, err := e.Store.Get(ctx, old.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantControl := old.Control
		if wantControl == "ACTIVE" {
			wantControl = "PAUSING"
		}
		if got.Generation != old.Generation+1 || got.Version <= old.Version || got.Control != wantControl || got.Spec.Digest != old.Spec.Digest || got.Spec.SourceHash != old.Spec.SourceHash || got.Review.Verdict != "OBSOLETE" || got.ForceApproval != nil || sourceMatchesIssue(got, got.Spec.SourceHash) {
			t.Fatalf("unsafe migration: old=%+v new=%+v", old, got)
		}
		var failureCount, passingPending int
		if err := e.Store.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='gate' AND data->>'pass'='false'),count(*) FILTER (WHERE kind='gate' AND data->>'pass'='true' AND state<>'obsolete') FROM outbox WHERE workflow_id=$1`, got.ID).Scan(&failureCount, &passingPending); err != nil || failureCount != 1 || passingPending != 0 {
			t.Fatalf("gate migration failure=%d passing=%d err=%v", failureCount, passingPending, err)
		}
		if err := e.Store.Save(ctx, &old); err != store.ErrStale {
			t.Fatalf("old worker not fenced: %v", err)
		}
		if err := e.Store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		again, err := e.Store.Get(ctx, got.ID)
		if err != nil || again.Version != got.Version || again.Generation != got.Generation {
			t.Fatalf("migration not idempotent: %v", err)
		}
	}
	unchanged, err := e.Store.Get(ctx, current.ID)
	if err != nil || unchanged.Generation != 1 || unchanged.Control != "ACTIVE" {
		t.Fatalf("v3 workflow changed: %+v %v", unchanged, err)
	}
	e.settleControls(ctx)
	for _, old := range before {
		got, err := e.Store.Get(ctx, old.ID)
		if err != nil {
			t.Fatal(err)
		}
		err = e.Command(ctx, got.ID, "maintainer", "/ai analyze", true)
		if old.Control == "ACTIVE" {
			if err != nil {
				t.Fatalf("migrated %s cannot follow recovery instructions: %v", old.Phase, err)
			}
			got, err = e.Store.Get(ctx, got.ID)
			if err != nil || got.Phase != "TRIAGE" || got.Control != "ACTIVE" || got.Spec.Digest != old.Spec.Digest {
				t.Fatalf("analysis recovery invalid: %+v %v", got, err)
			}
		} else if err == nil {
			t.Fatalf("migration bypassed %s control", old.Control)
		}
	}
}

func TestIssueWhitespaceEditRevokesExistingGate(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	body := "```python\nif enabled:\n    send()\nlog()\n```"
	w := domain.Workflow{ID: "indentation-edit", RepositoryID: 991234, Issue: 1, Repository: "local/pilot", IssueTitle: "Implement program", IssueBody: body, Control: "ACTIVE", Phase: "AWAITING_MAINTAINER", PR: 1, HeadSHA: "head", Spec: domain.Spec{Approved: true, SourceHash: normalizedIssueSourceHash("Implement program", body)}, Review: domain.Review{Verdict: "PASS"}}
	w.Spec.Digest = domain.SpecDigest(w.Spec)
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	w, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.issueEdited(ctx, w, &gh.Issue{Title: w.IssueTitle, Body: strings.ReplaceAll(body, "\nlog()", "\n    log()")}); err != nil {
		t.Fatal(err)
	}
	got, err := e.Store.Get(ctx, w.ID)
	if err != nil || got.Generation != w.Generation+1 || got.Control != "PAUSING" || got.Review.Verdict != "OBSOLETE" {
		t.Fatalf("semantic edit not invalidated: %+v %v", got, err)
	}
}
