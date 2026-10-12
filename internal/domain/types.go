package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type Criterion struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}
type Spec struct {
	Revision    int         `json:"revision"`
	Digest      string      `json:"digest"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Criteria    []Criterion `json:"criteria"`
	NonGoals    []string    `json:"non_goals"`
	Risk        string      `json:"risk"`
	SourceHash  string      `json:"source_hash"`
	ApprovedBy  string      `json:"approved_by"`
	Approved    bool        `json:"approved"`
}
type Task struct {
	ID            string          `json:"id"`
	Kind          string          `json:"kind,omitempty"`
	Description   string          `json:"description"`
	Paths         []string        `json:"paths"`
	DependsOn     []string        `json:"depends_on"`
	Status        string          `json:"status"`
	Attempts      int             `json:"attempts"`
	LeaseEpoch    int64           `json:"lease_epoch"`
	IntegratedSHA string          `json:"integrated_sha"`
	Report        json.RawMessage `json:"report,omitempty"`
	ObligationIDs []string        `json:"obligation_ids,omitempty"`
}
type Obligation struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	CriterionID string   `json:"criterion_id"`
	Result      string   `json:"result"`
	Evidence    []string `json:"evidence"`
}
type Finding struct {
	ID              string   `json:"id"`
	Severity        string   `json:"severity"`
	Category        string   `json:"category"`
	Path            string   `json:"path"`
	Line            int      `json:"line"`
	Description     string   `json:"description"`
	Expected        string   `json:"expected"`
	Actual          string   `json:"actual"`
	Preconditions   string   `json:"preconditions"`
	EvidenceType    string   `json:"evidence_type"`
	Evidence        []string `json:"evidence"`
	State           string   `json:"state"`
	VerificationKey string   `json:"verification_key"`
	History         []string `json:"history"`
}
type Evidence struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	Command         []string  `json:"command,omitempty"`
	ExitCode        int       `json:"exit_code"`
	Path            string    `json:"path"`
	Hash            string    `json:"hash"`
	CreatedAt       time.Time `json:"created_at"`
	BaseSHA         string    `json:"base_sha"`
	HeadSHA         string    `json:"head_sha"`
	ProfileDigest   string    `json:"profile_digest"`
	TestedCommitSHA string    `json:"tested_commit_sha,omitempty"`
	TestedTreeSHA   string    `json:"tested_tree_sha,omitempty"`
}
type Review struct {
	ID                 string       `json:"id"`
	Key                string       `json:"key"`
	Tasks              []Task       `json:"tasks"`
	Obligations        []Obligation `json:"obligations"`
	Findings           []Finding    `json:"findings"`
	SkepticComplete    bool         `json:"skeptic_complete"`
	VerificationPassed bool         `json:"verification_passed"`
	Verdict            string       `json:"verdict"`
	Reason             string       `json:"reason"`
}
type Workflow struct {
	ID              string     `json:"id"`
	RepositoryID    int64      `json:"repository_id"`
	Repository      string     `json:"repository"`
	InstallationID  int64      `json:"installation_id"`
	Issue           int        `json:"issue"`
	Phase           string     `json:"phase"`
	Control         string     `json:"control"`
	Generation      int64      `json:"generation"`
	Version         int64      `json:"version"`
	AuthorizedBy    string     `json:"authorized_by"`
	IssueTitle      string     `json:"issue_title"`
	IssueBody       string     `json:"issue_body"`
	Discussion      []string   `json:"discussion"`
	Spec            Spec       `json:"spec"`
	Tasks           []Task     `json:"tasks"`
	ProposalSpec    *Spec      `json:"proposal_spec,omitempty"`
	PR              int        `json:"pr"`
	PRReady         bool       `json:"pr_ready"`
	Branch          string     `json:"branch"`
	BaseBranch      string     `json:"base_branch"`
	BaseSHA         string     `json:"base_sha"`
	HeadSHA         string     `json:"head_sha"`
	TestedSHA       string     `json:"tested_sha,omitempty"`
	TestedTreeSHA   string     `json:"tested_tree_sha,omitempty"`
	CheckID         int64      `json:"check_id"`
	CheckSHA        string     `json:"check_sha"`
	GateStatus      string     `json:"gate_status"`
	GateConclusion  string     `json:"gate_conclusion"`
	GateConfirmedAt time.Time  `json:"gate_confirmed_at,omitempty"`
	Review          Review     `json:"review"`
	Evidence        []Evidence `json:"evidence"`
	FixRounds       int        `json:"fix_rounds"`
	ModelCalls      int        `json:"model_calls"`
	InputTokens     int64      `json:"input_tokens"`
	OutputTokens    int64      `json:"output_tokens"`
	RuntimeSeconds  int64      `json:"runtime_seconds"`
	LastError       string     `json:"last_error"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CreatedAt       time.Time  `json:"created_at"`
	LeaseOwner      string     `json:"lease_owner,omitempty"`
	LeaseUntil      time.Time  `json:"lease_until,omitempty"`
}
type GateDecision struct {
	ID              string       `json:"id"`
	WorkflowID      string       `json:"workflow_id"`
	Generation      int64        `json:"generation"`
	VerificationKey string       `json:"verification_key"`
	SpecDigest      string       `json:"spec_digest"`
	BaseSHA         string       `json:"base_sha"`
	HeadSHA         string       `json:"head_sha"`
	Verdict         string       `json:"verdict"`
	Reason          string       `json:"reason"`
	Evidence        []Evidence   `json:"evidence"`
	Coverage        []Obligation `json:"coverage"`
	Findings        []Finding    `json:"findings"`
	CreatedAt       time.Time    `json:"created_at"`
}

func ID(repoID int64, issue int) string { return fmt.Sprintf("%d-%d", repoID, issue) }
func Digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func SpecDigest(s Spec) string { s.Digest = ""; return Digest(s) }
func VerificationKey(w Workflow, policy, profile, image string) string {
	return Digest([]any{w.RepositoryID, w.PR, w.Spec.Digest, w.Generation, w.BaseSHA, w.HeadSHA, policy, profile, image})
}
func Runnable(w Workflow) bool {
	if w.Control != "ACTIVE" {
		return false
	}
	switch w.Phase {
	case "TRIAGE", "PLANNING", "DEVELOPING", "VERIFYING", "REVIEWING", "CHANGES_REQUESTED":
		return true
	}
	return false
}
func Gate(w Workflow, key string) (bool, string) {
	if w.Control != "ACTIVE" {
		return false, "workflow is not active"
	}
	if !w.Spec.Approved || w.Review.Key != key {
		return false, "review identity is obsolete or spec is not approved"
	}
	if !w.Review.VerificationPassed || !w.Review.SkepticComplete {
		return false, "verification or skeptic is incomplete"
	}
	if len(w.Review.Obligations) == 0 {
		return false, "review coverage is empty"
	}
	covered := map[string]bool{}
	for _, o := range w.Review.Obligations {
		if o.Result != "satisfied" || len(o.Evidence) == 0 {
			return false, "mandatory obligation " + o.ID + " is not verified"
		}
		covered[o.CriterionID] = true
	}
	for _, c := range w.Spec.Criteria {
		if !covered[c.ID] {
			return false, "acceptance criterion " + c.ID + " is uncovered"
		}
	}
	for _, f := range w.Review.Findings {
		if f.Severity == "critical" || f.Severity == "high" || f.Severity == "medium" {
			if f.State != "REFUTED" && f.State != "RESOLVED" {
				return false, "unresolved finding " + f.ID
			}
		}
	}
	return true, "current candidate meets all mandatory obligations"
}
