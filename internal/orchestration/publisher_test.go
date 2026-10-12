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
