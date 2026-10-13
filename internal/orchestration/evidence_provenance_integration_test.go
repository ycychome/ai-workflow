package orchestration

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
)

func TestReadingEvidencePreservesOriginalTrustAndCannotCreateCounterevidence(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	for i, tc := range []struct {
		kind       string
		exit       int
		canSatisfy bool
	}{
		{"TOOL_VERIFIED", 124, false}, {"TOOL_VERIFIED", -1, false}, {"TOOL_VERIFIED", 0, true}, {"SOURCE_ANALYSIS", 0, true},
	} {
		t.Run(fmt.Sprintf("%s-%d", tc.kind, tc.exit), func(t *testing.T) {
			w, r, _ := reportSafetyFixture()
			w.ID = fmt.Sprintf("proof-read-%d", i)
			w.RepositoryID = 998766
			w.Issue = i + 1
			w.Repository = "local/pilot"
			w.Phase = "REVIEWING"
			if err := e.Store.Create(ctx, w); err != nil {
				t.Fatal(err)
			}
			stored, err := e.Store.Get(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			w.Generation = stored.Generation
			original, err := e.artifact(ctx, w, "original", tc.kind, sandbox.RunResult{ExitCode: tc.exit, TimedOut: tc.exit == 124})
			if err != nil {
				t.Fatal(err)
			}
			original.ExitCode = tc.exit
			w.Evidence = []domain.Evidence{original}
			session := toolSession{e: e, w: w, workspace: &sandbox.Workspace{ID: "proof-reader"}, review: true, readableEvidence: []domain.Evidence{original}}
			output, err := session.handler(ctx, "read_evidence", raw(map[string]string{"evidence_id": original.ID}))
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				EvidenceID string `json:"evidence_id"`
				Kind       string `json:"kind"`
				Exit       int    `json:"exit_code"`
			}
			if err := json.Unmarshal([]byte(output), &body); err != nil {
				t.Fatal(err)
			}
			if body.EvidenceID != original.ID || body.Kind != original.Kind || body.Exit != original.ExitCode || len(session.listEvidence()) != 0 {
				t.Fatalf("read created new trust or provenance: %+v new=%+v", body, session.listEvidence())
			}
			r.Obligations[0].Evidence = []string{body.EvidenceID}
			r.Findings = nil
			if err := mergeReport(&w, r, []string{"O1"}, session.listEvidence(), true); (err == nil) != tc.canSatisfy {
				t.Fatalf("original trust changed after read: canSatisfy=%v err=%v", tc.canSatisfy, err)
			}
			// Leave the obligation unverified so any failure specifically exercises
			// the attempt to turn a read into independent refutation/downgrade.
			for _, mode := range []string{"refute", "downgrade"} {
				r.Obligations[0].Result = "unverified"
				finding := w.Review.Findings[0]
				finding.EvidenceType = "SOURCE_ANALYSIS"
				finding.Evidence = []string{body.EvidenceID}
				if mode == "refute" {
					finding.State = "REFUTED"
				} else {
					finding.Severity = "low"
				}
				r.Findings = []domain.Finding{finding}
				if err := mergeReport(&w, r, []string{"O1"}, session.listEvidence(), true); err == nil {
					t.Fatalf("reading original %s proof allowed independent %s", tc.kind, mode)
				}
			}
			var reads int
			if err := e.Store.DB.QueryRow(ctx, `SELECT count(*) FROM audit WHERE workflow_id=$1 AND kind='evidence_read' AND data->>'evidence_id'=$2`, w.ID, original.ID).Scan(&reads); err != nil || reads != 1 {
				t.Fatalf("read not audited: %d %v", reads, err)
			}
		})
	}
}
