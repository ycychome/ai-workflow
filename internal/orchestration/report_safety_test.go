package orchestration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
)

func reportSafetyFixture() (domain.Workflow, reviewReport, []domain.Evidence) {
	ev := domain.Evidence{ID: "original", Kind: "SOURCE_ANALYSIS", HeadSHA: "head", BaseSHA: "base"}
	f := domain.Finding{Severity: "high", State: "CONFIRMED", EvidenceType: "SOURCE_ANALYSIS", Description: "wrong result", Expected: "correct", Actual: "incorrect", Preconditions: "boundary input", Path: "a.go", Line: 1, Evidence: []string{ev.ID}}
	f.ID = "F-" + domain.Digest([]any{f.Path, f.Line, f.Description})[:10]
	w := domain.Workflow{Control: "ACTIVE", HeadSHA: "head", BaseSHA: "base", Spec: domain.Spec{Approved: true, Criteria: []domain.Criterion{{ID: "AC1"}}}, Evidence: []domain.Evidence{ev}, Review: domain.Review{Key: "key", VerificationPassed: true, SkepticComplete: true, Obligations: []domain.Obligation{{ID: "O1", CriterionID: "AC1", Result: "satisfied", Evidence: []string{ev.ID}}}, Findings: []domain.Finding{f}}}
	return w, reviewReport{Obligations: append([]domain.Obligation(nil), w.Review.Obligations...), Findings: []domain.Finding{f}}, []domain.Evidence{ev}
}
func TestMergeReportRejectsInvalidEnums(t *testing.T) {
	for _, edit := range []func(*domain.Finding){func(f *domain.Finding) { f.Severity = "HIGH" }, func(f *domain.Finding) { f.State = "FIX_CANDIDATE" }, func(f *domain.Finding) { f.State = "" }, func(f *domain.Finding) { f.EvidenceType = "UNKNOWN" }} {
		w, r, ev := reportSafetyFixture()
		edit(&r.Findings[0])
		if err := mergeReport(&w, r, []string{"O1"}, ev, false); err == nil {
			t.Fatalf("invalid finding accepted: %+v", r.Findings[0])
		}
	}
}
func TestFindingDowngradeRequiresNewIndependentSuccessfulProof(t *testing.T) {
	for _, tc := range []struct {
		name         string
		skeptic      bool
		kind         string
		exit         int
		reuse, valid bool
	}{
		{name: "claim", skeptic: true, kind: "MODEL_CLAIM"}, {name: "original proof reused", skeptic: true, kind: "SOURCE_ANALYSIS", reuse: true}, {name: "timed out", skeptic: true, kind: "TOOL_VERIFIED", exit: 124}, {name: "infrastructure failure", skeptic: true, kind: "TOOL_VERIFIED", exit: -1}, {name: "parallel reviewer collision", kind: "SOURCE_ANALYSIS"}, {name: "independent source", skeptic: true, kind: "SOURCE_ANALYSIS", valid: true}, {name: "independent passing tool", skeptic: true, kind: "TOOL_VERIFIED", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, r, ev := reportSafetyFixture()
			id := "counter"
			if tc.reuse {
				id = "original"
			}
			counter := domain.Evidence{ID: id, Kind: tc.kind, ExitCode: tc.exit, HeadSHA: w.HeadSHA, BaseSHA: w.BaseSHA}
			ev = append(ev, counter)
			r.Findings[0].Severity = "low"
			r.Findings[0].State = "PROPOSED"
			r.Findings[0].EvidenceType = tc.kind
			r.Findings[0].Evidence = []string{id}
			err := mergeReport(&w, r, []string{"O1"}, ev, tc.skeptic)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if pass, _ := domain.Gate(w, "key"); pass != tc.valid {
				t.Fatalf("gate pass=%v valid=%v", pass, tc.valid)
			}
		})
	}
}
func TestFailedToolsCannotProveSatisfiedOrResolved(t *testing.T) {
	for _, exit := range []int{-1, 1, 124} {
		for _, result := range []string{"satisfied", "failed", "unverified"} {
			w, r, _ := reportSafetyFixture()
			ev := domain.Evidence{ID: "failed", Kind: "TOOL_VERIFIED", ExitCode: exit, HeadSHA: w.HeadSHA, BaseSHA: w.BaseSHA}
			r.Obligations[0].Result = result
			r.Obligations[0].Evidence = []string{ev.ID}
			r.Findings = nil
			err := mergeReport(&w, r, []string{"O1"}, []domain.Evidence{ev}, false)
			if (err == nil) != (result != "satisfied") {
				t.Fatalf("exit=%d result=%s err=%v", exit, result, err)
			}
		}
		for _, state := range []string{"REFUTED", "RESOLVED"} {
			w, r, ev := reportSafetyFixture()
			failed := domain.Evidence{ID: "failed", Kind: "TOOL_VERIFIED", ExitCode: exit, HeadSHA: w.HeadSHA, BaseSHA: w.BaseSHA}
			ev = append(ev, failed)
			r.Findings[0].State = state
			r.Findings[0].EvidenceType = "TOOL_VERIFIED"
			r.Findings[0].Evidence = []string{failed.ID}
			if err := mergeReport(&w, r, []string{"O1"}, ev, true); err == nil {
				t.Fatalf("exit=%d allowed %s", exit, state)
			}
		}
	}
}
func TestEvidenceEncodingFailsBeforeWritingArtifact(t *testing.T) {
	e := &Engine{cfg: config.Config{DataDir: t.TempDir()}}
	for _, data := range []any{json.RawMessage(`{"broken":`), make(chan int)} {
		if _, err := e.artifact(context.Background(), domain.Workflow{ID: "fixture"}, "invalid", "SOURCE_ANALYSIS", data); err == nil {
			t.Fatal("bad evidence encoding succeeded")
		}
		if output, err := encodeToolOutput(data, nil); err == nil || output != "" {
			t.Fatalf("bad output encoding succeeded: %q %v", output, err)
		}
	}
	if _, err := os.Stat(e.Config().DataDir + "/evidence"); !os.IsNotExist(err) {
		t.Fatalf("failed encoding created evidence artifacts: %v", err)
	}
}
func TestIssueSourceHashPreservesLiteralWhitespace(t *testing.T) {
	for _, pair := range [][2]string{{`print("a b")`, `print("a  b")`}, {"if enabled:\n    send()\nlog()", "if enabled:\n    send()\n    log()"}, {"a\tb", "a b"}, {"a\nb", "a b"}} {
		if normalizedIssueSourceHash("requirement", pair[0]) == normalizedIssueSourceHash("requirement", pair[1]) {
			t.Fatalf("semantic whitespace collapsed: %q / %q", pair[0], pair[1])
		}
	}
	body := "a\nb\n"
	if normalizedIssueSourceHash("requirement", body) != normalizedIssueSourceHash("requirement", strings.ReplaceAll(body, "\n", "\r\n")) {
		t.Fatal("line-ending normalization differs")
	}
}

func TestSuccessfulEvidenceSupportsSatisfiedAndResolution(t *testing.T) {
	for _, kind := range []string{"SOURCE_ANALYSIS", "TOOL_VERIFIED"} {
		w, r, _ := reportSafetyFixture()
		ev := domain.Evidence{ID: "independent", Kind: kind, HeadSHA: w.HeadSHA, BaseSHA: w.BaseSHA}
		r.Obligations[0].Evidence = []string{ev.ID}
		r.Findings[0].State = "RESOLVED"
		r.Findings[0].EvidenceType = kind
		r.Findings[0].Evidence = []string{ev.ID}
		if err := mergeReport(&w, r, []string{"O1"}, []domain.Evidence{ev}, true); err != nil {
			t.Fatalf("valid %s resolution rejected: %v", kind, err)
		}
		if pass, reason := domain.Gate(w, "key"); !pass {
			t.Fatalf("valid %s resolution blocked: %s", kind, reason)
		}
	}
}
