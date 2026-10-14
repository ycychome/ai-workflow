package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

var ErrNoEvidenceRequest = errors.New("没有可领取的补充证据请求")

type EvidenceIntent struct {
	Request domain.EvidenceRequest `json:"request"`
	Task    domain.Task            `json:"task"`
}

// coordinationSnapshot rejects stale policy/source/spec identities before any
// ledger update. The locked update also checks the captured workflow generation.
func (e *Engine) coordinationSnapshot(ctx context.Context, id, key string) (domain.Workflow, error) {
	w, err := e.Store.Get(ctx, id)
	if err != nil {
		return w, err
	}
	if key == "" || w.Review.Key != key || e.key(ctx, w) != key || w.HeadSHA == "" || !w.Spec.Approved {
		return w, errors.New("冻结验证身份缺失或已过期")
	}
	return w, nil
}
func checkCoordinationKey(w domain.Workflow, key string) error {
	if key == "" || w.Review.Key != key {
		return errors.New("验证身份已变化，请针对当前冻结候选重新申请")
	}
	return nil
}
func (e *Engine) coordinationRun(ctx context.Context, w domain.Workflow, id string, roles ...string) (domain.AgentRun, error) {
	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil {
		return domain.AgentRun{}, err
	}
	for _, run := range runs {
		if run.ID != id {
			continue
		}
		allowed := false
		for _, role := range roles {
			allowed = allowed || run.Role == role
		}
		if !allowed || run.Generation != w.Generation || (run.Status != domain.AgentActive && run.Status != domain.AgentFinished) || run.StartedAt.IsZero() || (run.Status == domain.AgentActive && !run.LeaseUntil.After(time.Now())) {
			return run, errors.New("智能体运行身份、角色或状态不满足此操作要求")
		}
		if run.Status == domain.AgentFinished {
			accepted, err := e.Store.HasAcceptedAgentAttempt(ctx, run)
			if err != nil {
				return run, err
			}
			if !accepted {
				return run, errors.New("已结束运行缺少已接受的任务报告记录")
			}
		}
		return run, nil
	}
	return domain.AgentRun{}, errors.New("未找到已调度的智能体运行记录")
}
func findCoordinationFinding(w *domain.Workflow, id string) *domain.Finding {
	for i := range w.Review.Findings {
		if w.Review.Findings[i].ID == id {
			return &w.Review.Findings[i]
		}
	}
	return nil
}

// RequestEvidence accepts only a trusted requester run, never a model-provided
// role string. Requests remain in the ledger across retries/replans for quota.
func (e *Engine) RequestEvidence(ctx context.Context, workflowID, key, requesterRunID, targetRole, question, findingID string) (domain.EvidenceRequest, error) {
	var result domain.EvidenceRequest
	w, err := e.coordinationSnapshot(ctx, workflowID, key)
	if err != nil {
		return result, err
	}
	run, err := e.coordinationRun(ctx, w, requesterRunID, "review_planner", "skeptic")
	if err != nil {
		return result, err
	}
	if err := validateEvidenceProposalFields(evidenceProposal{Role: targetRole, Question: question, FindingID: findingID}); err != nil {
		return result, err
	}
	id := "ER-" + domain.Digest([]string{w.ID, key, requesterRunID, targetRole, question, findingID})[:24]
	err = e.Store.UpdateCoordination(store.WithCoordinationRun(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), run), w.ID, "evidence_request_created", func(x *domain.Workflow) (any, error) {
		if err := checkCoordinationKey(*x, key); err != nil {
			return nil, err
		}
		if x.Control != "ACTIVE" {
			return nil, errors.New("工作流当前不能申请补充证据")
		}
		for _, old := range x.EvidenceRequests {
			if old.ID == id {
				result = old
				return nil, store.ErrNoChange
			}
		}
		if len(x.EvidenceRequests) >= e.Config().EffectiveMaxEvidenceRequests() {
			return nil, errors.New("补充证据请求累计限额已耗尽")
		}
		if findingID != "" {
			f := findCoordinationFinding(x, findingID)
			if f == nil || f.VerificationKey != key {
				return nil, errors.New("请求关联的发现不存在或不属于当前验证身份")
			}
		}
		now := time.Now().UTC()
		result = domain.EvidenceRequest{ID: id, RequestedBy: run.ID, TargetRole: targetRole, TaskID: "evidence-" + id, FindingID: findingID, Question: question, VerificationKey: key, Status: "QUEUED", Evidence: []string{}, CreatedAt: now, UpdatedAt: now}
		x.EvidenceRequests = append(x.EvidenceRequests, result)
		return result, nil
	})
	return result, err
}
func evidenceIntent(request domain.EvidenceRequest) EvidenceIntent {
	findings := []string{}
	if request.FindingID != "" {
		findings = append(findings, request.FindingID)
	}
	return EvidenceIntent{Request: request, Task: domain.Task{ID: request.TaskID, Role: request.TargetRole, Kind: "verification", Description: request.Question, Paths: []string{}, DependsOn: []string{}, ObligationIDs: []string{}, FindingIDs: findings, Status: "QUEUED", Attempts: request.Attempt}}
}
func (e *Engine) ClaimEvidenceRequest(ctx context.Context, workflowID, key string) (EvidenceIntent, error) {
	var result EvidenceIntent
	w, err := e.coordinationSnapshot(ctx, workflowID, key)
	if err != nil {
		return result, err
	}
	err = e.Store.UpdateCoordination(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), workflowID, "evidence_request_claimed", func(x *domain.Workflow) (any, error) {
		if err := checkCoordinationKey(*x, key); err != nil {
			return nil, err
		}
		if x.Control != "ACTIVE" {
			return nil, errors.New("工作流当前不能执行补充证据请求")
		}
		for i := range x.EvidenceRequests {
			r := &x.EvidenceRequests[i]
			if r.Status != "QUEUED" || r.VerificationKey != key {
				continue
			}
			deferred := false
			for _, task := range x.Review.Tasks {
				if task.ID == r.TaskID && task.BudgetDeferred {
					deferred = true
				}
			}
			if !deferred && r.Attempt >= e.Config().Budget.MaxAttempts {
				return nil, errors.New("补充证据请求尝试次数已耗尽")
			}
			r.Status = "RUNNING"
			if !deferred {
				r.Attempt++
			}
			r.AgentRunID = ""
			r.UpdatedAt = time.Now().UTC()
			result = evidenceIntent(*r)
			return *r, nil
		}
		return nil, ErrNoEvidenceRequest
	})
	return result, err
}

func (e *Engine) BindEvidenceRequestRun(ctx context.Context, workflowID, key, requestID, runID string) error {
	w, err := e.coordinationSnapshot(ctx, workflowID, key)
	if err != nil {
		return err
	}
	run, err := e.coordinationRun(ctx, w, runID, "verifier", "reviewer")
	if err != nil {
		return err
	}
	return e.Store.UpdateCoordination(store.WithCoordinationRun(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), run), workflowID, "evidence_request_run_bound", func(x *domain.Workflow) (any, error) {
		if err := checkCoordinationKey(*x, key); err != nil {
			return nil, err
		}
		for i := range x.EvidenceRequests {
			r := &x.EvidenceRequests[i]
			if r.ID != requestID {
				continue
			}
			if r.Status != "RUNNING" || r.VerificationKey != key || r.TargetRole != run.Role || r.TaskID != run.TaskID {
				return nil, errors.New("补充证据请求与执行任务不匹配")
			}
			if r.AgentRunID != "" && r.AgentRunID != run.ID {
				return nil, errors.New("请求已经绑定另一运行")
			}
			if r.AgentRunID == run.ID {
				return nil, store.ErrNoChange
			}
			if run.StartedAt.Before(r.UpdatedAt) {
				return nil, errors.New("执行运行早于本次领取，不可绑定旧尝试")
			}
			r.AgentRunID = run.ID
			r.UpdatedAt = time.Now().UTC()
			return *r, nil
		}
		return nil, errors.New("未找到补充证据请求")
	})
}

// validateCoordinationProof accepts supervisor-collected tool records. The
// caller must pass toolSession.listEvidence(), never IDs asserted by a model.
func (e *Engine) validateCoordinationProof(w domain.Workflow, run domain.AgentRun, proof []domain.Evidence) error {
	if len(proof) == 0 {
		return errors.New("必须提供本次独立执行的实际证据")
	}
	seen := map[string]bool{}
	for _, ev := range proof {
		if ev.ID == "" || ev.AgentRunID != run.ID || seen[ev.ID] || ev.BaseSHA != w.BaseSHA || ev.HeadSHA != w.HeadSHA || ev.CreatedAt.Before(run.StartedAt) || (ev.Kind != "TOOL_VERIFIED" && ev.Kind != "SOURCE_ANALYSIS") {
			return errors.New("证据重复、过期、来源类型无效或不属于当前候选")
		}
		seen[ev.ID] = true
		if run.Role == "verifier" && ev.Kind != "TOOL_VERIFIED" {
			return errors.New("verifier 必须提供实际工具执行证据")
		}
		if _, err := readRecordedEvidence(e.Config().DataDir, w.ID, ev); err != nil {
			return fmt.Errorf("证据 %s 校验失败：%w", ev.ID, err)
		}
	}
	return nil
}
func appendCoordinationProof(w *domain.Workflow, proof []domain.Evidence) ([]string, error) {
	ids := make([]string, 0, len(proof))
	for _, ev := range proof {
		ids = append(ids, ev.ID)
		found := false
		for _, old := range w.Evidence {
			if old.ID == ev.ID {
				if domain.Digest(old) != domain.Digest(ev) {
					return nil, errors.New("证据 ID 已存在且内容不同")
				}
				found = true
				break
			}
		}
		if !found {
			w.Evidence = append(w.Evidence, ev)
		}
	}
	return ids, nil
}
func (e *Engine) CompleteEvidenceRequest(ctx context.Context, workflowID, key, requestID, runID string, proof []domain.Evidence) error {
	w, err := e.coordinationSnapshot(ctx, workflowID, key)
	if err != nil {
		return err
	}
	run, err := e.coordinationRun(ctx, w, runID, "verifier", "reviewer")
	if err != nil {
		return err
	}
	if err = e.validateCoordinationProof(w, run, proof); err != nil {
		return err
	}
	// Only a persisted accepted report may complete the request/task pair.
	var acceptedReport json.RawMessage
	var acceptedEpoch int64
	if err = e.Store.DB.QueryRow(ctx, `SELECT report,lease_epoch FROM attempts WHERE id=$1 AND workflow_id=$2 AND intent_id=$3 AND generation=$4 AND state='ACCEPTED' AND report IS NOT NULL AND report<>'null'::jsonb`, run.ID, workflowID, run.TaskID, run.Generation).Scan(&acceptedReport, &acceptedEpoch); err != nil {
		return fmt.Errorf("补充证据缺少已接受的持久报告: %w", err)
	}
	return e.Store.UpdateCoordination(store.WithCoordinationRun(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), run), workflowID, "evidence_request_completed", func(x *domain.Workflow) (any, error) {
		if err := checkCoordinationKey(*x, key); err != nil {
			return nil, err
		}
		if x.Control != "ACTIVE" {
			return nil, errors.New("工作流已停止，不能接受晚到证据")
		}
		for i := range x.EvidenceRequests {
			r := &x.EvidenceRequests[i]
			if r.ID != requestID {
				continue
			}
			if (r.Status != "RUNNING" && r.Status != "COMPLETED") || r.VerificationKey != key || r.AgentRunID != run.ID || r.TaskID != run.TaskID || r.TargetRole != run.Role {
				return nil, errors.New("补充证据请求的执行身份已变更")
			}
			taskIndex := -1
			for j := range x.Review.Tasks {
				if x.Review.Tasks[j].ID == r.TaskID {
					taskIndex = j
					break
				}
			}
			if taskIndex < 0 {
				return nil, errors.New("补充证据对应的任务已经不存在")
			}
			task := &x.Review.Tasks[taskIndex]
			if task.LeaseEpoch != acceptedEpoch || task.PlanRevision != run.PlanRevision || task.Status == "SUPERSEDED" {
				return nil, store.ErrStale
			}
			ids, err := appendCoordinationProof(x, proof)
			if err != nil {
				return nil, err
			}
			// Completion and accepted task visibility share one workflow transaction.
			// A crash after commit cannot leave the review DAG waiting on this request.
			task.Status = "ACCEPTED"
			task.BudgetDeferred = false
			task.AgentRunID = run.ID
			task.Report = acceptedReport
			r.Evidence = ids
			r.Status = "COMPLETED"
			r.UpdatedAt = time.Now().UTC()
			return *r, nil
		}
		return nil, errors.New("未找到补充证据请求")
	})
}

// RetireEvidenceRequests records terminal states without releasing agent slots.
// The scheduler owns cancellation/STOPPING and must confirm exit independently.
func (e *Engine) RetireEvidenceRequests(ctx context.Context, workflowID, currentKey string) error {
	return e.Store.UpdateCoordination(ctx, workflowID, "evidence_requests_obsoleted", func(w *domain.Workflow) (any, error) {
		ids := []string{}
		for i := range w.EvidenceRequests {
			r := &w.EvidenceRequests[i]
			if r.VerificationKey != currentKey && r.Status != "OBSOLETE" {
				r.Status = "OBSOLETE"
				r.UpdatedAt = time.Now().UTC()
				ids = append(ids, r.ID)
			}
		}
		if len(ids) == 0 {
			return nil, store.ErrNoChange
		}
		return map[string]any{"request_ids": ids, "verification_key": currentKey}, nil
	})
}

// RequeueEvidenceRequest is a controller recovery action after a failed attempt.
// A RUNNING/STOPPING process must first be stopped through the agent supervisor.
func (e *Engine) RequeueEvidenceRequest(ctx context.Context, workflowID, key, requestID string, expectedAttempt int) error {
	w, err := e.coordinationSnapshot(ctx, workflowID, key)
	if err != nil {
		return err
	}
	runs, err := e.Store.ListAgentRuns(ctx, workflowID)
	if err != nil {
		return err
	}
	return e.Store.UpdateCoordination(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), workflowID, "evidence_request_requeued", func(x *domain.Workflow) (any, error) {
		if err := checkCoordinationKey(*x, key); err != nil {
			return nil, err
		}
		for i := range x.EvidenceRequests {
			r := &x.EvidenceRequests[i]
			if r.ID != requestID {
				continue
			}
			if r.Status != "RUNNING" || r.Attempt != expectedAttempt {
				return nil, errors.New("补充证据请求尝试已变化")
			}
			for _, run := range runs {
				if run.ID == r.AgentRunID && (run.Status == domain.AgentActive || run.Status == domain.AgentStopping) {
					return nil, errors.New("旧运行尚未确认退出，不能重新排队")
				}
			}
			r.Status = "QUEUED"
			r.AgentRunID = ""
			r.UpdatedAt = time.Now().UTC()
			return *r, nil
		}
		return nil, errors.New("未找到补充证据请求")
	})
}

// RecordFindingResolution records a builder's proposal only. Neither fixes nor
// rebuttals can change Finding.State until a separately scheduled review run
// supplies new, hash-verified evidence through VerifyFindingResolution.
func (e *Engine) RecordFindingResolution(ctx context.Context, workflowID, key, builderRunID, findingID, kind, argument string, proof []domain.Evidence) (domain.FindingResolution, error) {
	var result domain.FindingResolution
	w, err := e.coordinationSnapshot(ctx, workflowID, key)
	if err != nil {
		return result, err
	}
	run, err := e.coordinationRun(ctx, w, builderRunID, "builder")
	if err != nil {
		return result, err
	}
	if kind != "fix" && kind != "rebuttal" {
		return result, errors.New("发现处置类型必须为 fix 或 rebuttal")
	}
	if strings.TrimSpace(argument) == "" {
		return result, errors.New("发现处置必须提供具体说明")
	}
	if len(proof) > 0 {
		if err = e.validateCoordinationProof(w, run, proof); err != nil {
			return result, err
		}
	}
	id := "FR-" + domain.Digest([]string{w.ID, key, run.ID, findingID, kind, argument})[:24]
	err = e.Store.UpdateCoordination(store.WithCoordinationRun(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), run), workflowID, "finding_resolution_proposed", func(x *domain.Workflow) (any, error) {
		if err := checkCoordinationKey(*x, key); err != nil {
			return nil, err
		}
		if findCoordinationFinding(x, findingID) == nil {
			return nil, errors.New("发现处置引用了不存在的 finding")
		}
		for _, old := range x.FindingResolutions {
			if old.ID == id {
				result = old
				return nil, store.ErrNoChange
			}
		}
		assigned := false
		for _, task := range x.Tasks {
			if task.ID == run.TaskID {
				for _, id := range task.FindingIDs {
					assigned = assigned || id == findingID
				}
			}
		}
		if !assigned {
			return nil, errors.New("该实现任务未分配此 finding")
		}
		ids, err := appendCoordinationProof(x, proof)
		if err != nil {
			return nil, err
		}
		result = domain.FindingResolution{ID: id, FindingID: findingID, TaskID: run.TaskID, Kind: kind, Argument: argument, Evidence: ids, SourceVerificationKey: key, CandidateSHA: w.HeadSHA, VerificationKey: key, State: "PROPOSED"}
		x.FindingResolutions = append(x.FindingResolutions, result)
		return result, nil
	})
	return result, err
}
func applyFindingResolution(w *domain.Workflow, key, resolutionID string, run domain.AgentRun, decision string, proof []domain.Evidence) error {
	if len(proof) == 0 {
		return errors.New("独立复核必须提供实际证据")
	}
	if run.Role != "reviewer" && run.Role != "skeptic" {
		return errors.New("只有独立 reviewer 或 skeptic 可以确认发现结案")
	}
	if decision != "accepted" && decision != "rejected" && decision != "disputed" {
		return errors.New("复核结论必须为 accepted、rejected 或 disputed")
	}
	for i := range w.FindingResolutions {
		r := &w.FindingResolutions[i]
		if r.ID != resolutionID {
			continue
		}
		if r.VerificationKey != key || r.State != "PROPOSED" || r.TaskID == run.TaskID {
			return errors.New("处置记录已过期、已经复核或缺少独立任务隔离")
		}
		finding := findCoordinationFinding(w, r.FindingID)
		if finding == nil {
			return errors.New("处置关联的 finding 不存在")
		}
		if decision == "accepted" {
			for _, ev := range proof {
				if ev.Kind == "TOOL_VERIFIED" && ev.ExitCode != 0 {
					return errors.New("失败命令不能作为确认修复或反驳成立的证据")
				}
			}
		}
		ids, err := appendCoordinationProof(w, proof)
		if err != nil {
			return err
		}
		r.Evidence = append(r.Evidence, ids...)
		r.VerifiedBy = run.ID
		switch decision {
		case "accepted":
			r.State = "VERIFIED"
			finding.State = "RESOLVED"
			if r.Kind == "rebuttal" {
				finding.State = "REFUTED"
			}
			finding.VerificationKey = key
			finding.Evidence = append(finding.Evidence, ids...)
		case "rejected":
			r.State = "REJECTED"
			finding.State = "CONFIRMED"
		case "disputed":
			r.State = "DISPUTED"
			finding.State = "DISPUTED"
		}
		finding.History = append(finding.History, fmt.Sprintf("独立复核 %s：%s，处置记录 %s", run.ID, decision, r.ID))
		return nil
	}
	return errors.New("未找到发现处置记录")
}
func (e *Engine) VerifyFindingResolution(ctx context.Context, workflowID, key, resolutionID, reviewerRunID, decision string, proof []domain.Evidence) error {
	w, err := e.coordinationSnapshot(ctx, workflowID, key)
	if err != nil {
		return err
	}
	run, err := e.coordinationRun(ctx, w, reviewerRunID, "reviewer", "skeptic")
	if err != nil {
		return err
	}
	if err = e.validateCoordinationProof(w, run, proof); err != nil {
		return err
	}
	return e.Store.UpdateCoordination(store.WithCoordinationRun(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), run), workflowID, "finding_resolution_reviewed", func(x *domain.Workflow) (any, error) {
		if err := checkCoordinationKey(*x, key); err != nil {
			return nil, err
		}
		if x.Control != "ACTIVE" {
			return nil, errors.New("工作流已停止，不能接受晚到复核")
		}
		if err := applyFindingResolution(x, key, resolutionID, run, decision, proof); err != nil {
			return nil, err
		}
		return map[string]any{"resolution_id": resolutionID, "reviewer_run_id": run.ID, "decision": decision, "verification_key": key}, nil
	})
}

func applyFindingWaiver(w *domain.Workflow, waiver domain.Waiver, now time.Time) error {
	if strings.TrimSpace(waiver.Actor) == "" || strings.TrimSpace(waiver.Reason) == "" || !waiver.ExpiresAt.After(now) {
		return errors.New("豁免必须提供维护者、具体理由和未来的到期时间")
	}
	if err := checkCoordinationKey(*w, waiver.VerificationKey); err != nil {
		return err
	}
	finding := findCoordinationFinding(w, waiver.FindingID)
	if finding == nil || finding.VerificationKey != waiver.VerificationKey {
		return errors.New("豁免必须关联当前冻结候选的具体 finding")
	}
	if finding.State == "RESOLVED" || finding.State == "REFUTED" {
		return errors.New("已解决或已排除的 finding 无需豁免")
	}
	waiver.CreatedAt = now
	w.Waivers = append(w.Waivers, waiver)
	finding.State = "WAIVED"
	finding.History = append(finding.History, fmt.Sprintf("维护者 %s 豁免至 %s：%s", waiver.Actor, waiver.ExpiresAt.UTC().Format(time.RFC3339), waiver.Reason))
	return nil
}

// CurrentFindingWaiver checks an explicit WAIVED state plus an unexpired exact
// candidate waiver. Gate must call this for blocking WAIVED findings; an old
// record alone never changes coverage, verification or review completion.
func CurrentFindingWaiver(w domain.Workflow, finding domain.Finding, key string, now time.Time) bool {
	if finding.State != "WAIVED" || key == "" || w.Review.Key != key || finding.VerificationKey != key {
		return false
	}
	for _, waiver := range w.Waivers {
		if waiver.FindingID == finding.ID && waiver.VerificationKey == key && strings.TrimSpace(waiver.Actor) != "" && strings.TrimSpace(waiver.Reason) != "" && !waiver.CreatedAt.IsZero() && !waiver.CreatedAt.After(now) && waiver.ExpiresAt.After(now) {
			return true
		}
	}
	return false
}
func (e *Engine) GrantFindingWaiver(ctx context.Context, workflowID, actor, findingID, key, reason string, expiry time.Time, dashboard ...bool) error {
	w, err := e.coordinationSnapshot(ctx, workflowID, key)
	if err != nil {
		return err
	}
	if !e.authorized(ctx, w, actor, len(dashboard) > 0 && dashboard[0]) {
		return errors.New("只有已授权的仓库维护者可以豁免发现")
	}
	return e.Store.UpdateCoordination(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), workflowID, "finding_waived", func(x *domain.Workflow) (any, error) {
		waiver := domain.Waiver{FindingID: findingID, Actor: actor, Reason: reason, VerificationKey: key, ExpiresAt: expiry}
		if err := applyFindingWaiver(x, waiver, time.Now().UTC()); err != nil {
			return nil, err
		}
		return x.Waivers[len(x.Waivers)-1], nil
	})
}

// HandleWaiverCommand preserves identifier/reason case. Call before lowercasing
// the command in Engine.Command. Existing commands return handled=false.
func (e *Engine) HandleWaiverCommand(ctx context.Context, workflowID, actor, command string, dashboard ...bool) (bool, error) {
	trimmed := strings.TrimSpace(command)
	if len(trimmed) >= 4 && strings.EqualFold(trimmed[:4], "/ai ") {
		trimmed = strings.TrimSpace(trimmed[4:])
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "waive") {
		return false, nil
	}
	if len(fields) < 5 {
		return true, errors.New("用法：/ai waive <finding_id> <verification_key> <RFC3339到期时间> <理由>")
	}
	expiry, err := time.Parse(time.RFC3339, fields[3])
	if err != nil {
		return true, errors.New("豁免到期时间必须使用 RFC3339 格式")
	}
	return true, e.GrantFindingWaiver(ctx, workflowID, actor, fields[1], fields[2], strings.Join(fields[4:], " "), expiry, dashboard...)
}
