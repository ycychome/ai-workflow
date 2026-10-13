package domain

import "testing"

func TestGateRejectsMalformedFindingEnums(t *testing.T) {
	for _, tc := range []struct {
		name    string
		finding Finding
		pass    bool
	}{
		{"unknown severity", Finding{Severity: "HIGH", State: "CONFIRMED", EvidenceType: "SOURCE_ANALYSIS"}, false},
		{"unknown state", Finding{Severity: "low", State: "confirmed", EvidenceType: "SOURCE_ANALYSIS"}, false},
		{"unknown evidence kind", Finding{Severity: "low", State: "CONFIRMED", EvidenceType: "source_analysis"}, false},
		{"internal pending fix", Finding{Severity: "high", State: "FIX_CANDIDATE", EvidenceType: "SOURCE_ANALYSIS"}, false},
		{"valid low", Finding{Severity: "low", State: "CONFIRMED", EvidenceType: "SOURCE_ANALYSIS"}, true},
		{"valid refuted high", Finding{Severity: "high", State: "REFUTED", EvidenceType: "SOURCE_ANALYSIS"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := Workflow{Control: "ACTIVE", Spec: Spec{Approved: true, Criteria: []Criterion{{ID: "AC1"}}}, Review: Review{Key: "key", VerificationPassed: true, SkepticComplete: true, Obligations: []Obligation{{ID: "O1", CriterionID: "AC1", Result: "satisfied", Evidence: []string{"E1"}}}, Findings: []Finding{tc.finding}}}
			if pass, reason := Gate(w, "key"); pass != tc.pass {
				t.Fatalf("pass=%v reason=%s", pass, reason)
			}
		})
	}
}
