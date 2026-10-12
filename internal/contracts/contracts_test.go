package contracts

import (
	"reflect"
	"testing"
	"time"
)

func validKey() VerificationKey {
	return VerificationKey{1, 2, "spec", 1, "base", "head", "policy", "profile", "image"}
}
func validEvidence(k VerificationKey) Evidence {
	return Evidence{Key: k, TestedCommit: k.HeadSHA, MechanicalPassed: true, ProvenanceValid: true, Coverage: []Obligation{{"AC-01", "satisfied", "runner-exit-log"}, {"SEC-01", "satisfied", "sandbox-evidence"}}}
}
func TestEveryIdentityDimensionInvalidatesPASS(t *testing.T) {
	key := validKey()
	value := reflect.ValueOf(&key).Elem()
	for i := 0; i < value.NumField(); i++ {
		t.Run(value.Type().Field(i).Name, func(t *testing.T) {
			changed := key
			field := reflect.ValueOf(&changed).Elem().Field(i)
			if field.Kind() == reflect.String {
				field.SetString(field.String() + "-changed")
			} else {
				field.SetInt(field.Int() + 1)
			}
			v, _ := Gate(changed, Active, validEvidence(key), []string{"AC-01", "SEC-01"})
			if v != Obsolete {
				t.Fatalf("got %s", v)
			}
		})
	}
}
func TestFailClosedGateAndGitHubConclusions(t *testing.T) {
	key := validKey()
	cases := []struct {
		name     string
		mutate   func(*Evidence)
		control  Control
		required []string
		want     Verdict
	}{
		{"complete", func(e *Evidence) {}, Active, []string{"AC-01", "SEC-01"}, Pass},
		{"timeout with no findings", func(e *Evidence) { e.MechanicalPassed = false }, Active, []string{"AC-01"}, Fail},
		{"pending", func(e *Evidence) { e.Pending = true }, Active, []string{"AC-01"}, Pending},
		{"unassigned obligation", func(e *Evidence) {}, Active, []string{"AC-02"}, Fail},
		{"unverified", func(e *Evidence) { e.Coverage[0].Result = "unverified" }, Active, []string{"AC-01"}, Fail},
		{"no evidence", func(e *Evidence) { e.Coverage[0].Evidence = "" }, Active, []string{"AC-01"}, Fail},
		{"no provenance", func(e *Evidence) { e.ProvenanceValid = false }, Active, []string{"AC-01"}, Fail},
		{"blocking finding", func(e *Evidence) { e.BlockingFindings = 1 }, Active, []string{"AC-01"}, Fail},
		{"duplicate coverage", func(e *Evidence) { e.Coverage = append(e.Coverage, e.Coverage[0]) }, Active, []string{"AC-01"}, Fail},
		{"paused", func(e *Evidence) {}, Paused, []string{"AC-01"}, Fail},
		{"wrong tested commit", func(e *Evidence) { e.TestedCommit = "different" }, Active, []string{"AC-01"}, Obsolete},
		{"empty obligations", func(e *Evidence) {}, Active, nil, Fail},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			e := validEvidence(key)
			tt.mutate(&e)
			v, _ := Gate(key, tt.control, e, tt.required)
			if v != tt.want {
				t.Fatalf("want %s got %s", tt.want, v)
			}
			check := CheckFor(v)
			if (check.Conclusion == "success") != (v == Pass) {
				t.Fatal("invalid success projection")
			}
			if check.Conclusion == "neutral" || check.Conclusion == "skipped" || check.Conclusion == "stale" {
				t.Fatal("illegal/unblocking conclusion")
			}
		})
	}
}
func TestExpiredAndReassignedAttemptsCannotAdvance(t *testing.T) {
	now := time.Now()
	a := Attempt{ID: "a", Generation: 1, Epoch: 1, ExpiresAt: now.Add(time.Minute), Status: "RUNNING"}
	w := Workflow{Generation: 1, Control: Active, Attempt: a}
	if w.Accept(a, now) != nil {
		t.Fatal("valid result rejected")
	}
	for _, mutate := range []func(*Workflow){func(w *Workflow) { w.Generation++ }, func(w *Workflow) { w.Attempt.Epoch++ }, func(w *Workflow) { w.Control = Pausing }, func(w *Workflow) { w.Attempt.ExpiresAt = now }, func(w *Workflow) { w.Attempt.Status = "OUTPUT_READY" }} {
		changed := w
		mutate(&changed)
		if changed.Accept(a, now) == nil {
			t.Fatal("stale result accepted")
		}
	}
}
