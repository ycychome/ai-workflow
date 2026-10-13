package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// IssueSourceHash preserves indentation, literal spaces and Markdown semantics.
// Only CRLF transport normalization is considered an exact no-change edit.
func IssueSourceHash(title, body string) string {
	return "issue-v3:" + Digest([]string{strings.ReplaceAll(title, "\r\n", "\n"), strings.ReplaceAll(body, "\r\n", "\n")})
}

type SpecChangeDecision struct {
	Decision               string    `json:"decision"`
	Actor                  string    `json:"actor"`
	Reason                 string    `json:"reason"`
	ExpectedApprovedDigest string    `json:"expected_approved_digest"`
	DecidedAt              time.Time `json:"decided_at"`
}
type SpecChangeProposal struct {
	ID                     string               `json:"id"`
	PreviousApprovedDigest string               `json:"previous_approved_digest"`
	SourceHash             string               `json:"source_hash"`
	Spec                   Spec                 `json:"spec"`
	ProposedBy             string               `json:"proposed_by"`
	ProposedAt             time.Time            `json:"proposed_at"`
	Decisions              []SpecChangeDecision `json:"decisions"`
}
type SpecHistoryEntry struct {
	ProposalID             string    `json:"proposal_id"`
	PreviousApprovedDigest string    `json:"previous_approved_digest"`
	Spec                   Spec      `json:"spec"`
	ApprovedBy             string    `json:"approved_by"`
	ApprovedAt             time.Time `json:"approved_at"`
}
type RiskPathChange struct {
	BeforeDigest string `json:"before_digest,omitempty"`
	AfterDigest  string `json:"after_digest,omitempty"`
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path,omitempty"`
	Status       string `json:"status"`
}
type RiskAuthorization struct {
	ID              string           `json:"id"`
	VerificationKey string           `json:"verification_key"`
	Changes         []RiskPathChange `json:"changes"`
	ChangesDigest   string           `json:"changes_digest"`
	Actor           string           `json:"actor"`
	Reason          string           `json:"reason"`
	CreatedAt       time.Time        `json:"created_at"`
	ExpiresAt       time.Time        `json:"expires_at"`
}

func approvedSpecDigest(w Workflow) string {
	if w.Spec.Approved {
		return w.Spec.Digest
	}
	return ""
}
func cloneSpec(spec Spec) Spec {
	spec.Criteria = append([]Criterion(nil), spec.Criteria...)
	spec.NonGoals = append([]string(nil), spec.NonGoals...)
	return spec
}
func specChangeID(wid string, p SpecChangeProposal) string {
	return "SC-" + Digest([]any{wid, p.PreviousApprovedDigest, p.SourceHash, p.Spec, p.ProposedBy, p.ProposedAt})[:32]
}
func validateImmutableProposal(wid string, p SpecChangeProposal) error {
	if p.ID != specChangeID(wid, p) || p.Spec.Digest != SpecDigest(p.Spec) || p.Spec.Approved || p.Spec.ApprovedBy != "" || p.SourceHash != p.Spec.SourceHash || p.ProposedAt.IsZero() || strings.TrimSpace(p.ProposedBy) == "" {
		return errors.New("规格提案身份或内容已改变")
	}
	return nil
}

// ProposeSpecChange appends an immutable candidate and never changes w.Spec.
// The caller must persist the returned workflow with normal lease/version CAS.
func ProposeSpecChange(w *Workflow, spec Spec, actor string, now time.Time) (SpecChangeProposal, error) {
	var empty SpecChangeProposal
	if w.Spec.Approved && (w.Spec.Digest == "" || w.Spec.Digest != SpecDigest(w.Spec)) {
		return empty, errors.New("已批准规格摘要不一致，不能据此提出新版本")
	}
	if strings.TrimSpace(actor) == "" || now.IsZero() {
		return empty, errors.New("规格提案缺少提出身份或时间")
	}
	if strings.TrimSpace(spec.Title) == "" || strings.TrimSpace(spec.Description) == "" || len(spec.Criteria) == 0 {
		return empty, errors.New("规格提案必须包含标题、说明和验收标准")
	}
	if spec.SourceHash != IssueSourceHash(w.IssueTitle, w.IssueBody) {
		return empty, errors.New("规格提案与当前 Issue 原文不一致")
	}
	if spec.Revision != w.Spec.Revision+1 {
		return empty, errors.New("规格提案修订号必须紧接当前已批准规格")
	}
	seen := map[string]bool{}
	for _, criterion := range spec.Criteria {
		if strings.TrimSpace(criterion.ID) == "" || strings.TrimSpace(criterion.Description) == "" || seen[criterion.ID] {
			return empty, errors.New("规格验收标准 ID 重复或内容缺失")
		}
		seen[criterion.ID] = true
	}
	spec = cloneSpec(spec)
	spec.Approved = false
	spec.ApprovedBy = ""
	spec.Digest = SpecDigest(spec)
	previous := approvedSpecDigest(*w)
	for _, existing := range w.SpecChanges {
		if existing.PreviousApprovedDigest == previous && existing.Spec.Digest == spec.Digest && existing.ProposedBy == actor && len(existing.Decisions) == 0 {
			if err := validateImmutableProposal(w.ID, existing); err != nil {
				return empty, err
			}
			return existing, nil
		}
	}
	proposal := SpecChangeProposal{PreviousApprovedDigest: previous, SourceHash: spec.SourceHash, Spec: spec, ProposedBy: actor, ProposedAt: now.UTC(), Decisions: []SpecChangeDecision{}}
	proposal.ID = specChangeID(w.ID, proposal)
	w.SpecChanges = append(w.SpecChanges, proposal)
	display := cloneSpec(spec)
	w.ProposalSpec = &display
	return proposal, nil
}

// DecideSpecChange requires both the immutable proposal ID and the approved
// digest observed by the approving caller. No model semantic-equivalence claim
// can bypass this comparison or the current issue-v3 source identity.
func DecideSpecChange(w *Workflow, id, expectedApprovedDigest, decision, actor, reason string, now time.Time) error {
	if w.Spec.Approved && (w.Spec.Digest == "" || w.Spec.Digest != SpecDigest(w.Spec)) {
		return errors.New("已批准规格摘要不一致，不能接受新版本")
	}
	if decision != "accepted" && decision != "rejected" {
		return errors.New("规格决议必须为 accepted 或 rejected")
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || now.IsZero() {
		return errors.New("规格决议缺少身份、理由或时间")
	}
	for i := range w.SpecChanges {
		p := &w.SpecChanges[i]
		if p.ID != id {
			continue
		}
		if err := validateImmutableProposal(w.ID, *p); err != nil {
			return err
		}
		if len(p.Decisions) > 0 {
			return errors.New("规格提案已有最终决议，不能覆盖历史")
		}
		if p.PreviousApprovedDigest != expectedApprovedDigest || approvedSpecDigest(*w) != expectedApprovedDigest {
			return errors.New("已批准规格已变化，请重新检查后决定")
		}
		if p.SourceHash != IssueSourceHash(w.IssueTitle, w.IssueBody) {
			return errors.New("Issue 已变化，此规格提案不能继续批准")
		}
		if now.Before(p.ProposedAt) {
			return errors.New("决议时间不能早于提出时间")
		}
		p.Decisions = append(p.Decisions, SpecChangeDecision{Decision: decision, Actor: actor, Reason: reason, ExpectedApprovedDigest: expectedApprovedDigest, DecidedAt: now.UTC()})
		if decision == "rejected" {
			if w.ProposalSpec != nil && w.ProposalSpec.Digest == p.Spec.Digest {
				w.ProposalSpec = nil
			}
			return nil
		}
		if w.Spec.Approved {
			recorded := false
			for _, history := range w.SpecHistory {
				recorded = recorded || history.Spec.Digest == w.Spec.Digest
			}
			if !recorded {
				w.SpecHistory = append(w.SpecHistory, SpecHistoryEntry{Spec: cloneSpec(w.Spec), ApprovedBy: w.Spec.ApprovedBy})
			}
		}
		next := cloneSpec(p.Spec)
		next.Approved = true
		next.ApprovedBy = actor
		next.Digest = SpecDigest(next)
		w.SpecHistory = append(w.SpecHistory, SpecHistoryEntry{ProposalID: p.ID, PreviousApprovedDigest: expectedApprovedDigest, Spec: cloneSpec(next), ApprovedBy: actor, ApprovedAt: now.UTC()})
		w.Spec = next
		w.ProposalSpec = nil
		return nil
	}
	return fmt.Errorf("未找到规格提案 %q", id)
}

// ApplyIssueSourceEdit updates presentation only for exact normalized equality.
// Every other edit is treated as semantic or uncertain and revokes the current
// generation without rewriting the previously approved specification.
func ApplyIssueSourceEdit(w *Workflow, title, body string) (bool, error) {
	if w.IssueTitle == title && w.IssueBody == body {
		return false, nil
	}
	same := IssueSourceHash(w.IssueTitle, w.IssueBody) == IssueSourceHash(title, body)
	w.IssueTitle = title
	w.IssueBody = body
	if same {
		return false, nil
	}
	w.Generation++
	if w.Control == "ACTIVE" {
		w.Control = "PAUSING"
	}
	w.Review.Verdict = "OBSOLETE"
	w.ForceApproval = nil
	w.ProposalSpec = nil
	w.LastError = "Issue 内容已变化，请停止旧运行并重新提出规格；已批准规格保留，只有明确批准新提案后才会替换。"
	return true, nil
}
