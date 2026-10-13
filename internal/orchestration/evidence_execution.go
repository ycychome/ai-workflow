package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

type evidenceProposal struct {
	Role      string `json:"role"`
	Question  string `json:"question"`
	FindingID string `json:"finding_id"`
}

type evidenceFindingOption struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"`
	State       string `json:"state"`
	Description string `json:"description"`
}

func evidenceFindingOptions(w domain.Workflow) []evidenceFindingOption {
	options := make([]evidenceFindingOption, 0, len(w.Review.Findings))
	if w.Review.Key == "" {
		return options
	}
	for _, finding := range w.Review.Findings {
		if finding.ID == "" || finding.VerificationKey != w.Review.Key {
			continue
		}
		description := strings.TrimSpace(finding.Description)
		if len([]rune(description)) > 240 {
			description = string([]rune(description)[:240])
		}
		options = append(options, evidenceFindingOption{ID: finding.ID, Severity: finding.Severity, State: finding.State, Description: description})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].ID < options[j].ID })
	return options
}

func evidenceProposalSchema() map[string]any {
	return object(map[string]any{"role": map[string]any{"type": "string", "enum": []string{"verifier", "reviewer"}}, "question": str(), "finding_id": str()})
}

// validateEvidenceProposals is a pure preflight for the complete batch. The
// transactional checks in RequestEvidence remain authoritative when each
// request is persisted.
func validateEvidenceProposals(w domain.Workflow, proposals []evidenceProposal) error {
	allowed := map[string]bool{}
	ids := make([]string, 0, len(w.Review.Findings))
	if w.Review.Key != "" {
		for _, finding := range w.Review.Findings {
			if finding.ID == "" || finding.VerificationKey != w.Review.Key {
				continue
			}
			allowed[finding.ID] = true
			ids = append(ids, finding.ID)
		}
	}
	sort.Strings(ids)
	for i, proposal := range proposals {
		if err := validateEvidenceProposalFields(proposal); err != nil {
			return fmt.Errorf("补充证据请求 %d：%w", i+1, err)
		}
		if proposal.FindingID != "" && !allowed[proposal.FindingID] {
			return fmt.Errorf("补充证据请求 %d 引用了未知或过期发现 %q；当前验证身份可关联的发现：%s", i+1, proposal.FindingID, strings.Join(ids, ", "))
		}
	}
	return nil
}

func validateEvidenceProposalFields(proposal evidenceProposal) error {
	if proposal.Role != "verifier" && proposal.Role != "reviewer" {
		return errors.New("补充证据只能交由 verifier 或 reviewer 执行")
	}
	if strings.TrimSpace(proposal.Question) == "" {
		return errors.New("补充证据请求必须说明具体问题")
	}
	return nil
}

func (e *Engine) acceptEvidenceProposals(ctx context.Context, w *domain.Workflow, runID string, proposals []evidenceProposal) error {
	if err := validateEvidenceProposals(*w, proposals); err != nil {
		return err
	}
	for _, proposal := range proposals {
		if _, err := e.RequestEvidence(ctx, w.ID, w.Review.Key, runID, proposal.Role, proposal.Question, proposal.FindingID); err != nil {
			return err
		}
	}
	if len(proposals) > 0 {
		current, err := e.Store.Get(ctx, w.ID)
		if err != nil {
			return err
		}
		*w = current
	}
	return nil
}

// executeEvidenceRequests is an event-driven finite drain, not a second planner.
// Every intent is persisted before admission, and reclaims preserve its quota.
func (e *Engine) executeEvidenceRequests(ctx context.Context, w *domain.Workflow) error {
	for _, request := range w.EvidenceRequests {
		if request.Status == "RUNNING" && request.VerificationKey == w.Review.Key {
			deferred := false
			for _, task := range w.Review.Tasks {
				if task.ID == request.TaskID && task.BudgetDeferred {
					deferred = true
				}
			}
			if !deferred && request.Attempt >= e.Config().Budget.MaxAttempts {
				return fmt.Errorf("证据请求 %s 尝试次数已耗尽", request.ID)
			}
			if err := e.RequeueEvidenceRequest(ctx, w.ID, w.Review.Key, request.ID, request.Attempt); err != nil {
				return err
			}
		}
	}
	for {
		intent, err := e.ClaimEvidenceRequest(ctx, w.ID, w.Review.Key)
		if errors.Is(err, ErrNoEvidenceRequest) {
			current, readErr := e.Store.Get(ctx, w.ID)
			if readErr != nil {
				return readErr
			}
			*w = current
			return nil
		}
		if err != nil {
			return err
		}
		current, err := e.Store.Get(ctx, w.ID)
		if err != nil {
			return err
		}
		*w = current
		index := -1
		for i, task := range w.Review.Tasks {
			if task.ID == intent.Task.ID {
				index = i
			}
		}
		intent.Task.LeaseEpoch = 1
		if index >= 0 {
			intent.Task.LeaseEpoch = w.Review.Tasks[index].LeaseEpoch + 1
		}
		intent.Task.PlanRevision = planRevision(*w, true)
		if index < 0 {
			if w.TasksCreated >= e.Config().EffectiveMaxTasksPerWorkflow() {
				return errors.New("证据任务超过累计任务限额")
			}
			w.TasksCreated++
			w.Review.Tasks = append(w.Review.Tasks, intent.Task)
		} else {
			w.Review.Tasks[index] = intent.Task
		}
		if err = e.Store.Save(ctx, w); err != nil {
			return err
		}
		if err = e.executeEvidenceIntent(ctx, *w, intent); err != nil {
			// Bind/completion/budget checkpoint transactions advance the version.
			// Reload before execute's fail handler persists a resumable daily checkpoint.
			current, readErr := e.Store.Get(context.WithoutCancel(ctx), w.ID)
			if readErr != nil {
				return fmt.Errorf("%w; reload evidence checkpoint: %v", err, readErr)
			}
			*w = current
			return err
		}
		current, err = e.Store.Get(ctx, w.ID)
		if err != nil {
			return err
		}
		*w = current
	}
}

func (e *Engine) executeEvidenceIntent(ctx context.Context, w domain.Workflow, intent EvidenceIntent) (retErr error) {
	role := agent.RoleID(intent.Request.TargetRole)
	execution, err := e.beginAgent(ctx, w, intent.Task, role)
	if err != nil {
		return err
	}
	defer func() {
		if err := execution.finish(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	ctx = execution.ctx
	w = execution.w
	defer func() {
		if !errors.Is(retErr, store.ErrDailyBudget) {
			return
		}
		checkpointCtx, checkpointCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer checkpointCancel()
		checkpointErr := e.Store.UpdateCoordination(checkpointCtx, w.ID, "evidence_daily_budget_checkpoint", func(current *domain.Workflow) (any, error) {
			for i := range current.Review.Tasks {
				task := &current.Review.Tasks[i]
				if task.ID == execution.task.ID {
					if task.LeaseEpoch != execution.task.LeaseEpoch {
						return nil, store.ErrStale
					}
					task.BudgetDeferred = true
					task.Status = "QUEUED"
					return map[string]any{"task_id": task.ID, "agent_run_id": execution.run.ID, "attempt": task.Attempts}, nil
				}
			}
			return nil, store.ErrStale
		})
		if checkpointErr != nil {
			retErr = fmt.Errorf("%w; save daily evidence checkpoint: %v", retErr, checkpointErr)
		}
	}()
	if err = e.BindEvidenceRequestRun(ctx, w.ID, w.Review.Key, intent.Request.ID, execution.run.ID); err != nil {
		return err
	}
	files, err := e.reviewSnapshot(ctx, w)
	if err != nil {
		return err
	}
	workspace, err := execution.manager.CreateWithServices(ctx, execution.run.ID, files, e.repository(w).VerificationServices)
	if err != nil {
		return err
	}
	if err = execution.manager.SealReadOnly(ctx, workspace, role == agent.RoleReviewer); err != nil {
		return err
	}
	session := &toolSession{e: e, w: w, ctx: ctx, execution: execution, manager: execution.manager, workspace: workspace, review: true, snapshotSHA: w.TestedSHA, snapshotTreeSHA: w.TestedTreeSHA}
	session.baseFiles, err = e.baseSnapshot(ctx, w)
	if err != nil {
		return err
	}
	var report json.RawMessage
	if e.Config().Runner.Mode == "fake" {
		if err = session.fakePreflight(ctx); err != nil {
			return err
		}
		_, err = session.handler(ctx, "run", raw(map[string]any{"args": []string{"go", "test", "./..."}, "timeout_seconds": 300}))
		if err != nil {
			return err
		}
		report = raw(map[string]any{"summary": "已执行分配的补充检查"})
	} else {
		schema := nonCodeBuilderSchema()
		if role == agent.RoleReviewer {
			schema = reviewSchema()
		}
		report, err = session.ask(string(raw(map[string]any{"spec": w.Spec, "task": intent.Task, "evidence_request": intent.Request, "obligations": []domain.Obligation{}, "verification_key": w.Review.Key})), session.verificationTools(), schema)
		if err != nil {
			return err
		}
	}
	changes, err := execution.manager.CollectChanges(ctx, workspace)
	if err != nil {
		return err
	}
	if len(changes) > 0 {
		return errors.New("补充证据任务不得修改冻结候选")
	}
	proof := []domain.Evidence{}
	for _, ev := range session.listEvidence() {
		if role != agent.RoleVerifier || ev.Kind == "TOOL_VERIFIED" {
			proof = append(proof, ev)
		}
	}
	if len(proof) == 0 {
		return errors.New("补充证据任务没有产生有效的实际证据")
	}
	if err = e.finishTaskAttempt(ctx, execution.run.ID, w, execution.task, "ACCEPTED", report, session.listEvidence(), nil); err != nil {
		return err
	}
	if err = e.CompleteEvidenceRequest(ctx, w.ID, w.Review.Key, intent.Request.ID, execution.run.ID, proof); err != nil {
		return err
	}
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return err
	}
	current.Evidence = mergeEvidence(current.Evidence, session.listEvidence())
	for i, task := range current.Review.Tasks {
		if task.ID == intent.Task.ID {
			current.Review.Tasks[i].Status = "ACCEPTED"
			current.Review.Tasks[i].Report = report
			current.Review.Tasks[i].AgentRunID = execution.run.ID
			current.Review.Tasks[i].Preflight = session.preflight
		}
	}
	return e.Store.Save(ctx, &current)
}

func isEvidenceTask(task domain.Task) bool { return strings.HasPrefix(task.ID, "evidence-ER-") }

func completedRequestEvidence(w domain.Workflow) []domain.Evidence {
	ids := map[string]bool{}
	for _, request := range w.EvidenceRequests {
		if request.Status == "COMPLETED" && request.VerificationKey == w.Review.Key {
			for _, id := range request.Evidence {
				ids[id] = true
			}
		}
	}
	out := []domain.Evidence{}
	for _, ev := range w.Evidence {
		if ids[ev.ID] {
			out = append(out, ev)
		}
	}
	return out
}
