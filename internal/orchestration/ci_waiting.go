package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

// ErrTrustedCIPending 表示非终态等待，不能被解释为验证失败或允许合并。
var ErrTrustedCIPending = errors.New("等待可信 CI 回执")

type TrustedCIPendingError struct{ Reason string }

func (e *TrustedCIPendingError) Error() string        { return e.Reason }
func (e *TrustedCIPendingError) Is(target error) bool { return target == ErrTrustedCIPending }

// checkpointTrustedCI 必须在等待前保存固定候选以及已成功的本地验证证据。
func (e *Engine) checkpointTrustedCI(ctx context.Context, w *domain.Workflow, findings []domain.Finding) error {
	w.Phase = "WAITING_CI"
	w.LastError = "本地可信验证已完成，等待独立可信 CI 签发回执"
	w.Review = domain.Review{Key: e.key(ctx, *w), Findings: findings, Verdict: "WAITING_CI", VerificationPassed: false}
	return e.Store.Save(ctx, w, gateAction(*w, false, w.LastError), statusAction(*w))
}
func trustedVerificationActions(w *domain.Workflow, key string, findings []domain.Finding, verificationPassed bool) []store.Action {
	for i := range findings {
		f := &findings[i]
		if f.VerificationKey != key {
			f.History = append(f.History, "候选变化后重新验证："+f.VerificationKey)
			f.VerificationKey = key
			f.State = "FIX_CANDIDATE"
			f.Evidence = nil
			f.EvidenceType = "MODEL_CLAIM"
		}
	}
	w.Review = domain.Review{ID: fmt.Sprintf("%s-review-%d-%s", w.ID, w.Generation, w.HeadSHA), Key: key, Findings: findings, VerificationPassed: verificationPassed, Verdict: "RUNNING"}
	w.LastError = ""
	if evaluationProfile(*w) == "builder_ci" {
		w.Phase = "EVALUATION_COMPLETE"
		w.Review.Verdict = "PROFILE_PASS"
		if !verificationPassed {
			w.Review.Verdict = "PROFILE_FAIL"
		}
		w.Review.Reason = "候选已通过可信机械验证；该对照配置没有独立模型审查"
		if !verificationPassed {
			w.Review.Reason = "候选未通过可信机械验证；该对照配置没有独立模型审查"
			w.LastError = w.Review.Reason
		}
		return []store.Action{gateAction(*w, false, "评估结果仅用于对照记录，不授权生产合并"), statusAction(*w)}
	}
	w.Phase = "REVIEWING"
	return []store.Action{store.NewAction(*w, "ready", map[string]any{}), gateAction(*w, false, "独立审查进行中"), statusAction(*w)}
}
func (e *Engine) validWaitingCIChecks(w domain.Workflow, runs []domain.AgentRun) error {
	repo := e.repository(w)
	profile := domain.Digest([]any{repo.Verification, repo.VerificationServices, e.Config().EffectiveWorkspaceBytes(), agent.PromptVersion})
	if w.TestedSHA == "" || w.TestedTreeSHA == "" || len(repo.Verification) == 0 {
		return errors.New("CI 等待检查点缺少候选或可信命令")
	}
	stopped := map[string]bool{}
	for _, run := range runs {
		if run.Generation == w.Generation && run.Role == "verifier" && run.Status == domain.AgentFinished && !run.StoppedAt.IsZero() {
			stopped[run.ID] = true
		}
	}
	for n, command := range repo.Verification {
		found := false
		for _, ev := range w.Evidence {
			if !stopped[ev.AgentRunID] || ev.ID != fmt.Sprintf("verify:g%d:%d:%s", w.Generation, n, w.HeadSHA) || ev.Kind != "TRUSTED_CHECK" || ev.ExitCode != 0 || ev.HeadSHA != w.HeadSHA || ev.BaseSHA != w.BaseSHA || ev.TestedCommitSHA != w.TestedSHA || ev.TestedTreeSHA != w.TestedTreeSHA || ev.ProfileDigest != profile || domain.Digest(ev.Command) != domain.Digest(command) {
				continue
			}
			body, err := readRecordedEvidence(e.Config().DataDir, w.ID, ev)
			if err != nil {
				return err
			}
			var result sandbox.RunResult
			if json.Unmarshal(body, &result) != nil || result.ExitCode != 0 || result.TimedOut {
				return errors.New("CI 等待检查点的本地验证证据无效")
			}
			found = true
			break
		}
		if !found {
			return errors.New("CI 等待检查点缺少当前配置的成功验证证据")
		}
	}
	return nil
}

// recoverWaitingCI 只接续验证后段；等待期间没有模型调用、Agent 槽或测试重跑。
func (e *Engine) recoverWaitingCI(ctx context.Context) error {
	workflows, err := e.Store.List(ctx)
	if err != nil {
		return err
	}
	for _, w := range workflows {
		if w.Phase != "WAITING_CI" || w.Control != "ACTIVE" || w.LeaseUntil.After(time.Now()) {
			continue
		}
		runs, err := e.Store.ListAgentRuns(ctx, w.ID)
		if err != nil {
			return err
		}
		active := false
		for _, run := range runs {
			if run.Status == domain.AgentActive || run.Status == domain.AgentStopping || (!run.StartedAt.IsZero() && run.StoppedAt.IsZero()) {
				active = true
				break
			}
		}
		if active {
			continue
		}
		key := e.key(ctx, w)
		if w.Review.Key != key {
			err = e.Store.Update(ctx, w.ID, func(current *domain.Workflow) ([]store.Action, error) {
				if !sameCIWaitingCheckpoint(*current, w) {
					return nil, store.ErrStale
				}
				current.Phase = "VERIFYING"
				current.Review.Verdict = "OBSOLETE"
				current.LastError = "CI 等待期间候选或验证配置变化，需要重新验证"
				return []store.Action{statusAction(*current)}, nil
			})
			if err != nil && !errors.Is(err, store.ErrStale) {
				return err
			}
			continue
		}
		proofErr := e.checkEvaluationConfig(ctx, w)
		if proofErr == nil {
			proofErr = e.validWaitingCIChecks(w, runs)
		}
		var evidence *domain.Evidence
		if proofErr == nil {
			evidence, proofErr = e.trustedCIGate(ctx, w)
		}
		if errors.Is(proofErr, ErrTrustedCIPending) {
			continue
		}
		err = e.Store.Update(ctx, w.ID, func(current *domain.Workflow) ([]store.Action, error) {
			if !sameCIWaitingCheckpoint(*current, w) {
				return nil, store.ErrStale
			}
			if proofErr != nil {
				current.LastError = proofErr.Error()
				current.Phase = "BLOCKED"
				current.Review.Verdict = "INVALID"
				return []store.Action{gateAction(*current, false, current.LastError), statusAction(*current)}, nil
			}
			if evidence == nil {
				return nil, errors.New("等待中的可信 CI 策略缺失，不能静默放行")
			}
			current.Evidence = mergeEvidence(current.Evidence, []domain.Evidence{*evidence})
			if len(current.Review.Tasks) > 0 {
				current.Review.VerificationPassed = true
				current.Review.Verdict = "RUNNING"
				current.Phase = "REVIEWING"
				current.LastError = ""
				return []store.Action{statusAction(*current)}, nil
			}

			return trustedVerificationActions(current, key, append([]domain.Finding(nil), current.Review.Findings...), true), nil
		})
		if err != nil && !errors.Is(err, store.ErrStale) {
			return err
		}
	}
	return nil
}
func sameCIWaitingCheckpoint(a, b domain.Workflow) bool {
	return a.Phase == "WAITING_CI" && a.Control == "ACTIVE" && a.Version == b.Version && a.Generation == b.Generation && a.HeadSHA == b.HeadSHA && a.BaseSHA == b.BaseSHA && a.Spec.Digest == b.Spec.Digest && a.TestedSHA == b.TestedSHA && a.TestedTreeSHA == b.TestedTreeSHA && a.Review.Key == b.Review.Key && !a.LeaseUntil.After(time.Now())
}
