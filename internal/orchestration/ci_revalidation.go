package orchestration

import (
	"context"
	"errors"
	"strings"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

// 新 attempt 只能使合法固定来源进入等待；不能掩盖 workflow 来源被替换。
func validateWaitingCISource(ctx context.Context, client CIIdentityClient, p TrustedCIPolicy, c CICandidateIdentity) error {
	parts := strings.Split(p.Repository, "/")
	workflow, err := client.GetActionsWorkflow(ctx, parts[0], parts[1], p.WorkflowID)
	if err != nil || workflow.ID != p.WorkflowID || workflow.Path != p.WorkflowPath || workflow.State != "active" {
		return errors.New("可信 CI 工作流注册身份已失效")
	}
	for _, ref := range []string{p.SourceCommitSHA, c.BaseSHA, c.HeadSHA, c.TestMergeSHA} {
		source, err := client.GetRepositoryFileIdentity(ctx, parts[0], parts[1], p.WorkflowPath, ref)
		if err != nil || source.Type != "file" || source.Path != p.WorkflowPath || source.SHA != p.SourceBlobSHA {
			return errors.New("可信 CI 工作流来源已变化或无法验证")
		}
	}
	commit, err := client.GetCommitInfo(ctx, parts[0], parts[1], c.TestMergeSHA)
	if err != nil || commit.SHA != c.TestMergeSHA || commit.TreeSHA != c.TreeSHA || len(commit.Parents) != 2 || !((commit.Parents[0] == c.BaseSHA && commit.Parents[1] == c.HeadSHA) || (commit.Parents[1] == c.BaseSHA && commit.Parents[0] == c.HeadSHA)) {
		return errors.New("可信 CI 固定合并提交身份已失效")
	}
	return nil
}
func invalidateTrustedCIPass(w *domain.Workflow, err error) []store.Action {
	w.Review.VerificationPassed = false
	w.LastError = "可信 CI 重新核验未通过：" + err.Error()
	w.GateConclusion = "failure"
	w.GateStatus = "completed"
	w.Phase = "BLOCKED"
	w.Review.Verdict = "INVALID"
	if errors.Is(err, ErrTrustedCIPending) {
		w.Phase = "WAITING_CI"
		w.Review.Verdict = "WAITING_CI"
	}
	return []store.Action{gateAction(*w, false, w.LastError), statusAction(*w)}
}
func (e *Engine) recordTrustedCIDrift(ctx context.Context, w domain.Workflow, err error) error {
	return e.Store.Update(ctx, w.ID, func(current *domain.Workflow) ([]store.Action, error) {
		if current.Version != w.Version || current.Generation != w.Generation || current.HeadSHA != w.HeadSHA || current.Review.Verdict == "FORCED" {
			return nil, store.ErrStale
		}
		return invalidateTrustedCIPass(current, err), nil
	})
}
