package orchestration

import (
	"errors"
	"github.com/ycychome/ai-workflow/internal/domain"
	"testing"
	"time"
)

func TestTrustedCIPendingIsNotFailure(t *testing.T) {
	err := &TrustedCIPendingError{Reason: "等待回执"}
	if !errors.Is(err, ErrTrustedCIPending) || err.Error() != "等待回执" {
		t.Fatal("CI 等待必须具有独立类型")
	}
}
func TestCIWaitingCheckpointRejectsConcurrentResume(t *testing.T) {
	w := domain.Workflow{ID: "workflow", Version: 7, Generation: 2, Phase: "WAITING_CI", Control: "ACTIVE", HeadSHA: "head", BaseSHA: "base", TestedSHA: "merge", TestedTreeSHA: "tree", Spec: domain.Spec{Digest: "spec"}, Review: domain.Review{Key: "key"}}
	if !sameCIWaitingCheckpoint(w, w) {
		t.Fatal("相同检查点应可恢复")
	}
	for _, change := range []func(*domain.Workflow){
		func(x *domain.Workflow) { x.Version++ }, func(x *domain.Workflow) { x.Generation++ }, func(x *domain.Workflow) { x.Phase = "REVIEWING" }, func(x *domain.Workflow) { x.Control = "PAUSED" }, func(x *domain.Workflow) { x.TestedTreeSHA = "new" }, func(x *domain.Workflow) { x.LeaseUntil = time.Now().Add(time.Minute) },
	} {
		current := w
		change(&current)
		if sameCIWaitingCheckpoint(current, w) {
			t.Fatal("变化或被占用的检查点不能复苏")
		}
	}
}
func TestTrustedVerificationResumePreservesProfiles(t *testing.T) {
	full := domain.Workflow{ID: "workflow", Phase: "WAITING_CI", Review: domain.Review{Key: "key"}}
	actions := trustedVerificationActions(&full, "key", nil, true)
	if full.Phase != "REVIEWING" || !full.Review.VerificationPassed || len(actions) != 3 {
		t.Fatal("完整团队应接续独立审查")
	}
	ablated := domain.Workflow{ID: "workflow", Phase: "WAITING_CI", Evaluation: &domain.EvaluationContext{Profile: "builder_ci"}}
	actions = trustedVerificationActions(&ablated, "key", nil, true)
	if ablated.Phase != "EVALUATION_COMPLETE" || ablated.Review.Verdict != "PROFILE_PASS" || len(actions) != 2 {
		t.Fatal("Builder CI 对照应结束评估并保持生产门禁关闭")
	}
}

func TestFrozenReviewKeepsFailedMechanicalStatus(t *testing.T) {
	w := domain.Workflow{ID: "review", Evaluation: &domain.EvaluationContext{Kind: "review", Profile: "full"}}
	trustedVerificationActions(&w, "key", nil, false)
	if w.Phase != "REVIEWING" || w.Review.VerificationPassed {
		t.Fatal("冻结审查应继续评估且保留机械验证失败")
	}
}

func TestTrustedCIDriftSeparatesWaitingFromInvalid(t *testing.T) {
	for _, tc := range []struct {
		err   error
		phase string
	}{{&TrustedCIPendingError{Reason: "需要新回执"}, "WAITING_CI"}, {errors.New("工作流来源被替换"), "BLOCKED"}} {
		w := domain.Workflow{Phase: "AWAITING_MAINTAINER", Review: domain.Review{VerificationPassed: true, Verdict: "PASS"}, GateConclusion: "success"}
		actions := invalidateTrustedCIPass(&w, tc.err)
		if w.Phase != tc.phase || w.Review.VerificationPassed || w.GateConclusion != "failure" || len(actions) != 2 {
			t.Fatal("CI身份变化必须撤绿并区分可等待状态")
		}
	}
}
