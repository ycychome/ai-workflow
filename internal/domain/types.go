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
	BudgetDeferred bool            `json:"budget_deferred,omitempty"`
	Role           string          `json:"role,omitempty"`
	PlanRevision   int             `json:"plan_revision"`
	FindingIDs     []string        `json:"finding_ids,omitempty"`
	AgentRunID     string          `json:"agent_run_id,omitempty"`
	Preflight      *Preflight      `json:"preflight,omitempty"`
	ID             string          `json:"id"`
	Kind           string          `json:"kind,omitempty"`
	Description    string          `json:"description"`
	Paths          []string        `json:"paths"`
	DependsOn      []string        `json:"depends_on"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	LeaseEpoch     int64           `json:"lease_epoch"`
	IntegratedSHA  string          `json:"integrated_sha"`
	Report         json.RawMessage `json:"report,omitempty"`
	ObligationIDs  []string        `json:"obligation_ids,omitempty"`
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
	AgentRunID      string    `json:"agent_run_id,omitempty"`
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
	PlanRevision       int          `json:"plan_revision"`
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

// ForceApproval records a maintainer's explicit decision to bypass automatic
// analysis or review. It is an identity-bound authorization, not verification
// evidence; PR approvals are valid only for the exact recorded candidate.
type ForceApproval struct {
	Kind             string      `json:"kind"`
	Actor            string      `json:"actor"`
	Command          string      `json:"command"`
	ApprovedAt       time.Time   `json:"approved_at"`
	Generation       int64       `json:"generation"`
	SpecDigest       string      `json:"spec_digest"`
	PolicyDigest     string      `json:"policy_digest"`
	VerificationKey  string      `json:"verification_key"`
	IssueHash        string      `json:"issue_hash,omitempty"`
	BaseSHA          string      `json:"base_sha,omitempty"`
	HeadSHA          string      `json:"head_sha,omitempty"`
	Bypassed         []string    `json:"bypassed"`
	PriorConstraints []Criterion `json:"prior_constraints,omitempty"`
	Discussion       []string    `json:"discussion,omitempty"`
}
type Workflow struct {
	PlannedStateDigest string               `json:"planned_state_digest,omitempty"`
	ResumePhase        string               `json:"resume_phase,omitempty"`
	BudgetDeferredAt   time.Time            `json:"budget_deferred_at,omitempty"`
	Evaluation         *EvaluationContext   `json:"evaluation,omitempty"`
	RepositoryEpoch    string               `json:"repository_epoch"`
	SpecHistory        []SpecHistoryEntry   `json:"spec_history,omitempty"`
	SpecChanges        []SpecChangeProposal `json:"spec_changes,omitempty"`
	RiskAuthorizations []RiskAuthorization  `json:"risk_authorizations,omitempty"`
	PlanRevision       int                  `json:"plan_revision"`
	TasksCreated       int                  `json:"tasks_created"`
	PlanHistory        []PlanDelta          `json:"plan_history,omitempty"`
	SourceArtifacts    []SourceArtifact     `json:"source_artifacts,omitempty"`
	IntegrationRecords []IntegrationRecord  `json:"integration_records,omitempty"`
	EvidenceRequests   []EvidenceRequest    `json:"evidence_requests,omitempty"`
	FindingResolutions []FindingResolution  `json:"finding_resolutions,omitempty"`
	Waivers            []Waiver             `json:"waivers,omitempty"`
	ID                 string               `json:"id"`
	RepositoryID       int64                `json:"repository_id"`
	Repository         string               `json:"repository"`
	InstallationID     int64                `json:"installation_id"`
	Issue              int                  `json:"issue"`
	Phase              string               `json:"phase"`
	Control            string               `json:"control"`
	Generation         int64                `json:"generation"`
	Version            int64                `json:"version"`
	AuthorizedBy       string               `json:"authorized_by"`
	IssueTitle         string               `json:"issue_title"`
	IssueBody          string               `json:"issue_body"`
	LegacyIssueTitle   *string              `json:"legacy_issue_title,omitempty"`
	Discussion         []string             `json:"discussion"`
	Spec               Spec                 `json:"spec"`
	Tasks              []Task               `json:"tasks"`
	ProposalSpec       *Spec                `json:"proposal_spec,omitempty"`
	PR                 int                  `json:"pr"`
	PRReady            bool                 `json:"pr_ready"`
	Branch             string               `json:"branch"`
	BaseBranch         string               `json:"base_branch"`
	BaseSHA            string               `json:"base_sha"`
	HeadSHA            string               `json:"head_sha"`
	TestedSHA          string               `json:"tested_sha,omitempty"`
	TestedTreeSHA      string               `json:"tested_tree_sha,omitempty"`
	CheckID            int64                `json:"check_id"`
	CheckSHA           string               `json:"check_sha"`
	GateStatus         string               `json:"gate_status"`
	GateConclusion     string               `json:"gate_conclusion"`
	GateConfirmedAt    time.Time            `json:"gate_confirmed_at,omitempty"`
	Review             Review               `json:"review"`
	ForceApproval      *ForceApproval       `json:"force_approval,omitempty"`
	Evidence           []Evidence           `json:"evidence"`
	FixRounds          int                  `json:"fix_rounds"`
	ModelCalls         int                  `json:"model_calls"`
	InputTokens        int64                `json:"input_tokens"`
	OutputTokens       int64                `json:"output_tokens"`
	RuntimeSeconds     int64                `json:"runtime_seconds"`
	LastError          string               `json:"last_error"`
	UpdatedAt          time.Time            `json:"updated_at"`
	CreatedAt          time.Time            `json:"created_at"`
	LeaseOwner         string               `json:"lease_owner,omitempty"`
	LeaseUntil         time.Time            `json:"lease_until,omitempty"`
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
	if w.Review.Verdict == "FORCED" {
		return false, "人工越过不代表自动验证通过"
	}
	if w.Control != "ACTIVE" {
		return false, "工作流当前未运行"
	}
	if !w.Spec.Approved || w.Review.Key != key {
		return false, "审查身份已过期或规格尚未批准"
	}
	if !w.Review.VerificationPassed || !w.Review.SkepticComplete {
		return false, "验证或质疑复核尚未完成"
	}
	if len(w.Review.Tasks) == 0 {
		return false, "独立审查任务为空"
	}
	assigned := map[string]bool{}
	for _, task := range w.Review.Tasks {
		if task.Status == "SUPERSEDED" {
			continue
		}
		if task.Status != "ACCEPTED" {
			return false, "审查任务尚未完成：" + task.ID
		}
		if task.ID != "skeptic" {
			for _, id := range task.ObligationIDs {
				assigned[id] = true
			}
		}
	}
	for _, request := range w.EvidenceRequests {
		if request.VerificationKey == key && request.Status != "COMPLETED" && request.Status != "OBSOLETE" {
			return false, "补充证据请求尚未完成：" + request.ID
		}
	}
	if len(w.Review.Obligations) == 0 {
		return false, "审查覆盖范围为空"
	}
	covered := map[string]bool{}
	for _, o := range w.Review.Obligations {
		if !assigned[o.ID] {
			return false, "强制审查义务尚未分配：" + o.ID
		}
		if o.Result != "satisfied" || len(o.Evidence) == 0 {
			return false, "强制审查义务 " + o.ID + " 尚未验证"
		}
		covered[o.CriterionID] = true
	}
	for _, c := range w.Spec.Criteria {
		if !covered[c.ID] {
			return false, "验收条件 " + c.ID + " 尚未覆盖"
		}
	}
	for _, f := range w.Review.Findings {
		if err := ValidateFindingEnums(f, true); err != nil {
			return false, "无效审查发现 " + f.ID + ": " + err.Error()
		}
		if f.Severity == "critical" || f.Severity == "high" || f.Severity == "medium" {
			if f.State != "REFUTED" && f.State != "RESOLVED" && !ValidFindingWaiver(w, f, key, time.Now()) {
				return false, "尚未解决的审查发现 " + f.ID
			}
		}
	}
	return true, "当前候选满足全部强制审查义务"
}

// Internal verification may retain FIX_CANDIDATE while a changed candidate awaits review.
// Model reports are restricted to the public report states.
func ValidateFindingEnums(f Finding, internal bool) error {
	switch f.Severity {
	case "critical", "high", "medium", "low", "info":
	default:
		return fmt.Errorf("invalid severity %q", f.Severity)
	}
	switch f.EvidenceType {
	case "MODEL_CLAIM", "SOURCE_ANALYSIS", "TOOL_VERIFIED":
	default:
		return fmt.Errorf("invalid evidence type %q", f.EvidenceType)
	}
	switch f.State {
	case "PROPOSED", "CONFIRMED", "REFUTED", "DISPUTED", "RESOLVED":
	case "FIX_CANDIDATE", "WAIVED":
		if !internal {
			return fmt.Errorf("invalid report state %q", f.State)
		}
	default:
		return fmt.Errorf("invalid state %q", f.State)
	}
	return nil
}

func ValidFindingWaiver(w Workflow, f Finding, key string, now time.Time) bool {
	if f.State != "WAIVED" || f.VerificationKey != key {
		return false
	}
	for _, waiver := range w.Waivers {
		if waiver.FindingID == f.ID && waiver.VerificationKey == key && waiver.Actor != "" && waiver.Reason != "" && !waiver.CreatedAt.IsZero() && !waiver.CreatedAt.After(now) && waiver.ExpiresAt.After(now) {
			return true
		}
	}
	return false
}
