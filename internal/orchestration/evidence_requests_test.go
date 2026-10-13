package orchestration

import (
	"testing"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func coordinationFixture() domain.Workflow {
	return domain.Workflow{ID: "wf", HeadSHA: "head", BaseSHA: "base", Review: domain.Review{Key: "key", Findings: []domain.Finding{{ID: "F-CaseSensitive", State: "CONFIRMED", VerificationKey: "key"}}}, FindingResolutions: []domain.FindingResolution{{ID: "resolution", FindingID: "F-CaseSensitive", TaskID: "builder-task", Kind: "fix", VerificationKey: "key", State: "PROPOSED"}}}
}
func TestWaiverRequiresExplicitCurrentStateAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	w := coordinationFixture()
	waiver := domain.Waiver{FindingID: "F-CaseSensitive", Actor: "maintainer", Reason: "已确认限定场景风险并安排后续修复", VerificationKey: "key", ExpiresAt: now.Add(time.Hour)}
	if err := applyFindingWaiver(&w, waiver, now); err != nil {
		t.Fatal(err)
	}
	f := w.Review.Findings[0]
	if !CurrentFindingWaiver(w, f, "key", now) {
		t.Fatal("explicit current waiver rejected")
	}
	if CurrentFindingWaiver(w, f, "other-key", now) || CurrentFindingWaiver(w, f, "key", now.Add(time.Hour)) {
		t.Fatal("stale or expired waiver accepted")
	}
	f.State = "CONFIRMED"
	if CurrentFindingWaiver(w, f, "key", now) {
		t.Fatal("record alone authorized non-waived finding")
	}
}
func TestWaiverRejectsMissingReasonAndWrongFindingKey(t *testing.T) {
	now := time.Now().UTC()
	for _, change := range []func(*domain.Waiver){func(v *domain.Waiver) { v.Reason = "" }, func(v *domain.Waiver) { v.Actor = "" }, func(v *domain.Waiver) { v.VerificationKey = "old" }, func(v *domain.Waiver) { v.ExpiresAt = now }, func(v *domain.Waiver) { v.FindingID = "missing" }} {
		w := coordinationFixture()
		v := domain.Waiver{FindingID: "F-CaseSensitive", Actor: "maintainer", Reason: "具体理由", VerificationKey: "key", ExpiresAt: now.Add(time.Hour)}
		change(&v)
		if err := applyFindingWaiver(&w, v, now); err == nil {
			t.Fatal("invalid waiver accepted")
		}
		if len(w.Waivers) != 0 || w.Review.Findings[0].State != "CONFIRMED" {
			t.Fatal("invalid waiver changed finding")
		}
	}
}
func TestBuilderCannotCloseOwnFinding(t *testing.T) {
	proof := []domain.Evidence{{ID: "independent-proof", Kind: "TOOL_VERIFIED", ExitCode: 0}}
	for _, run := range []domain.AgentRun{{ID: "builder", TaskID: "builder-task", Role: "builder"}, {ID: "reviewer", TaskID: "builder-task", Role: "reviewer"}} {
		w := coordinationFixture()
		if err := applyFindingResolution(&w, "key", "resolution", run, "accepted", proof); err == nil {
			t.Fatal("non-independent resolution accepted")
		}
		if w.Review.Findings[0].State != "CONFIRMED" {
			t.Fatal("builder claim closed finding")
		}
	}
	w := coordinationFixture()
	if err := applyFindingResolution(&w, "key", "resolution", domain.AgentRun{ID: "reviewer", TaskID: "review-task", Role: "reviewer"}, "accepted", proof); err != nil {
		t.Fatal(err)
	}
	if w.Review.Findings[0].State != "RESOLVED" || w.FindingResolutions[0].VerifiedBy != "reviewer" {
		t.Fatal("independent resolution was not recorded")
	}
}
func TestResolutionRejectsFailedOrMissingProof(t *testing.T) {
	for _, proof := range [][]domain.Evidence{nil, {{ID: "failed", Kind: "TOOL_VERIFIED", ExitCode: 1}}} {
		w := coordinationFixture()
		if err := applyFindingResolution(&w, "key", "resolution", domain.AgentRun{ID: "skeptic", TaskID: "review-task", Role: "skeptic"}, "accepted", proof); err == nil {
			t.Fatal("unverified repair accepted")
		}
	}
}
func TestEvidenceIntentPreservesFindingAndTargetRole(t *testing.T) {
	request := domain.EvidenceRequest{ID: "req", TaskID: "evidence-task", TargetRole: "reviewer", FindingID: "F-CaseSensitive", Question: "检查边界输入是否触发错误", VerificationKey: "key", Attempt: 2}
	intent := evidenceIntent(request)
	if intent.Task.Role != "reviewer" || intent.Task.Kind != "verification" || len(intent.Task.FindingIDs) != 1 || intent.Task.FindingIDs[0] != request.FindingID || intent.Task.Attempts != 2 {
		t.Fatalf("intent lost request identity: %+v", intent)
	}
}
func TestEvidenceIdentityCannotBeReplaced(t *testing.T) {
	w := coordinationFixture()
	w.Evidence = []domain.Evidence{{ID: "proof", Hash: "original"}}
	if _, err := appendCoordinationProof(&w, []domain.Evidence{{ID: "proof", Hash: "replacement"}}); err == nil {
		t.Fatal("evidence ID overwritten")
	}
}
