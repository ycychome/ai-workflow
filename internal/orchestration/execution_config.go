package orchestration

import (
	"context"
	"errors"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
)

// ExecutionConfig includes the cross-process admission limits actually used by
// the queue, rather than a possibly older controller's in-memory copy.
func (e *Engine) ExecutionConfig(ctx context.Context) (config.Config, error) {
	cfg := e.Config()
	global, workflow, err := e.Store.AgentLimits(ctx)
	if err != nil {
		return cfg, err
	}
	cfg.Runner.AgentConcurrency = global
	cfg.Runner.WorkflowAgentConcurrency = workflow
	return cfg, nil
}

func (e *Engine) checkEvaluationConfig(ctx context.Context, w domain.Workflow) error {
	if w.Evaluation == nil {
		return nil
	}
	cfg, err := e.ExecutionConfig(ctx)
	if err != nil {
		return err
	}
	if cfg.EvaluationConfigDigest() != w.Evaluation.ConfigDigest {
		return errors.New("评估执行配置已变化；本轮结果不能与冻结配置混用")
	}
	return nil
}
