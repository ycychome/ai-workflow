package orchestration

import (
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func acceptedTriageReport() triageReport {
	return triageReport{
		Decision: "accepted", Title: "Repair Add", Description: "Make Add return the arithmetic sum.",
		Criteria: []domain.Criterion{{ID: "AC-1", Description: "Add(2, 3) returns 5"}},
	}
}

func TestApplyTriageReportApprovalModes(t *testing.T) {
	for _, requireAccept := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic approval", true: "maintainer approval"}[requireAccept], func(t *testing.T) {
			old := domain.Spec{Title: "old", Digest: "old-digest", Approved: true, ApprovedBy: "maintainer"}
			w := domain.Workflow{IssueBody: "Fix addition", Phase: "TRIAGE", Spec: old}
			if err := applyTriageReport(&w, acceptedTriageReport(), requireAccept); err != nil {
				t.Fatal(err)
			}
			if requireAccept {
				if w.Phase != "SPEC_READY" || w.ProposalSpec == nil || w.ProposalSpec.Approved || w.Spec.Digest != old.Digest {
					t.Fatalf("maintainer approval mode changed accepted state: %+v", w)
				}
				if !strings.Contains(w.LastError, "/ai accept") {
					t.Fatalf("SPEC_READY must explain explicit acceptance: %q", w.LastError)
				}
				return
			}
			if w.Phase != "PLANNING" || w.ProposalSpec != nil || !w.Spec.Approved || w.Spec.ApprovedBy != "ai:dev-triage" {
				t.Fatalf("automatic approval did not enter development: %+v", w)
			}
			if w.Spec.SourceHash != normalizedIssueSourceHash(w.IssueTitle, w.IssueBody) || w.Spec.Digest != domain.SpecDigest(w.Spec) || w.LastError != "" {
				t.Fatalf("approved specification identity is incomplete: %+v", w.Spec)
			}
		})
	}
}

func TestApplyTriageNonAcceptedDecisionsStayUnapproved(t *testing.T) {
	for _, decision := range []string{"needs_info", "rejected", "deferred"} {
		t.Run(decision, func(t *testing.T) {
			w := domain.Workflow{IssueBody: "request", Phase: "TRIAGE", Spec: domain.Spec{Title: "previous", Approved: true, ApprovedBy: "human"}}
			report := triageReport{Decision: decision, Questions: []string{"Which behavior?"}}
			if err := applyTriageReport(&w, report, false); err != nil {
				t.Fatal(err)
			}
			if w.Spec.Title != "previous" || !w.Spec.Approved || w.ProposalSpec != nil {
				t.Fatalf("non-accepted decision changed accepted spec: %+v", w)
			}
			if w.Phase == "PLANNING" {
				t.Fatal("non-accepted decision must never enter development")
			}
			if decision == "needs_info" && !strings.Contains(w.LastError, "Which behavior?") {
				t.Fatalf("clarifying question was lost: %q", w.LastError)
			}
		})
	}
}

func TestApplyTriageRejectsInvalidAcceptedReport(t *testing.T) {
	for _, report := range []triageReport{
		{Decision: "accepted", Title: "", Description: "desc", Criteria: []domain.Criterion{{ID: "AC1", Description: "done"}}},
		{Decision: "accepted", Title: "title", Description: "", Criteria: []domain.Criterion{{ID: "AC1", Description: "done"}}},
		{Decision: "accepted", Title: "title", Description: "desc"},
		{Decision: "accepted", Title: "title", Description: "desc", Criteria: []domain.Criterion{{ID: "AC1", Description: "done"}, {ID: "AC1", Description: "duplicate"}}},
	} {
		w := domain.Workflow{IssueBody: "request", Phase: "TRIAGE", Spec: domain.Spec{Approved: false}}
		if err := applyTriageReport(&w, report, false); err == nil {
			t.Fatalf("invalid accepted report unexpectedly passed: %+v", report)
		}
		if w.Phase != "TRIAGE" || w.ProposalSpec != nil || w.Spec.Approved {
			t.Fatalf("invalid report mutated workflow approval state: %+v", w)
		}
	}
}
