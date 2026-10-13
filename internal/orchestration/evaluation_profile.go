package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/store"
)

func evaluationProfile(w domain.Workflow) string {
	if w.Evaluation == nil {
		return "full"
	}
	return w.Evaluation.Profile
}

// Both ablations use exactly one Builder at a time and no model Planner. The
// approved specification is its complete contract; normal tool/path fences and
// cumulative budgets still apply.
func (e *Engine) planEvaluationBuilder(ctx context.Context, w *domain.Workflow) error {
	if !w.Spec.Approved {
		return errors.New("评估实现任务需要已批准规格")
	}
	task := domain.Task{ID: fmt.Sprintf("single-builder-r%d", w.PlanRevision+1), Kind: "code", Role: string(agent.RoleBuilder), Description: "实现已批准规格，并以实际源码产物交付全部验收要求", Paths: append([]string{}, e.repository(*w).AllowedPaths...)}
	if len(task.Paths) == 0 {
		return errors.New("评估仓库没有可写范围")
	}
	report := developmentPlanReport{ExpectedRevision: w.PlanRevision, Reason: "按冻结对照配置安排一个实现者", Tasks: []domain.Task{task}}
	for _, finding := range w.Review.Findings {
		if finding.State != "RESOLVED" && finding.State != "REFUTED" {
			report.Tasks[0].FindingIDs = append(report.Tasks[0].FindingIDs, finding.ID)
		}
	}
	for _, prior := range w.Tasks {
		if prior.Status != "INTEGRATED" && prior.Status != "SUPERSEDED" {
			report.Supersede = append(report.Supersede, prior.ID)
		}
	}
	if err := applyDevelopmentPlan(w, report, e.Config().EffectiveMaxTasksPerWorkflow()); err != nil {
		return err
	}
	w.Phase = "DEVELOPING"
	w.LastError = ""
	return e.Store.Save(ctx, w, statusAction(*w))
}

func (e *Engine) finishSingleReview(ctx context.Context, w *domain.Workflow) error {
	passed, reason := singleReviewPassed(*w)
	if passed || w.Evaluation.Kind == "review" || w.FixRounds >= e.Config().Budget.MaxFixRounds {
		return e.finishEvaluation(ctx, w, passed, reason)
	}
	blocking := false
	for _, finding := range w.Review.Findings {
		if findingSeverityRank(finding.Severity) >= findingSeverityRank("medium") && finding.State != "RESOLVED" && finding.State != "REFUTED" {
			blocking = true
		}
	}
	if !blocking {
		return e.finishEvaluation(ctx, w, false, reason)
	}
	w.FixRounds++
	w.Phase = "CHANGES_REQUESTED"
	w.Review.Verdict = "CHANGES_REQUESTED"
	w.LastError = reason
	return e.Store.Save(ctx, w, gateAction(*w, false, reason), statusAction(*w))
}
func (e *Engine) CreateEvaluationWorkflow(ctx context.Context, repo config.Repository, installationID int64, issue gh.Issue, evaluation domain.EvaluationContext, spec *domain.Spec, pr *gh.PullRequest) (domain.Workflow, error) {
	if !domain.ValidEvaluationProfile(evaluation.Profile) || evaluation.TrialID == "" || evaluation.ConfigDigest == "" || evaluation.ManifestDigest == "" || evaluation.BaseSHA == "" || evaluation.BaseBranch == "" {
		return domain.Workflow{}, errors.New("评估上下文缺少受信冻结身份")
	}
	if evaluation.Kind != "review" && evaluation.Kind != "development" && evaluation.Kind != "smoke" {
		return domain.Workflow{}, errors.New("未知评估种类")
	}
	if !strings.HasPrefix(strings.TrimSpace(issue.Body), "<!-- ai-workflow-evaluation-trial:"+evaluation.TrialID+" -->") {
		return domain.Workflow{}, errors.New("评估 Issue 与已预留 trial 不匹配")
	}
	w := issueIntake(repo, installationID, issue, false)
	w.Evaluation = &evaluation
	w.AuthorizedBy = "evaluation-api"
	w.Phase = "TRIAGE"
	w.BaseBranch = evaluation.BaseBranch
	w.BaseSHA = evaluation.BaseSHA
	w.HeadSHA = evaluation.BaseSHA
	if evaluation.Kind == "review" {
		if spec == nil || pr == nil || pr.Number <= 0 || evaluation.CandidateSHA == "" || pr.Head.SHA != evaluation.CandidateSHA || pr.Base.SHA != evaluation.BaseSHA {
			return w, errors.New("成对审查评估需要冻结规格和匹配候选的真实 PR")
		}
		if spec.SourceHash != domain.IssueSourceHash(issue.Title, issue.Body) {
			return w, errors.New("评估规格未绑定冻结题面")
		}
		candidateSpec := *spec
		candidateSpec.Revision = 1
		candidateSpec.Approved = false
		candidateSpec.ApprovedBy = ""
		proposal, err := domain.ProposeSpecChange(&w, candidateSpec, "evaluation-api", time.Now().UTC())
		if err != nil {
			return w, err
		}
		if err = domain.DecideSpecChange(&w, proposal.ID, proposal.PreviousApprovedDigest, "accepted", "evaluation-api", "按冻结题面进行独立评估", time.Now().UTC()); err != nil {
			return w, err
		}
		w.PR = pr.Number
		w.HeadSHA = evaluation.CandidateSHA
		w.Branch = pr.Head.Ref
		w.Phase = "VERIFYING"
	}
	if _, err := e.Store.CreateIntake(ctx, w, statusAction); err != nil {
		return w, err
	}
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return w, err
	}
	if current.Evaluation == nil || domain.Digest(*current.Evaluation) != domain.Digest(evaluation) {
		return w, errors.New("评估工作流已经存在且冻结身份不一致")
	}
	return current, nil
}

func (e *Engine) finishEvaluation(ctx context.Context, w *domain.Workflow, passed bool, reason string) error {
	if w.Evaluation == nil {
		return errors.New("缺少评估上下文")
	}
	w.Phase = "EVALUATION_COMPLETE"
	w.Review.Verdict = "PROFILE_FAIL"
	if passed {
		w.Review.Verdict = "PROFILE_PASS"
	}
	w.Review.Reason = reason
	w.LastError = ""
	if !passed {
		w.LastError = reason
	}
	// Ablations never publish the production merge-gate as success. A trial
	// outcome is distinct from human-labelled correctness and human merging.
	return e.Store.Save(ctx, w, gateAction(*w, false, "评估结果仅用于对照记录，不授权生产合并"), statusAction(*w))
}

func singleReviewPassed(w domain.Workflow) (bool, string) {
	if !w.Review.VerificationPassed || len(w.Review.Tasks) != 1 || w.Review.Tasks[0].Status != "ACCEPTED" {
		return false, "单审查者流程尚未完成"
	}
	for _, ob := range w.Review.Obligations {
		if ob.Result != "satisfied" || len(ob.Evidence) == 0 {
			return false, "审查义务未验证：" + ob.ID
		}
	}
	for _, finding := range w.Review.Findings {
		if findingSeverityRank(finding.Severity) >= findingSeverityRank("medium") && finding.State != "RESOLVED" && finding.State != "REFUTED" {
			return false, "审查发现未解决：" + finding.ID
		}
	}
	return true, "可信验证及单个独立审查者完成当前候选检查"
}

func (e *Engine) ResumeDailyBudget(ctx context.Context, id string) error {
	w, err := e.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	if w.Phase != "WAITING_DAILY_BUDGET" {
		return errors.New("工作流不是按日额度等待状态")
	}
	if err := e.checkEvaluationConfig(ctx, w); err != nil {
		return err
	}
	remaining, err := e.Store.DailyRequestRemaining(ctx, e.Config().Budget.DailyCalls)
	if err != nil {
		return err
	}
	if remaining <= 0 {
		return store.ErrDailyBudget
	}
	if w.ModelCalls >= e.Config().Budget.IssueCalls {
		return store.ErrIssueBudget
	}
	return e.Store.Update(ctx, id, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Phase != "WAITING_DAILY_BUDGET" || x.Control != "ACTIVE" {
			return nil, store.ErrStale
		}
		if x.ResumePhase == "" {
			return nil, errors.New("按日暂停缺少恢复阶段")
		}
		x.Phase = x.ResumePhase
		x.ResumePhase = ""
		x.LastError = ""
		return []store.Action{statusAction(*x)}, nil
	})
}

func (e *Engine) resumeDailyBudgetWorkflows(ctx context.Context) {
	remaining, err := e.Store.DailyRequestRemaining(ctx, e.Config().Budget.DailyCalls)
	if err != nil || remaining <= 0 {
		return
	}
	workflows, err := e.Store.List(ctx)
	if err != nil {
		return
	}
	for _, w := range workflows {
		if w.Control == "ACTIVE" && w.Phase == "WAITING_DAILY_BUDGET" {
			_ = e.ResumeDailyBudget(ctx, w.ID)
		}
	}
}

func profileFailureReason(w domain.Workflow, reason string) string {
	return fmt.Sprintf("评估配置 %s：%s", evaluationProfile(w), reason)
}
