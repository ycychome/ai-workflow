package domain

import (
	"strings"
	"testing"
	"time"
)

func specChangeFixture() Workflow {
	w := Workflow{ID: "workflow", IssueTitle: "实现排序", IssueBody: "保留重复项\n    精确保留缩进", Generation: 1, Control: "ACTIVE"}
	w.Spec = Spec{Revision: 1, Title: "旧规格", Description: "保留重复项", Criteria: []Criterion{{ID: "AC-1", Description: "排序正确"}}, SourceHash: IssueSourceHash(w.IssueTitle, w.IssueBody), Approved: true, ApprovedBy: "maintainer"}
	w.Spec.Digest = SpecDigest(w.Spec)
	return w
}
func specChangeCandidate(w Workflow) Spec {
	return Spec{Revision: w.Spec.Revision + 1, Title: "新规格", Description: "升序排序且保持重复项", Criteria: []Criterion{{ID: "AC-1", Description: "升序且不去重"}}, SourceHash: IssueSourceHash(w.IssueTitle, w.IssueBody)}
}
func TestSpecProposalDoesNotRewriteApprovedSpec(t *testing.T) {
	w := specChangeFixture()
	old := Digest(w.Spec)
	now := time.Now().UTC()
	candidate := specChangeCandidate(w)
	proposal, err := ProposeSpecChange(&w, candidate, "ai:triage", now)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Criteria[0].Description = "外部修改"
	if Digest(w.Spec) != old || w.SpecChanges[0].Spec.Criteria[0].Description == "外部修改" {
		t.Fatal("proposal aliased or replaced approved specification")
	}
	if err := DecideSpecChange(&w, proposal.ID, proposal.PreviousApprovedDigest, "accepted", "maintainer", "确认该候选规格", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !w.Spec.Approved || w.Spec.ApprovedBy != "maintainer" || len(w.SpecHistory) != 2 || w.SpecHistory[0].Spec.Title != "旧规格" || w.SpecChanges[0].Spec.Approved {
		t.Fatal("immutable history or approval identity lost")
	}
}
func TestSpecDecisionCASAndTamperRejection(t *testing.T) {
	for _, mutation := range []func(*Workflow){func(w *Workflow) { w.Spec.Digest = "new-approved-digest" }, func(w *Workflow) { w.SpecChanges[0].Spec.Description = "被修改" }, func(w *Workflow) { w.SpecChanges[0].PreviousApprovedDigest = "changed" }, func(w *Workflow) { w.IssueBody += "\n新增要求" }} {
		w := specChangeFixture()
		now := time.Now().UTC()
		p, err := ProposeSpecChange(&w, specChangeCandidate(w), "planner", now)
		if err != nil {
			t.Fatal(err)
		}
		mutation(&w)
		if err := DecideSpecChange(&w, p.ID, p.PreviousApprovedDigest, "accepted", "maintainer", "确认", now.Add(time.Second)); err == nil {
			t.Fatal("changed proposal or approved history accepted")
		}
	}
}
func TestRejectedProposalCannotBeAcceptedLater(t *testing.T) {
	w := specChangeFixture()
	now := time.Now().UTC()
	old := w.Spec.Digest
	p, err := ProposeSpecChange(&w, specChangeCandidate(w), "planner", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := DecideSpecChange(&w, p.ID, old, "rejected", "maintainer", "不满足需求", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := DecideSpecChange(&w, p.ID, old, "accepted", "maintainer", "改为通过", now.Add(time.Second*2)); err == nil {
		t.Fatal("immutable decision overwritten")
	}
	if w.Spec.Digest != old || len(w.SpecChanges[0].Decisions) != 1 {
		t.Fatal("rejected decision changed approved spec")
	}
}
func TestSourceIdentityPreservesSemanticWhitespace(t *testing.T) {
	w := specChangeFixture()
	old := w.Spec.Digest
	if IssueSourceHash("题目", "    code\n") != IssueSourceHash("题目", "    code\r\n") {
		t.Fatal("transport line endings changed identity")
	}
	if IssueSourceHash("题目", "    code\n") == IssueSourceHash("题目", "  code\n") {
		t.Fatal("indentation collapsed")
	}
	changed, err := ApplyIssueSourceEdit(&w, w.IssueTitle, strings.ReplaceAll(w.IssueBody, "\n", "\r\n"))
	if err != nil || changed || w.Generation != 1 || w.Control != "ACTIVE" {
		t.Fatal("exact normalized edit revoked execution")
	}
	changed, err = ApplyIssueSourceEdit(&w, w.IssueTitle, w.IssueBody+" ")
	if err != nil || !changed || w.Generation != 2 || w.Control != "PAUSING" || w.Spec.Digest != old || w.Review.Verdict != "OBSOLETE" {
		t.Fatal("uncertain edit did not preserve approved spec and revoke old generation")
	}
}
