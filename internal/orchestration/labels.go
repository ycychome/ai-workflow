package orchestration

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
)

const workflowLabelPrefix = "ai-workflow:"

func workflowLabels(w domain.Workflow) []gh.Label {
	phases := map[string]string{
		"INTAKE": "待处理", "TRIAGE": "需求分析", "SPEC_READY": "待确认方案", "PLANNING": "规划中", "DEVELOPING": "开发中",
		"VERIFYING": "验证中", "REVIEWING": "审查中", "WAITING_CONFIGURATION": "等待配置", "NEEDS_INFO": "需要补充信息",
		"WAITING_CI": "等待可信CI", "WAITING_RISK": "等待风险授权", "WAITING_DAILY_BUDGET": "等待次日额度", "EVALUATION_COMPLETE": "评估已完成",
		"DEFERRED": "已暂缓", "REJECT_PROPOSED": "等待覆核的拒绝建议", "REJECTED": "已拒绝", "CHANGES_REQUESTED": "待修复", "BLOCKED": "受阻",
		"READY": "待远端检查", "AWAITING_MAINTAINER": "待维护者合并", "MERGED": "已合并",
	}
	controls := map[string]string{"PAUSING": "暂停中", "PAUSED": "已暂停", "CANCELLING": "取消中", "CANCELLED": "已取消", "TAKEOVER": "人工接管", "HUMAN_OWNED": "人工控制", "COMPLETED": "已完成"}
	name := phases[w.Phase]
	if name == "" {
		name = w.Phase
	}
	color := "1d76db"
	switch w.Phase {
	case "NEEDS_INFO", "SPEC_READY", "DEFERRED", "WAITING_CONFIGURATION", "REJECT_PROPOSED", "WAITING_CI", "WAITING_RISK", "WAITING_DAILY_BUDGET":
		color = "fbca04"
	case "BLOCKED", "CHANGES_REQUESTED", "REJECTED":
		color = "d73a4a"
	case "READY", "AWAITING_MAINTAINER":
		color = "0e8a16"
	case "MERGED":
		color = "8250df"
	}
	labels := []gh.Label{{Name: workflowLabelPrefix + name, Color: color, Description: "AI 工作流当前阶段：" + w.Phase}}
	if w.Control != "" && w.Control != "ACTIVE" {
		name = controls[w.Control]
		if name == "" {
			name = w.Control
		}
		labels = append(labels, gh.Label{Name: workflowLabelPrefix + name, Color: "6e7781", Description: "AI 工作流控制状态：" + w.Control})
	}
	return labels
}

// This loop never claims automatic workflow capacity. Informational effects
// remain independently retryable even when a publication needs reconciliation.
func (e *Engine) labelLoop(ctx context.Context) {
	repair := func() {
		if err := e.Store.QueueLabelRepair(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("queue GitHub state labels", "error", err)
		}
	}
	repair()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	nextRepair := time.Now().Add(5 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if time.Now().After(nextRepair) {
			repair()
			nextRepair = time.Now().Add(5 * time.Minute)
		}
		a, err := e.Store.ClaimLabelAction(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("claim GitHub state labels", "error", err)
			}
			continue
		}
		// Bound network effects below the outbox lease. On uncertain outcomes the
		// next attempt reads GitHub and applies the latest desired state again.
		effectCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		var receipt any
		if err = e.Store.StartAction(effectCtx, a); err == nil {
			w, getErr := e.Store.Get(effectCtx, a.WorkflowID)
			err = getErr
			if err == nil {
				receipt, err = e.syncWorkflowLabels(effectCtx, w)
			}
		}
		cancel()
		if finishErr := e.Store.FinishAction(context.WithoutCancel(ctx), a, receipt, err, outcomeUnknown(err)); finishErr != nil && ctx.Err() == nil {
			slog.Warn("finish GitHub state labels", "workflow", a.WorkflowID, "error", finishErr)
		}
	}
}

func (e *Engine) syncWorkflowLabels(ctx context.Context, w domain.Workflow) (any, error) {
	if w.Repository == "local/pilot" {
		return map[string]bool{"local": true}, nil
	}
	c, owner, repo, err := e.client(ctx, w)
	if err != nil {
		return nil, err
	}
	labels := workflowLabels(w)
	targets := []int{w.Issue}
	if w.PR > 0 && w.PR != w.Issue {
		targets = append(targets, w.PR)
	}
	for _, number := range targets {
		if err = c.SyncManagedLabels(ctx, owner, repo, number, workflowLabelPrefix, labels); err != nil {
			return nil, err
		}
	}
	return map[string]any{"targets": targets, "labels": labels}, nil
}
