package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func validateDevPlanReport(output json.RawMessage) error {
	var report struct {
		Tasks []domain.Task `json:"tasks"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		return fmt.Errorf("解析开发计划失败：%w", err)
	}
	if err := validatePlannerTaskIDs(report.Tasks); err != nil {
		return err
	}
	return validatePlan(report.Tasks)
}

func validateReviewPlanReport(output json.RawMessage) error {
	var report struct {
		Tasks []domain.Task `json:"tasks"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		return fmt.Errorf("解析审查计划失败：%w", err)
	}
	if err := validatePlannerTaskIDs(report.Tasks); err != nil {
		return err
	}
	if err := validatePlan(report.Tasks); err != nil {
		return err
	}
	for _, task := range report.Tasks {
		if task.Kind != "verification" {
			return fmt.Errorf("审查任务 %s 的 kind 必须为 verification", task.ID)
		}
	}
	return nil
}

type triageReport struct {
	Decision    string             `json:"decision"`
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Risk        string             `json:"risk"`
	Criteria    []domain.Criterion `json:"criteria"`
	NonGoals    []string           `json:"non_goals"`
	Questions   []string           `json:"questions"`
}

func applyTriageReport(w *domain.Workflow, report triageReport, requireAcceptForDevelopment bool) error {
	switch report.Decision {
	case "accepted":
		if strings.TrimSpace(report.Title) == "" || strings.TrimSpace(report.Description) == "" || len(report.Criteria) == 0 {
			return errors.New("同意执行的分诊报告必须包含标题、描述和验收条件")
		}
		seen := map[string]bool{}
		for _, c := range report.Criteria {
			if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Description) == "" || seen[c.ID] {
				return errors.New("分诊验收条件无效")
			}
			seen[c.ID] = true
		}
		proposal := domain.Spec{Revision: w.Spec.Revision + 1, Title: report.Title, Description: report.Description, Criteria: report.Criteria, NonGoals: report.NonGoals, Risk: report.Risk, SourceHash: normalizedIssueSourceHash(w.IssueTitle, w.IssueBody)}
		change, err := domain.ProposeSpecChange(w, proposal, "ai:dev-triage", time.Now().UTC())
		if err != nil {
			return err
		}
		if requireAcceptForDevelopment {
			w.Phase = "SPEC_READY"
			w.LastError = "请审查提案规格，并发布 /ai accept 批准后开始开发"
			return nil
		}
		if err := domain.DecideSpecChange(w, change.ID, change.PreviousApprovedDigest, "accepted", "ai:dev-triage", "自动需求审批通过", time.Now().UTC()); err != nil {
			return err
		}
		w.Phase = "PLANNING"
		w.LastError = ""
	case "needs_info":
		w.ProposalSpec = nil
		w.Phase = "NEEDS_INFO"
		w.LastError = strings.Join(report.Questions, "\n")
	case "rejected":
		w.ProposalSpec = nil
		w.Phase = "REJECT_PROPOSED"
		w.LastError = ""
	case "deferred":
		w.ProposalSpec = nil
		w.Phase = "DEFERRED"
		w.LastError = ""
	default:
		return errors.New("不支持的分诊决定")
	}
	return nil
}

func (e *Engine) triage(ctx context.Context, w *domain.Workflow) (retErr error) {
	session, cleanup, err := e.plannerSession(ctx, w, "triage")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	ctx = session.ctx
	var report triageReport
	if e.Config().Runner.Mode == "fake" {
		if !strings.HasPrefix(w.IssueTitle, "Fixture:") {
			return errors.New("Fake 运行器只接受文档规定的 Fixture: repair Add 任务")
		}
		if err := session.fakePreflight(ctx); err != nil {
			return err
		}
		report.Decision = "accepted"
		report.Title = "修复计算器加法"
		report.Description = "使 Add 返回两个整数之和，保留已有接口和测试"
		report.Risk = "low"
		report.Criteria = []domain.Criterion{{ID: "AC-01", Description: "Add 返回两个整数之和且现有测试通过"}}
	} else {
		out, err := session.ask(string(raw(map[string]any{"title": w.IssueTitle, "body": w.IssueBody, "discussion": w.Discussion, "previous_spec": w.Spec})), session.readTools(), triageSchema())
		w.Evidence = append(w.Evidence, session.listEvidence()...)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(out, &report); err != nil {
			return err
		}
	}
	if err := applyTriageReport(w, report, e.Config().Runner.RequireAcceptForDevelopment); err != nil {
		return err
	}
	return e.Store.Save(ctx, w, statusAction(*w))
}
func (e *Engine) plan(ctx context.Context, w *domain.Workflow) (retErr error) {
	if evaluationProfile(*w) != "full" {
		return e.planEvaluationBuilder(ctx, w)
	}
	if !w.Spec.Approved {
		return errors.New("需要先批准任务规格")
	}
	session, cleanup, err := e.plannerSession(ctx, w, "dev-plan")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	ctx = session.ctx
	var report developmentPlanReport
	if e.Config().Runner.Mode == "fake" {
		if err := session.fakePreflight(ctx); err != nil {
			return err
		}
		report.ExpectedRevision = w.PlanRevision
		report.Reason = "安排确定性加法修复"
		needsRepair := len(w.Tasks) == 0 || w.Phase == "CHANGES_REQUESTED"
		for _, task := range w.Tasks {
			if task.Status == "RETRY_WAIT" || task.Status == "BLOCKED" {
				needsRepair = true
			}
		}
		if needsRepair {
			report.Tasks = []domain.Task{{ID: fmt.Sprintf("repair-add-r%d", w.PlanRevision+1), Kind: "code", Description: "修复 Add 使其返回 a + b", Paths: []string{"calculator.go"}}}
			for _, finding := range w.Review.Findings {
				report.Tasks[0].FindingIDs = append(report.Tasks[0].FindingIDs, finding.ID)
			}
			for _, task := range w.Tasks {
				if task.Status != "INTEGRATED" && task.Status != "SUPERSEDED" {
					report.Supersede = append(report.Supersede, task.ID)
				}
			}
		} else {
			report.Reason = "已检查任务完成事件，保留当前计划并交调度器继续"
		}
	} else {
		out, err := session.ask(string(raw(map[string]any{"spec": w.Spec, "expected_revision": w.PlanRevision, "current_tasks": w.Tasks, "integration_records": w.IntegrationRecords, "source_artifacts": w.SourceArtifacts, "last_error": w.LastError, "remaining_task_count": e.Config().EffectiveMaxTasksPerWorkflow() - w.TasksCreated, "findings": w.Review.Findings, "base_sha": w.BaseSHA, "head_sha": w.HeadSHA})), session.readTools(), planSchema(), validateDevelopmentDelta)
		w.Evidence = append(w.Evidence, session.listEvidence()...)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(out, &report); err != nil {
			return err
		}
	}
	if err := applyDevelopmentPlan(w, report, e.Config().EffectiveMaxTasksPerWorkflow()); err != nil {
		return err
	}
	w.Phase = "DEVELOPING"
	w.LastError = ""
	if w.BaseSHA == "" && w.Repository != "local/pilot" {
		c, o, r, err := e.client(ctx, *w)
		if err != nil {
			return err
		}
		sha, err := c.GetBranchSHA(ctx, o, r, w.BaseBranch)
		if err != nil {
			return err
		}
		w.BaseSHA = sha
	}
	if w.HeadSHA == "" {
		w.HeadSHA = w.BaseSHA
	}
	w.PlannedStateDigest = developmentStateDigest(*w)
	return e.Store.Save(ctx, w, statusAction(*w))
}

type buildResult struct {
	runID     string
	preflight *domain.Preflight
	artifact  *domain.SourceArtifact
	index     int
	changes   []sandbox.Change
	report    json.RawMessage
	evidence  []domain.Evidence
	err       error
}

func (e *Engine) build(ctx context.Context, w *domain.Workflow) error {
	if handled, err := e.resumeIntegration(ctx, w); handled {
		return err
	}
	// Publication is serial: retain the accepted artifact while its Outbox receipt is pending.
	for _, task := range w.Tasks {
		if task.Status == "VALIDATED" {
			return nil
		}
	}
	if evaluationProfile(*w) == "full" && len(w.Tasks) > 0 && w.PlannedStateDigest != developmentStateDigest(*w) {
		w.Phase = "PLANNING"
		return e.Store.Save(ctx, w, statusAction(*w))
	}
	cfg := e.Config()
	ready := readyTasks(w.Tasks, cfg.EffectiveMaxTasksPerWorkflow(), false)
	if len(ready) == 0 {
		all := true
		for _, t := range w.Tasks {
			if t.Status != "INTEGRATED" && t.Status != "SUPERSEDED" {
				all = false
			}
		}
		if !all {
			return errors.New("开发依赖图没有可执行任务")
		}
		w.Phase = "VERIFYING"
		return e.Store.Save(ctx, w, statusAction(*w))
	}
	var err error
	for _, idx := range ready {
		t := &w.Tasks[idx]
		if !t.BudgetDeferred && t.Attempts >= cfg.Budget.MaxAttempts {
			return fmt.Errorf("任务 %s 的尝试次数已耗尽", t.ID)
		}
		t.PlanRevision = planRevision(*w, false)
		if !t.BudgetDeferred {
			t.Attempts++
		}
		t.BudgetDeferred = false
		t.LeaseEpoch++
		t.Status = "QUEUED"
	}
	if err = e.Store.Save(ctx, w, statusAction(*w)); err != nil {
		return err
	}
	results := make(chan buildResult, len(ready))
	snapshot, err := cloneWorkflow(*w)
	if err != nil {
		return err
	}
	for _, idx := range ready {
		go func(i int) { results <- e.buildTask(ctx, snapshot, i, nil) }(idx)
	}
	var changes []gh.FileChange
	var proposals []resolutionProposal
	var integrationInputs []IntegrationInput
	ids := []string{}
	failed := false
	dailyDeferred := false
	var fatalErr error
	var reason string
	for range ready {
		res := <-results
		t := &w.Tasks[res.index]
		w.Evidence = append(w.Evidence, res.evidence...)
		t.Report = res.report
		t.AgentRunID = res.runID
		t.Preflight = res.preflight
		if res.err != nil {
			t.Status = "RETRY_WAIT"
			if errors.Is(res.err, store.ErrDailyBudget) {
				t.BudgetDeferred = true
				dailyDeferred = true
				continue
			}
			if errors.Is(res.err, store.ErrIssueBudget) {
				fatalErr = res.err
			}
			reason = res.err.Error()
			failed = true
			continue
		}
		if res.artifact != nil {
			w.SourceArtifacts = append(w.SourceArtifacts, *res.artifact)
			integrationInputs = append(integrationInputs, IntegrationInput{Task: *t, Artifact: *res.artifact, Changes: res.changes})
		}
		proposals = append(proposals, resolutionProposals(*t, res)...)
		if taskKind(*t) != "code" {
			if len(res.changes) != 0 {
				t.Status = "RETRY_WAIT"
				failed = true
				reason = "只读任务修改了源码快照"
				continue
			}
			t.Status = "INTEGRATED"
			t.IntegratedSHA = w.HeadSHA
			continue
		}
		if len(res.changes) == 0 {
			var report struct {
				Rebuttals []struct {
					FindingID string   `json:"finding_id"`
					Argument  string   `json:"argument"`
					Evidence  []string `json:"evidence"`
				} `json:"rebuttals"`
			}
			_ = json.Unmarshal(res.report, &report)
			if len(report.Rebuttals) > 0 {
				valid := true
				knownEvidence := map[string]bool{}
				for _, ev := range res.evidence {
					knownEvidence[ev.ID] = true
				}
				for _, rb := range report.Rebuttals {
					found := false
					for _, f := range w.Review.Findings {
						if f.ID == rb.FindingID {
							found = true
						}
					}
					assigned := false
					for _, id := range t.FindingIDs {
						if id == rb.FindingID {
							assigned = true
						}
					}
					if !found || !assigned || rb.Argument == "" || len(rb.Evidence) == 0 {
						valid = false
					}
					for _, id := range rb.Evidence {
						if !knownEvidence[id] {
							valid = false
						}
					}
				}
				if !valid {
					t.Status = "RETRY_WAIT"
					failed = true
					reason = "反驳缺少已知发现或独立记录的证据"
					continue
				}
				for _, rb := range report.Rebuttals {
					for i := range w.Review.Findings {
						f := &w.Review.Findings[i]
						if f.ID == rb.FindingID {
							f.History = append(f.History, "开发方反驳："+rb.Argument)
							f.State = "DISPUTED"
						}
					}
				}
				t.Status = "INTEGRATED"
				t.IntegratedSHA = w.HeadSHA
				continue
			}
			t.Status = "RETRY_WAIT"
			failed = true
			reason = "实现者没有返回实际源码产物"
			continue
		}
		for _, change := range res.changes {
			mode := "100644"
			if change.Mode&0111 != 0 {
				mode = "100755"
			}
			changes = append(changes, gh.FileChange{Path: change.Path, Content: change.Data, Delete: change.Deleted, Mode: mode})
		}
		t.Status = "VALIDATED"
		ids = append(ids, t.ID)
	}
	if failed {
		w.LastError = reason
		for _, t := range w.Tasks {
			if !t.BudgetDeferred && t.Status == "RETRY_WAIT" && t.Attempts >= cfg.Budget.MaxAttempts {
				return fmt.Errorf("任务尝试次数已耗尽：%s", reason)
			}
		}
	}
	if dailyDeferred || fatalErr != nil {
		if err := e.Store.Save(ctx, w, statusAction(*w)); err != nil {
			return err
		}
		if err := e.persistResolutionProposals(ctx, w, proposals); err != nil {
			return err
		}
		if fatalErr != nil {
			return fatalErr
		}
		return store.ErrDailyBudget
	}
	if len(changes) == 0 {
		if err := e.Store.Save(ctx, w, statusAction(*w)); err != nil {
			return err
		}
		return e.persistResolutionProposals(ctx, w, proposals)
	}
	if err := e.Store.Save(ctx, w); err != nil {
		return err
	}
	if err := e.persistResolutionProposals(ctx, w, proposals); err != nil {
		return err
	}
	return e.integrateAndPublish(ctx, w, integrationInputs, ids)
}

func (e *Engine) buildTask(ctx context.Context, w domain.Workflow, idx int, files map[string]sandbox.File) (res buildResult) {
	task := w.Tasks[idx]
	kind := taskKind(task)
	res = buildResult{index: idx}
	execution, err := e.beginAgent(ctx, w, task, taskRole(task, false))
	if err != nil {
		res.err = err
		return res
	}
	defer func() {
		if err := execution.finish(); err != nil && res.err == nil {
			res.err = err
		}
	}()
	ctx = execution.ctx
	w = execution.w
	task = execution.task
	id := execution.run.ID
	res.runID = id
	files, err = e.source(ctx, w, w.HeadSHA)
	if err != nil {
		res.err = err
		return res
	}
	var session *toolSession
	defer func() {
		if session != nil {
			res.evidence = mergeEvidence(res.evidence, session.listEvidence())
			res.preflight = session.preflight
		}
		state := "OUTPUT_READY"
		if res.err != nil {
			state = "BLOCKED"
		}
		if err := e.finishTaskAttempt(context.WithoutCancel(ctx), id, w, task, state, res.report, res.evidence, res.err); err != nil {
			if res.err == nil {
				res.err = err
			} else {
				res.err = fmt.Errorf("%w（同时无法保存尝试状态：%v）", res.err, err)
			}
		}
	}()
	if err := e.Store.AttemptOwned(ctx, id, w.ID, task.ID, w.Generation, task.LeaseEpoch, w.LeaseOwner, "RUNNING", nil, nil); err != nil {
		res.err = err
		return res
	}
	m := execution.manager
	workspace, err := m.CreateWithServices(ctx, id, files, e.repository(w).VerificationServices)
	if err != nil {
		res.err = err
		return res
	}
	session = &toolSession{e: e, w: w, workspace: workspace, manager: m, scope: task.Paths, ctx: ctx, execution: execution}
	if kind != "code" {
		if res.err = m.SealReadOnly(ctx, workspace, false); res.err != nil {
			return res
		}
		session.baseFiles, res.err = e.source(ctx, w, w.BaseSHA)
		if res.err != nil {
			return res
		}
	}
	if e.Config().Runner.Mode == "fake" {
		if res.err = session.fakePreflight(ctx); res.err != nil {
			return res
		}
		original := files["calculator.go"]
		original.Data = []byte(strings.Replace(string(original.Data), "return a - b", "return a + b", 1))
		res.err = m.WriteFile(ctx, workspace, "calculator.go", original)
		res.report = raw(map[string]any{"summary": "已修复确定性夹具的 Add 加法", "rebuttals": []any{}})
	} else {
		tools := session.tools()
		schema := builderSchema()
		if kind != "code" {
			if kind == "verification" {
				tools = session.verificationTools()
			} else {
				tools = session.readTools()
			}
			schema = nonCodeBuilderSchema()
		}
		res.report, res.err = session.ask(string(raw(map[string]any{"spec": w.Spec, "task": task, "integrated_sha": w.HeadSHA, "findings": w.Review.Findings, "dependencies": dependencyReports(w, task)})), tools, schema)
	}
	res.evidence = session.listEvidence()
	if res.err == nil {
		res.changes, res.err = m.CollectChanges(ctx, workspace)
	}
	if res.err == nil && kind != "code" {
		if len(res.changes) != 0 {
			res.err = errors.New("只读任务修改了工作区")
		} else {
			var report struct {
				Summary  string   `json:"summary"`
				Result   string   `json:"result"`
				Evidence []string `json:"evidence"`
			}
			if err := json.Unmarshal(res.report, &report); err != nil || strings.TrimSpace(report.Summary) == "" {
				res.err = errors.New("只读任务报告无效")
			} else if report.Result != "satisfied" && report.Result != "failed" && report.Result != "unverified" {
				res.err = errors.New("只读任务结果必须为 satisfied、failed 或 unverified")
			} else {
				known := map[string]domain.Evidence{}
				for _, ev := range session.listEvidence() {
					known[ev.ID] = ev
				}
				if len(report.Evidence) == 0 {
					res.err = errors.New("只读任务报告必须引用本次运行的证据")
				}
				hasSourceEvidence, hasSuccessfulCheck := false, false
				for _, id := range report.Evidence {
					ev, ok := known[id]
					if !ok {
						res.err = fmt.Errorf("只读任务引用了非本次运行产生的证据：%s", id)
						break
					}
					switch ev.Kind {
					case "SOURCE_ANALYSIS":
						hasSourceEvidence = true
					case "TOOL_VERIFIED":
						if ev.ExitCode != 0 {
							res.err = fmt.Errorf("验证报告引用了未成功的工具证据：%s", id)
							break
						}
						hasSuccessfulCheck = true
					default:
						res.err = fmt.Errorf("只读任务引用了不支持的证据类型 %s", ev.Kind)
					}
					if res.err != nil {
						break
					}
				}
				if res.err == nil && kind == "analysis" && !hasSourceEvidence {
					res.err = errors.New("分析任务必须引用实际源码分析证据")
				}
				if res.err == nil && report.Result != "satisfied" {
					res.err = errors.New("非代码任务没有报告 satisfied")
				}
				if res.err == nil && kind == "verification" && !hasSuccessfulCheck {
					res.err = errors.New("验证任务必须引用本次运行的成功工具检查")
				}
				if res.err == nil {
					ev, err := e.artifact(context.WithoutCancel(ctx), w, id+":non-code", "NON_CODE_ARTIFACT", map[string]any{"task_id": task.ID, "kind": kind, "head_sha": w.HeadSHA, "report": report, "evidence_ids": report.Evidence})
					if err != nil {
						res.err = err
					} else {
						res.evidence = append(res.evidence, ev)
					}
				}
			}
		}
	}
	if res.err == nil {
		for _, c := range res.changes {
			if !allowedPath(c.Path, e.repository(w), task.Paths) {
				res.err = fmt.Errorf("实际产物超出任务范围：%s", c.Path)
				break
			}
		}
	}
	if res.err == nil && len(res.changes) > 0 {
		ev, err := e.artifact(context.WithoutCancel(ctx), w, id+":source", "SOURCE_ARTIFACT", map[string]any{"generation": w.Generation, "lease_epoch": task.LeaseEpoch, "base_sha": w.HeadSHA, "changes": res.changes})
		if err != nil {
			res.err = err
		} else {
			ev.AgentRunID = execution.run.ID
			res.evidence = append(res.evidence, ev)
			res.artifact = &domain.SourceArtifact{ID: "source-" + domain.Digest([]any{execution.run.ID, ev.Hash})[:24], TaskID: task.ID, AgentRunID: execution.run.ID, Generation: w.Generation, PlanRevision: execution.run.PlanRevision, SpecDigest: w.Spec.Digest, ParentSHA: w.HeadSHA, ChangesDigest: domain.Digest(res.changes), EvidenceID: ev.ID, CreatedAt: ev.CreatedAt}
		}
	}
	return res
}

func (e *Engine) finishTaskAttempt(ctx context.Context, id string, w domain.Workflow, task domain.Task, state string, report any, evidence []domain.Evidence, runErr error) error {
	storedReport := report
	if runErr != nil {
		state = "BLOCKED"
		if errors.Is(runErr, store.ErrDailyBudget) {
			state = "WAITING_DAILY_BUDGET"
		}
		storedReport = map[string]any{"report": report, "error": e.redact(runErr.Error())}
	}
	if err := e.Store.AttemptOwned(ctx, id, w.ID, task.ID, w.Generation, task.LeaseEpoch, w.LeaseOwner, state, storedReport, evidence); err != nil {
		if runErr != nil {
			return err
		}
		failure := fmt.Errorf("保存成功尝试状态失败：%w", err)
		blockedReport := map[string]any{"report": report, "error": e.redact(failure.Error())}
		if fallbackErr := e.Store.AttemptOwned(ctx, id, w.ID, task.ID, w.Generation, task.LeaseEpoch, w.LeaseOwner, "BLOCKED", blockedReport, evidence); fallbackErr != nil {
			return fmt.Errorf("%w；保存阻塞状态失败：%v", failure, fallbackErr)
		}
		return failure
	}
	return nil
}

func mergeEvidence(existing, additional []domain.Evidence) []domain.Evidence {
	seen := make(map[string]bool, len(existing)+len(additional))
	out := make([]domain.Evidence, 0, len(existing)+len(additional))
	for _, ev := range append(append([]domain.Evidence(nil), existing...), additional...) {
		if ev.ID != "" && seen[ev.ID] {
			continue
		}
		seen[ev.ID] = true
		out = append(out, ev)
	}
	return out
}

func (e *Engine) verify(ctx context.Context, w *domain.Workflow) (retErr error) {
	controllerCtx := ctx
	execution, err := e.beginAgent(ctx, *w, domain.Task{ID: "trusted-verification", Attempts: 1, Paths: []string{}, DependsOn: []string{}}, agent.RoleVerifier)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, execution.finish()) }()
	ctx = execution.ctx
	*w = execution.w
	files, pending, err := e.verificationSnapshot(ctx, w)
	if err != nil {
		return err
	}
	if pending {
		w.LastError = "GitHub 正在计算当前测试合并提交"
		return e.Store.Save(ctx, w, statusAction(*w))
	}
	changes, riskErr := e.CurrentRiskChanges(ctx, *w)
	if riskErr != nil {
		return riskErr
	}
	if riskErr = e.CheckRiskAuthorization(ctx, *w, changes); riskErr != nil {
		if !errors.Is(riskErr, ErrRiskAuthorizationRequired) {
			return riskErr
		}
		w.Phase = "WAITING_RISK"
		w.LastError = riskErr.Error()
		w.Review.Verdict = "BLOCKED"
		w.Review.Key = e.key(ctx, *w)
		w.Review.VerificationPassed = false
		return e.Store.Save(ctx, w, gateAction(*w, false, w.LastError), statusAction(*w))
	}
	m := execution.manager
	workspace, err := m.CreateWithServices(ctx, execution.run.ID, files, e.repository(*w).VerificationServices)
	if err != nil {
		return err
	}
	if err = m.SealReadOnly(ctx, workspace, false); err != nil {
		return err
	}
	r := e.repository(*w)
	previousFindings := append([]domain.Finding(nil), w.Review.Findings...)
	verificationPassed := true
	for n, command := range r.Verification {
		result, runErr := m.Run(ctx, workspace, command, e.Config().AttemptTimeout())
		result.Stdout = e.redact(result.Stdout)
		result.Stderr = e.redact(result.Stderr)
		code := result.ExitCode
		if runErr != nil {
			code = -1
		}
		ev, err := e.artifact(context.WithoutCancel(ctx), *w, fmt.Sprintf("verify:g%d:%d:%s", w.Generation, n, w.HeadSHA), "TRUSTED_CHECK", result)
		if err != nil {
			return err
		}
		ev.AgentRunID = execution.run.ID
		ev.Command = command
		ev.ExitCode = code
		ev.TestedCommitSHA = w.TestedSHA
		ev.TestedTreeSHA = w.TestedTreeSHA
		w.Evidence = append(w.Evidence, ev)
		if code == 0 {
			for i := range previousFindings {
				f := &previousFindings[i]
				if f.ID == "mechanical-"+fmt.Sprint(n) {
					f.State = "RESOLVED"
					f.Evidence = []string{ev.ID}
					f.VerificationKey = e.key(ctx, *w)
					f.History = append(f.History, "受信检查已在此提交通过："+w.TestedSHA)
				}
			}
		}
		if code != 0 {
			if runErr != nil || result.TimedOut || strings.Contains(result.Stdout+result.Stderr, "module lookup disabled by GOPROXY=off") {
				return errors.New("验证基础设施未完成执行；恢复前请修复或预热运行器镜像")
			}
			w.Review = domain.Review{Key: e.key(ctx, *w), Verdict: "CHANGES_REQUESTED", Reason: "受信验证未通过"}
			w.LastError = "受信验证未通过: " + strings.Join(command, " ") + "\n" + result.Stdout + result.Stderr
			if w.Evaluation != nil && w.Evaluation.Kind == "review" {
				verificationPassed = false
				previousFindings = append(previousFindings, domain.Finding{ID: "mechanical-" + fmt.Sprint(n), Severity: "high", Category: "test", Description: "冻结候选未通过可信验证", Expected: "可信命令退出码为 0", Actual: fmt.Sprintf("命令退出码为 %d", code), Preconditions: "当前冻结候选及可信验证配置", EvidenceType: "TOOL_VERIFIED", Evidence: []string{ev.ID}, State: "CONFIRMED", VerificationKey: e.key(ctx, *w)})
				continue
			}
			if w.FixRounds >= e.Config().Budget.MaxFixRounds {
				return errors.New(w.LastError)
			}
			w.FixRounds++
			w.Review.Findings = append(previousFindings, domain.Finding{ID: "mechanical-" + fmt.Sprint(n), Severity: "high", Category: "test", Description: w.LastError, Expected: "受信命令退出码为 0", Actual: fmt.Sprintf("命令退出码为 %d", code), Preconditions: "当前固定候选使用受信验证配置", EvidenceType: "TOOL_VERIFIED", Evidence: []string{ev.ID}, State: "CONFIRMED", VerificationKey: w.Review.Key})
			w.Phase = "CHANGES_REQUESTED"
			return e.Store.Save(ctx, w, gateAction(*w, false, w.LastError), statusAction(*w))
		}
	}
	if err := execution.finish(); err != nil {
		return err
	}
	ctx = controllerCtx
	if w.Evaluation != nil && w.Evaluation.Kind == "review" && !verificationPassed {
		return e.Store.Save(ctx, w, trustedVerificationActions(w, e.key(ctx, *w), previousFindings, false)...)
	}
	if e.repository(*w).TrustedCI != nil {
		if err := e.checkpointTrustedCI(ctx, w, previousFindings); err != nil {
			return err
		}
	}
	ciEvidence, ciErr := e.trustedCIGate(ctx, *w)
	if errors.Is(ciErr, ErrTrustedCIPending) {
		return nil
	}
	if ciErr != nil {
		if w.Phase == "WAITING_CI" {
			w.LastError = ciErr.Error()
			return e.Store.Save(ctx, w, statusAction(*w))
		}
		return ciErr
	}
	if ciEvidence != nil {
		w.Evidence = mergeEvidence(w.Evidence, []domain.Evidence{*ciEvidence})
	}
	return e.Store.Save(ctx, w, trustedVerificationActions(w, e.key(ctx, *w), previousFindings, verificationPassed)...)

}
func mandatory(w domain.Workflow) []domain.Obligation {
	var out []domain.Obligation
	for _, c := range w.Spec.Criteria {
		out = append(out, domain.Obligation{ID: c.ID, CriterionID: c.ID, Description: c.Description, Result: "unverified", Evidence: []string{}})
	}
	for _, x := range [][2]string{{"SEC-01", "权限与信任边界得到保留"}, {"TEST-01", "受信测试及测试变更已审查"}, {"COMP-01", "受影响的公开接口保持兼容"}} {
		out = append(out, domain.Obligation{ID: x[0], Description: x[1], Result: "unverified", Evidence: []string{}})
	}
	return out
}
func (e *Engine) review(ctx context.Context, w *domain.Workflow) error {
	key := e.key(ctx, *w)
	pairedReview := w.Evaluation != nil && w.Evaluation.Kind == "review"
	if w.Review.Key != key || (!w.Review.VerificationPassed && !pairedReview) || w.TestedSHA == "" || w.TestedTreeSHA == "" {
		w.Phase = "VERIFYING"
		return e.Store.Save(ctx, w, gateAction(*w, false, "审查快照已变化"))
	}
	files, err := e.reviewSnapshot(ctx, *w)
	if err != nil {
		return err
	}
	baseline := mandatory(*w)
	if len(w.Review.Tasks) == 0 {
		var requests []evidenceProposal
		var plannerRunID string
		if evaluationProfile(*w) == "single_review" {
			w.Review.Obligations = baseline
			task := domain.Task{ID: "single-review", Kind: "verification", Description: "独立检查当前冻结候选的全部验收义务", Paths: []string{"**"}, Status: "QUEUED"}
			for _, obligation := range baseline {
				task.ObligationIDs = append(task.ObligationIDs, obligation.ID)
			}
			w.Review.Tasks = []domain.Task{task}
		} else if e.Config().Runner.Mode == "fake" {
			session, cleanup, err := e.plannerSession(ctx, w, "review-plan")
			if err != nil {
				return err
			}
			if err = session.fakePreflight(session.ctx); err != nil {
				cleanup()
				return err
			}
			w.Evidence = mergeEvidence(w.Evidence, session.listEvidence())
			if err := cleanup(); err != nil {
				return err
			}
			w.Review.Obligations = baseline
			for i, o := range baseline {
				w.Review.Tasks = append(w.Review.Tasks, domain.Task{ID: fmt.Sprintf("review-%d", i+1), Kind: "verification", Description: "独立验证分配的验收义务", Paths: []string{"**"}, Status: "QUEUED", ObligationIDs: []string{o.ID}})
			}
		} else {
			session, cleanup, err := e.plannerSession(ctx, w, "review-plan")
			if err != nil {
				return err
			}
			output, askErr := session.ask(string(raw(map[string]any{"spec": w.Spec, "mandatory_obligations": baseline, "head_sha": w.HeadSHA, "tested_commit_sha": w.TestedSHA, "tested_tree_sha": w.TestedTreeSHA})), session.readTools(), reviewPlanSchema(), validateReviewPlanReport)
			// The planner result is fenced and recorded before stopping its run.
			if askErr == nil {
				askErr = e.Store.AttemptOwned(session.ctx, session.execution.run.ID, w.ID, "review-plan", w.Generation, session.execution.run.LeaseEpoch, w.LeaseOwner, "OUTPUT_READY", output, session.listEvidence())
			}
			askErr = errors.Join(askErr, cleanup())
			w.Evidence = append(w.Evidence, session.listEvidence()...)
			if askErr != nil {
				return askErr
			}
			var plan struct {
				Requests    []evidenceProposal  `json:"evidence_requests"`
				Tasks       []domain.Task       `json:"tasks"`
				Obligations []domain.Obligation `json:"obligations"`
			}
			if err = json.Unmarshal(output, &plan); err != nil {
				return err
			}
			if err = validateReviewPlanReport(output); err != nil {
				return err
			}
			requests = plan.Requests
			plannerRunID = session.execution.run.ID
			w.Review.Tasks = plan.Tasks
			w.Review.Obligations = baseline
			for i := range w.Review.Tasks {
				if w.Review.Tasks[i].Kind == "" {
					w.Review.Tasks[i].Kind = "verification"
				}
				w.Review.Tasks[i].Status = "QUEUED"
			}
			known := map[string]bool{}
			for _, o := range baseline {
				known[o.ID] = true
			}
			for _, o := range plan.Obligations {
				if !known[o.ID] {
					o.Result = "unverified"
					o.Evidence = nil
					w.Review.Obligations = append(w.Review.Obligations, o)
					known[o.ID] = true
				}
			}
			assigned := map[string]bool{}
			for _, t := range w.Review.Tasks {
				for _, id := range t.ObligationIDs {
					if !known[id] {
						return errors.New("审查计划引用了未知义务")
					}
					assigned[id] = true
				}
			}
			for id := range known {
				if !assigned[id] {
					return fmt.Errorf("强制审查义务 %s 尚未分配", id)
				}
			}
		}
		if w.TasksCreated+len(w.Review.Tasks) > e.Config().EffectiveMaxTasksPerWorkflow() {
			return errors.New("审查计划超过累计任务限额")
		}
		w.TasksCreated += len(w.Review.Tasks)
		w.Review.PlanRevision++
		for i := range w.Review.Tasks {
			w.Review.Tasks[i].Role = string(agent.RoleReviewer)
			w.Review.Tasks[i].PlanRevision = w.Review.PlanRevision
		}
		if err = e.Store.Save(ctx, w, statusAction(*w)); err != nil {
			return err
		}
		if err = e.acceptEvidenceProposals(ctx, w, plannerRunID, requests); err != nil {
			return err
		}
	}
	if err = e.executeEvidenceRequests(ctx, w); err != nil {
		return err
	}
	for {
		ready := readyTasks(w.Review.Tasks, e.Config().EffectiveMaxTasksPerWorkflow(), true)
		if len(ready) == 0 {
			all := true
			for _, t := range w.Review.Tasks {
				if t.ID != "skeptic" && t.Status != "SUPERSEDED" && t.Status != "ACCEPTED" {
					all = false
				}
			}
			if !all {
				return errors.New("审查依赖图没有可执行任务")
			}
			break
		}
		var mu sync.Mutex
		type rr struct {
			idx      int
			report   reviewReport
			evidence []domain.Evidence
			err      error
		}
		ch := make(chan rr, len(ready))
		for _, idx := range ready {
			t := &w.Review.Tasks[idx]
			if !t.BudgetDeferred && t.Attempts >= e.Config().Budget.MaxAttempts {
				return errors.New("审查任务尝试次数已耗尽")
			}
			if !t.BudgetDeferred {
				t.Attempts++
			}
			t.BudgetDeferred = false
			t.PlanRevision = planRevision(*w, true)
			t.LeaseEpoch++
			t.Status = "QUEUED"
		}
		if err = e.Store.Save(ctx, w, statusAction(*w)); err != nil {
			return err
		}
		snapshot, err := cloneWorkflow(*w)
		if err != nil {
			return err
		}
		for _, idx := range ready {
			go func(i int) {
				report, ev, err := e.reviewTask(ctx, snapshot, snapshot.Review.Tasks[i], files, false)
				mu.Lock()
				ch <- rr{i, report, ev, err}
				mu.Unlock()
			}(idx)
		}
		var batchErr error
		for range ready {
			r := <-ch
			t := &w.Review.Tasks[r.idx]
			w.Evidence = append(w.Evidence, r.evidence...)
			if r.err != nil {
				t.Status = "RETRY_WAIT"
				t.BudgetDeferred = errors.Is(r.err, store.ErrDailyBudget)
				if batchErr == nil || errors.Is(batchErr, store.ErrDailyBudget) {
					batchErr = r.err
				}
				continue
			}
			t.Report = raw(r.report)
			if len(r.evidence) > 0 {
				t.AgentRunID = r.evidence[0].AgentRunID
			}
			if err = mergeReportWithMechanical(w, r.report, t.ObligationIDs, r.evidence, false, e.reviewerMechanicalEvidence(*w)); err != nil {
				t.Status = "BLOCKED"
				batchErr = err
				continue
			}
			t.Status = "ACCEPTED"
		}
		if err = e.Store.Save(ctx, w, statusAction(*w)); err != nil {
			return err
		}
		if batchErr != nil {
			return batchErr
		}
	}
	if evaluationProfile(*w) == "single_review" {
		for _, task := range w.Review.Tasks {
			if task.Role != string(agent.RoleReviewer) || task.Status != "ACCEPTED" {
				continue
			}
			var report reviewReport
			if err := json.Unmarshal(task.Report, &report); err != nil {
				return err
			}
			proof := []domain.Evidence{}
			for _, ev := range w.Evidence {
				if ev.AgentRunID == task.AgentRunID {
					proof = append(proof, ev)
				}
			}
			if err := e.verifyResolutionReport(ctx, w, task.AgentRunID, report, proof); err != nil {
				return err
			}
		}
		return e.finishSingleReview(ctx, w)
	}
	if !w.Review.SkepticComplete {
		task := domain.Task{ID: "skeptic", Role: string(agent.RoleSkeptic), Kind: "verification", Description: "复核发现、反证及覆盖遗漏", Paths: []string{"**"}, Status: "QUEUED", Attempts: 1, LeaseEpoch: 1, PlanRevision: planRevision(*w, true)}
		prior := -1
		for i, t := range w.Review.Tasks {
			if t.ID == "skeptic" {
				prior = i
				task.Attempts = t.Attempts + 1
				if t.BudgetDeferred {
					task.Attempts = t.Attempts
				}
				task.LeaseEpoch = t.LeaseEpoch + 1
			}
		}
		if task.Attempts > e.Config().Budget.MaxAttempts {
			return errors.New("质疑复核任务尝试次数已耗尽")
		}
		for _, o := range w.Review.Obligations {
			task.ObligationIDs = append(task.ObligationIDs, o.ID)
		}
		if prior < 0 {
			if w.TasksCreated >= e.Config().EffectiveMaxTasksPerWorkflow() {
				return errors.New("累计任务数已达上限")
			}
			w.TasksCreated++
			w.Review.Tasks = append(w.Review.Tasks, task)
			prior = len(w.Review.Tasks) - 1
		} else {
			w.Review.Tasks[prior] = task
		}
		if err := e.Store.Save(ctx, w); err != nil {
			return err
		}
		report, ev, err := e.reviewTask(ctx, *w, task, nil, true)
		w.Evidence = append(w.Evidence, ev...)
		if err != nil {
			if errors.Is(err, store.ErrDailyBudget) {
				w.Review.Tasks[prior].BudgetDeferred = true
				w.Review.Tasks[prior].Status = "RETRY_WAIT"
				if saveErr := e.Store.Save(ctx, w); saveErr != nil {
					return saveErr
				}
			}
			return err
		}
		if err = mergeReport(w, report, task.ObligationIDs, ev, true); err != nil {
			return err
		}
		w.Review.Tasks[prior].Status = "ACCEPTED"
		w.Review.Tasks[prior].Report = raw(report)
		if len(ev) > 0 {
			w.Review.Tasks[prior].AgentRunID = ev[0].AgentRunID
		}
		if len(report.EvidenceRequests) > 0 {
			w.Review.SkepticComplete = false
			if err := e.Store.Save(ctx, w); err != nil {
				return err
			}
			if err := e.acceptEvidenceProposals(ctx, w, w.Review.Tasks[prior].AgentRunID, report.EvidenceRequests); err != nil {
				return err
			}
			return e.executeEvidenceRequests(ctx, w)
		}
		w.Review.SkepticComplete = true
		if err := e.Store.Save(ctx, w); err != nil {
			return err
		}
		if err := e.verifyResolutionReport(ctx, w, w.Review.Tasks[prior].AgentRunID, report, ev); err != nil {
			return err
		}
	}
	pass, reason := domain.Gate(*w, e.key(ctx, *w))
	if w.Evaluation != nil && (pass || w.Evaluation.Kind == "review") {
		return e.finishEvaluation(ctx, w, pass, reason)
	}
	w.Review.Reason = reason
	if pass {
		w.Review.Verdict = "PASS"
		w.Phase = "AWAITING_MAINTAINER"
		w.LastError = ""
	} else {
		w.Review.Verdict = "CHANGES_REQUESTED"
		w.LastError = reason
		blocking := false
		for _, f := range w.Review.Findings {
			if f.State != "RESOLVED" && f.State != "REFUTED" && (f.Severity == "critical" || f.Severity == "high" || f.Severity == "medium") {
				blocking = true
			}
		}
		if !blocking || w.FixRounds >= e.Config().Budget.MaxFixRounds {
			w.Phase = "BLOCKED"
		} else {
			w.FixRounds++
			w.Phase = "CHANGES_REQUESTED"
		}
	}
	return e.Store.Save(ctx, w, gateAction(*w, pass, reason), statusAction(*w))
}

type reviewReport struct {
	EvidenceRequests []evidenceProposal  `json:"evidence_requests"`
	Obligations      []domain.Obligation `json:"obligations"`
	Findings         []domain.Finding    `json:"findings"`
	Summary          string              `json:"summary"`
}

// An independent reviewer receives obligation definitions, including when its
// DAG predecessors already completed the same obligation in an earlier batch.
// Only the skeptic receives their recorded conclusions and evidence references.
func reviewTaskObligations(w domain.Workflow, task domain.Task, skeptic bool) []domain.Obligation {
	var obligations []domain.Obligation
	for _, obligation := range w.Review.Obligations {
		for _, id := range task.ObligationIDs {
			if obligation.ID != id {
				continue
			}
			if skeptic {
				obligation.Evidence = append([]string(nil), obligation.Evidence...)
			} else {
				obligation.Result = "unverified"
				obligation.Evidence = nil
			}
			obligations = append(obligations, obligation)
		}
	}
	return obligations
}

// Controller checks are shared factual inputs, unlike peer model conclusions.
// Match every frozen identity before granting access to their recorded bytes.
func (e *Engine) reviewerMechanicalEvidence(w domain.Workflow) []domain.Evidence {
	profile := domain.Digest([]any{e.repository(w).Verification, e.repository(w).VerificationServices, e.Config().EffectiveWorkspaceBytes(), agent.PromptVersion})
	return currentReviewerMechanicalEvidence(w, profile)
}
func currentReviewerMechanicalEvidence(w domain.Workflow, profile string) []domain.Evidence {
	var out []domain.Evidence
	if w.TestedSHA == "" || w.TestedTreeSHA == "" || profile == "" {
		return out
	}
	for _, ev := range w.Evidence {
		if (ev.Kind == "TRUSTED_CHECK" || ev.Kind == "TRUSTED_CI") && ev.ID != "" && ev.Hash != "" && ev.ProfileDigest == profile && ev.HeadSHA == w.HeadSHA && ev.BaseSHA == w.BaseSHA && ev.TestedCommitSHA == w.TestedSHA && ev.TestedTreeSHA == w.TestedTreeSHA {
			out = append(out, ev)
		}
	}
	return out
}

func (e *Engine) reviewTask(ctx context.Context, w domain.Workflow, task domain.Task, files map[string]sandbox.File, skeptic bool) (report reviewReport, ev []domain.Evidence, retErr error) {
	role := agent.RoleReviewer
	if skeptic {
		role = agent.RoleSkeptic
	}
	execution, err := e.beginAgent(ctx, w, task, role)
	if err != nil {
		return report, nil, err
	}
	defer func() {
		if err := execution.finish(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	ctx = execution.ctx
	w = execution.w
	task = execution.task
	id := execution.run.ID
	files, err = e.reviewSnapshot(ctx, w)
	if err != nil {
		return report, nil, err
	}
	var session *toolSession
	defer func() {
		if session != nil {
			ev = mergeEvidence(ev, session.listEvidence())
		}
		if err := e.finishTaskAttempt(context.WithoutCancel(ctx), id, w, task, "ACCEPTED", report, ev, retErr); err != nil {
			if retErr == nil {
				retErr = err
			} else {
				retErr = fmt.Errorf("%w（同时无法保存尝试状态：%v）", retErr, err)
			}
		}
	}()
	if err := e.Store.AttemptOwned(ctx, id, w.ID, task.ID, w.Generation, task.LeaseEpoch, w.LeaseOwner, "RUNNING", nil, nil); err != nil {
		return reviewReport{}, nil, err
	}
	m := execution.manager
	workspace, err := m.CreateWithServices(ctx, id, files, e.repository(w).VerificationServices)
	if err != nil {
		return reviewReport{}, nil, err
	}
	if err = m.SealReadOnly(ctx, workspace, true); err != nil {
		return reviewReport{}, nil, err
	}
	session = &toolSession{e: e, w: w, workspace: workspace, manager: m, review: true, snapshotSHA: w.TestedSHA, snapshotTreeSHA: w.TestedTreeSHA, ctx: ctx, execution: execution}
	baseFiles, err := e.baseSnapshot(ctx, w)
	if err != nil {
		return reviewReport{}, nil, err
	}
	session.baseFiles = baseFiles
	obligations := reviewTaskObligations(w, task, skeptic)
	report = reviewReport{Findings: []domain.Finding{}}
	if e.Config().Runner.Mode == "fake" {
		if err := session.fakePreflight(ctx); err != nil {
			return report, nil, err
		}
		output, err := session.handler(ctx, "read_file", raw(map[string]string{"path": "calculator.go"}))
		if err != nil {
			return report, nil, err
		}
		if !strings.Contains(output, "a + b") {
			return report, nil, errors.New("夹具审查发现 Add 实现不正确")
		}
		ev := session.listEvidence()
		for _, o := range obligations {
			o.Result = "satisfied"
			o.Evidence = []string{ev[len(ev)-1].ID}
			report.Obligations = append(report.Obligations, o)
		}
		report.Summary = "确定性夹具源码审查已完成"
	} else {
		input := map[string]any{"spec": w.Spec, "task": task, "obligations": obligations, "verification_key": w.Review.Key, "prior_findings": independentPriorFindings(w)}
		mechanical := e.reviewerMechanicalEvidence(w)
		session.readableEvidence = mechanical
		input["verification_evidence"] = evidenceSummaries(mechanical)
		if skeptic {
			input["peer_reports"] = w.Review.Tasks
			input["findings"] = w.Review.Findings
			input["finding_resolutions"] = w.FindingResolutions
			session.readableEvidence = mergeEvidence(mechanical, mergeEvidence(skepticEvidence(w), completedRequestEvidence(w)))
			input["evidence_requests"] = w.EvidenceRequests
			input["evidence"] = evidenceSummaries(session.readableEvidence)
		}
		validate := func(output json.RawMessage) error {
			var candidate reviewReport
			if err := json.Unmarshal(output, &candidate); err != nil {
				return err
			}
			// Validation must not mutate the accepted ledger before the report is returned.
			var preview domain.Workflow
			if err := json.Unmarshal(raw(w), &preview); err != nil {
				return err
			}
			err := mergeReportWithMechanical(&preview, candidate, task.ObligationIDs, session.listEvidence(), skeptic, mechanical)
			if err != nil {
				e.Store.Audit(context.WithoutCancel(ctx), w.ID, "report_rejected", map[string]any{"generation": w.Generation, "task_id": task.ID, "error": e.redact(err.Error())})
			}
			return err
		}
		out, err := session.ask(string(raw(input)), session.tools(), reviewSchema(), validate)
		if err != nil {
			return report, session.listEvidence(), err
		}
		if err = json.Unmarshal(out, &report); err != nil {
			return report, session.listEvidence(), err
		}
	}
	changes, err := m.CollectChanges(ctx, workspace)
	if err != nil {
		return report, session.listEvidence(), err
	}
	for _, change := range changes {
		if !strings.HasPrefix(change.Path, "review_tests/") {
			return report, session.listEvidence(), fmt.Errorf("审查修改了候选源码 %s", change.Path)
		}
	}
	ev = session.listEvidence()
	return report, ev, nil
}
func mergeReport(w *domain.Workflow, report reviewReport, assigned []string, evidence []domain.Evidence, skeptic bool) error {
	return mergeReportWithMechanical(w, report, assigned, evidence, skeptic, nil)
}
func mergeReportWithMechanical(w *domain.Workflow, report reviewReport, assigned []string, evidence []domain.Evidence, skeptic bool, mechanical []domain.Evidence) error {
	canResolve := skeptic || evaluationProfile(*w) == "single_review"
	if !skeptic && len(report.EvidenceRequests) > 0 {
		return errors.New("独立审查者不能自行扩展团队；补充请求由审查规划者或质疑复核者提出")
	}
	valid := map[string]domain.Evidence{}
	for _, ev := range evidence {
		valid[ev.ID] = ev
	}
	mechanicalSuccess := map[string]bool{}
	for _, ev := range mechanical {
		valid[ev.ID] = ev
		mechanicalSuccess[ev.ID] = ev.ExitCode == 0
	}
	if skeptic {
		for _, ev := range w.Evidence {
			if ev.HeadSHA == w.HeadSHA && ev.BaseSHA == w.BaseSHA {
				valid[ev.ID] = ev
			}
		}
	}
	allowed := map[string]bool{}
	for _, id := range assigned {
		allowed[id] = true
	}
	seen := map[string]bool{}
	for _, o := range report.Obligations {
		if !allowed[o.ID] || seen[o.ID] {
			return fmt.Errorf("审查报告包含未分配或重复义务 %q；每个已分配 ID 必须恰好返回一次", o.ID)
		}
		seen[o.ID] = true
		if o.Result != "satisfied" && o.Result != "failed" && o.Result != "unverified" {
			return errors.New("审查覆盖状态无效")
		}
		if o.Result == "satisfied" && len(o.Evidence) == 0 {
			return errors.New("标记 satisfied 的义务没有证据")
		}
		positiveEvidence := false
		for _, id := range o.Evidence {
			ev, exists := valid[id]
			if !exists {
				return fmt.Errorf("审查引用了不存在的证据 %s", id)
			}
			positiveEvidence = positiveEvidence || evidenceSupportsSuccess(ev) || mechanicalSuccess[id]
		}
		if o.Result == "satisfied" && !positiveEvidence {
			return errors.New("标记 satisfied 的义务需要源码分析或成功工具结果")
		}
		for i := range w.Review.Obligations {
			if w.Review.Obligations[i].ID == o.ID {
				w.Review.Obligations[i].Result = o.Result
				w.Review.Obligations[i].Evidence = o.Evidence
			}
		}
	}
	for id := range allowed {
		if !seen[id] {
			return fmt.Errorf("审查遗漏义务 %s", id)
		}
	}
	for _, f := range report.Findings {
		if err := domain.ValidateFindingEnums(f, false); err != nil {
			return err
		}
		if !canResolve || f.ID == "" {
			f.ID = "F-" + domain.Digest([]any{f.Path, f.Line, f.Description})[:10]
		}
		if f.Description == "" || f.Expected == "" || f.Actual == "" || f.Preconditions == "" {
			return errors.New("审查发现缺少假设、前提或预期与实际行为")
		}
		for _, id := range f.Evidence {
			if _, exists := valid[id]; !exists {
				return errors.New("审查发现引用的证据未记录")
			}
		}
		if f.EvidenceType != "MODEL_CLAIM" && len(f.Evidence) == 0 {
			return errors.New("审查发现声称使用了未记录的源码或工具证据")
		}
		if f.State == "REFUTED" || f.State == "RESOLVED" {
			if !canResolve || !hasIndependentCounterevidence(f, domain.Finding{}, evidence) {
				return errors.New("只能用独立证据反驳审查发现")
			}
		}
		f.VerificationKey = w.Review.Key
		found := false
		for i := range w.Review.Findings {
			if w.Review.Findings[i].ID == f.ID {
				prior := w.Review.Findings[i]
				if findingSeverityRank(f.Severity) < findingSeverityRank(prior.Severity) && prior.State != "REFUTED" && prior.State != "RESOLVED" {
					// A severity downgrade can clear the gate just as a resolution can.
					// Parallel reviewers cannot erase one another's blocking findings.
					if !canResolve || !hasIndependentCounterevidence(f, prior, evidence) {
						return errors.New("降低审查发现严重程度需要独立源码或工具反证")
					}
				}
				f.History = append(w.Review.Findings[i].History, "独立审查："+report.Summary)
				w.Review.Findings[i] = f
				found = true
			}
		}
		if !found {
			w.Review.Findings = append(w.Review.Findings, f)
		}
	}
	return nil
}

// Source snapshots used by local demo publishing are persisted like real artifacts.
func loadLocalSnapshot(directory string) (map[string]sandbox.File, error) {
	b, err := os.ReadFile(directory + "/snapshot.json")
	if os.IsNotExist(err) {
		return localFiles(), nil
	}
	if err != nil {
		return nil, err
	}
	var files map[string]sandbox.File
	err = json.Unmarshal(b, &files)
	return files, err
}
func sortedChanges(changes []gh.FileChange) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
}

func findingSeverityRank(severity string) int {
	switch severity {
	case "critical":
		return 5
	case "high":
		return 4
	case "medium":
		return 3
	case "low":
		return 2
	case "info":
		return 1
	}
	return 6 // Unknown persisted severity must not be weakened implicitly.
}

func hasIndependentCounterevidence(f, prior domain.Finding, evidence []domain.Evidence) bool {
	if f.EvidenceType == "MODEL_CLAIM" {
		return false
	}
	priorIDs := map[string]bool{}
	for _, id := range prior.Evidence {
		priorIDs[id] = true
	}
	for _, id := range f.Evidence {
		if priorIDs[id] {
			continue
		}
		for _, ev := range evidence {
			if ev.ID == id && evidenceSupportsSuccess(ev) {
				return true
			}
		}
	}
	return false
}

func evidenceSupportsSuccess(ev domain.Evidence) bool {
	return ev.Kind == "SOURCE_ANALYSIS" || (ev.Kind == "TOOL_VERIFIED" && ev.ExitCode == 0)
}
