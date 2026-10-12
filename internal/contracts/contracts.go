// Package contracts defines the M0 execution and verification contracts.
package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type Control string

const (
	Active      Control = "ACTIVE"
	Pausing     Control = "PAUSING"
	Paused      Control = "PAUSED"
	Cancelled   Control = "CANCELLED"
	HumanOwned  Control = "HUMAN_OWNED"
	Reconciling Control = "RECONCILING"
)

type Phase string

const (
	Intake             Phase = "INTAKE"
	Developing         Phase = "DEVELOPING"
	Reviewing          Phase = "REVIEWING"
	AwaitingMaintainer Phase = "AWAITING_MAINTAINER"
	Blocked            Phase = "BLOCKED"
	Merged             Phase = "MERGED"
)

type VerificationKey struct {
	RepositoryID  int64  `json:"repository_id"`
	PullRequest   int    `json:"pull_request"`
	SpecDigest    string `json:"spec_digest"`
	Generation    int64  `json:"generation"`
	BaseSHA       string `json:"base_sha"`
	HeadSHA       string `json:"head_sha"`
	PolicyDigest  string `json:"policy_digest"`
	ProfileDigest string `json:"profile_digest"`
	RunnerDigest  string `json:"runner_digest"`
}

func Digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (v VerificationKey) Valid() bool {
	return v.RepositoryID > 0 && v.PullRequest > 0 && v.Generation > 0 && v.SpecDigest != "" && v.BaseSHA != "" && v.HeadSHA != "" && v.PolicyDigest != "" && v.ProfileDigest != "" && v.RunnerDigest != ""
}

type Attempt struct {
	ID         string    `json:"id"`
	Generation int64     `json:"generation"`
	Epoch      int64     `json:"epoch"`
	ExpiresAt  time.Time `json:"expires_at"`
	Status     string    `json:"status"`
}
type Workflow struct {
	PRDraft      bool            `json:"pr_draft"`
	Commands     []int64         `json:"commands,omitempty"`
	ID           string          `json:"id"`
	Repository   string          `json:"repository"`
	RepositoryID int64           `json:"repository_id"`
	Issue        int             `json:"issue"`
	SpecDigest   string          `json:"spec_digest"`
	Phase        Phase           `json:"phase"`
	Control      Control         `json:"control"`
	Generation   int64           `json:"generation"`
	Version      int64           `json:"version"`
	Attempt      Attempt         `json:"attempt"`
	Runs         int             `json:"runs"`
	BaseSHA      string          `json:"base_sha"`
	HeadSHA      string          `json:"head_sha"`
	PullRequest  int             `json:"pull_request"`
	Key          VerificationKey `json:"key"`
	Reason       string          `json:"reason"`
}

var ErrStale = errors.New("stale generation, lease, identity or state")

func (w Workflow) Accept(a Attempt, now time.Time) error {
	if w.Control != Active || w.Generation != a.Generation || w.Attempt.ID != a.ID || w.Attempt.Epoch != a.Epoch || w.Attempt.Status != "RUNNING" || !now.Before(w.Attempt.ExpiresAt) {
		return ErrStale
	}
	return nil
}

type Verdict string

const (
	Pass     Verdict = "PASS"
	Fail     Verdict = "FAIL"
	Pending  Verdict = "PENDING"
	Obsolete Verdict = "OBSOLETE"
)

type Obligation struct {
	ID       string `json:"id"`
	Result   string `json:"result"`
	Evidence string `json:"evidence"`
}
type Evidence struct {
	Key              VerificationKey `json:"key"`
	TestedCommit     string          `json:"tested_commit"`
	MechanicalPassed bool            `json:"mechanical_passed"`
	ProvenanceValid  bool            `json:"provenance_valid"`
	Pending          bool            `json:"pending"`
	BlockingFindings int             `json:"blocking_findings"`
	Coverage         []Obligation    `json:"coverage"`
}

func Gate(current VerificationKey, control Control, e Evidence, required []string) (Verdict, string) {
	if !current.Valid() || current != e.Key || e.TestedCommit != current.HeadSHA {
		return Obsolete, "verification identity mismatch"
	}
	if control != Active {
		return Fail, "workflow is not publishable"
	}
	if e.Pending {
		return Pending, "verification pending"
	}
	if !e.MechanicalPassed || !e.ProvenanceValid || e.BlockingFindings > 0 {
		return Fail, "checks, provenance or findings block merge"
	}
	if len(required) == 0 {
		return Fail, "mandatory obligations are empty"
	}
	seen := map[string]bool{}
	for _, o := range e.Coverage {
		if seen[o.ID] {
			return Fail, "duplicate coverage"
		}
		seen[o.ID] = true
	}
	for _, id := range required {
		ok := false
		for _, o := range e.Coverage {
			if o.ID == id && o.Result == "satisfied" && o.Evidence != "" {
				ok = true
			}
		}
		if !ok {
			return Fail, "mandatory obligation incomplete: " + id
		}
	}
	return Pass, "all M0 obligations completed"
}

type Check struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

func CheckFor(v Verdict) Check {
	switch v {
	case Pass:
		return Check{"completed", "success"}
	case Pending:
		return Check{"in_progress", ""}
	default:
		return Check{"completed", "failure"}
	}
}

type Action struct {
	DispatchEpoch int64           `json:"-"`
	ID            string          `json:"id"`
	WorkflowID    string          `json:"workflow_id"`
	Generation    int64           `json:"generation"`
	Epoch         int64           `json:"epoch"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Receipt       json.RawMessage `json:"receipt"`
}
type Candidate struct {
	BaseSHA        string            `json:"base_sha"`
	ExpectedHead   string            `json:"expected_head"`
	Files          map[string]string `json:"files"`
	ArtifactDigest string            `json:"artifact_digest"`
}
