package orchestration

import (
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestSkepticCheckpointRequiresMatchingCompletedReviewers(t *testing.T) {
	base := domain.Workflow{
		ID: "repo-1", RepositoryID: 12, PR: 34, Phase: "BLOCKED", Control: "ACTIVE",
		Generation: 5, Version: 19, BaseSHA: "base-sha", HeadSHA: "head-sha",
		Spec: domain.Spec{Digest: "spec-digest", Approved: true},
		Review: domain.Review{ID: "review-id", Key: "review-key", Verdict: "PENDING", VerificationPassed: true,
			Tasks: []domain.Task{{ID: "review-1", Status: "ACCEPTED"}, {ID: "review-2", Status: "ACCEPTED"}}},
	}
	if !skepticCheckpointReady(base, "review-key") {
		t.Fatal("expected matching accepted-review checkpoint to be resumable")
	}
	if !sameSkepticCheckpoint(base, base, "review-key") {
		t.Fatal("expected unchanged checkpoint snapshot to match")
	}

	mutations := []struct {
		name string
		edit func(*domain.Workflow)
	}{
		{"different key", func(w *domain.Workflow) { w.Review.Key = "old-key" }},
		{"verification incomplete", func(w *domain.Workflow) { w.Review.VerificationPassed = false }},
		{"skeptic complete", func(w *domain.Workflow) { w.Review.SkepticComplete = true }},
		{"no reviewer tasks", func(w *domain.Workflow) { w.Review.Tasks = nil }},
		{"reviewer not accepted", func(w *domain.Workflow) { w.Review.Tasks[1].Status = "BLOCKED" }},
		{"obsolete verdict", func(w *domain.Workflow) { w.Review.Verdict = "OBSOLETE" }},
		{"phase changed", func(w *domain.Workflow) { w.Phase = "VERIFYING" }},
		{"spec not approved", func(w *domain.Workflow) { w.Spec.Approved = false }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			candidate.Review.Tasks = append([]domain.Task(nil), base.Review.Tasks...)
			tc.edit(&candidate)
			if skepticCheckpointReady(candidate, "review-key") {
				t.Fatal("checkpoint unexpectedly passed resume guard")
			}
		})
	}

	changed := base
	changed.HeadSHA = "different-head"
	if sameSkepticCheckpoint(changed, base, "review-key") {
		t.Fatal("changed candidate head must invalidate the checkpoint")
	}
}

func TestManualForceApprovalIsBoundToExactPRIdentity(t *testing.T) {
	w := domain.Workflow{PR: 21, Phase: "AWAITING_MAINTAINER", Control: "ACTIVE", Generation: 7, BaseSHA: "base", HeadSHA: "head", Spec: domain.Spec{Digest: "spec"}, Review: domain.Review{Key: "key"}}
	a := &domain.ForceApproval{Kind: "pr_merge_gate", Generation: 7, SpecDigest: "spec", PolicyDigest: "policy", VerificationKey: "key", BaseSHA: "base", HeadSHA: "head"}
	if !manualForceIdentity(w, a, "key", "policy") {
		t.Fatal("matching manual override should be current")
	}
	verified := w
	verified.Spec.Approved = true
	verified.Review.Verdict = "FORCED"
	verified.Review.VerificationPassed = true
	verified.Review.SkepticComplete = true
	verified.Review.Obligations = []domain.Obligation{{ID: "O", Result: "satisfied", Evidence: []string{"actual-prior-proof"}}}
	if pass, _ := domain.Gate(verified, "key"); pass {
		t.Fatal("forced approval must remain separate from an automatic pass even when prior verification succeeded")
	}
	mutations := []struct {
		name string
		edit func(*domain.Workflow, *domain.ForceApproval)
	}{
		{"generation", func(w *domain.Workflow, _ *domain.ForceApproval) { w.Generation++ }},
		{"head", func(w *domain.Workflow, _ *domain.ForceApproval) { w.HeadSHA = "new-head" }},
		{"base", func(w *domain.Workflow, _ *domain.ForceApproval) { w.BaseSHA = "new-base" }},
		{"spec", func(w *domain.Workflow, _ *domain.ForceApproval) { w.Spec.Digest = "new-spec" }},
		{"policy", func(_ *domain.Workflow, a *domain.ForceApproval) { a.PolicyDigest = "new-policy" }},
		{"wrong phase", func(w *domain.Workflow, _ *domain.ForceApproval) { w.Phase = "REVIEWING" }},
		{"human owned", func(w *domain.Workflow, _ *domain.ForceApproval) { w.Control = "HUMAN_OWNED" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			candidate, approval := w, *a
			tc.edit(&candidate, &approval)
			if manualForceIdentity(candidate, &approval, "key", "policy") {
				t.Fatal("changed override identity/control should be stale")
			}
		})
	}
}

func TestAcceptNeedsValidProposalAndAnalyzeIsNotAcceptance(t *testing.T) {
	w := domain.Workflow{Phase: "SPEC_READY", Control: "ACTIVE", IssueBody: "request"}
	p := &domain.Spec{Title: "title", Description: "description", SourceHash: normalizedIssueHash(w.IssueBody), Criteria: []domain.Criterion{{ID: "AC1", Description: "done"}}}
	p.Digest = domain.SpecDigest(*p)
	if err := validateProposalSpec(&w, p); err != nil {
		t.Fatalf("valid proposal rejected: %v", err)
	}
	w.IssueBody = "edited request"
	if err := validateProposalSpec(&w, p); err == nil {
		t.Fatal("proposal for an old Issue body must not be accepted")
	}
	if analyzeAllowed(domain.Workflow{Phase: "INTAKE", Control: "ACTIVE"}) != true {
		t.Fatal("explicit analyze should start INTAKE triage")
	}
	if forceIssueStage(domain.Workflow{Phase: "INTAKE", Control: "HUMAN_OWNED"}) {
		t.Fatal("forceaccept must not override human-owned control")
	}
	if !forceIssueStage(domain.Workflow{Phase: "NEEDS_INFO", Control: "CANCELLED"}) {
		t.Fatal("explicit maintainer forceaccept should be able to restore a settled cancellation")
	}
	if forceIssueStage(domain.Workflow{Phase: "NEEDS_INFO", Control: "CANCELLING"}) {
		t.Fatal("forceaccept must wait for an in-progress cancellation to settle")
	}
}
