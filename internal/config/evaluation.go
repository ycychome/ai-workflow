package config

import (
	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
)

// EvaluationExecutionConfig returns the frozen, non-secret execution contract.
func (c Config) EvaluationExecutionConfig() map[string]any {
	model := map[string]any{"dev": c.Model.Dev, "review": c.Model.Review, "stream": c.Model.Stream, "max_output_tokens": c.Model.MaxOutputTokens, "base_url": c.Model.BaseURL, "prompt_version": agent.PromptVersion, "runner_mode": c.Runner.Mode, "image": c.Runner.Image, "runner_policy": c.Runner, "repositories": c.Repositories, "github_app_id": c.GitHub.AppID, "installation_id": c.GitHub.InstallationID}
	return map[string]any{"model_profile": domain.Digest(model), "budget": map[string]any{"issue_calls": c.Budget.IssueCalls, "daily_calls": c.Budget.DailyCalls, "issue_minutes": c.Budget.IssueMinutes, "max_attempts": c.Budget.MaxAttempts, "max_fix_rounds": c.Budget.MaxFixRounds, "input_price_per_million": c.Budget.InputPrice, "output_price_per_million": c.Budget.OutputPrice}, "runner": map[string]any{"attempt_minutes": c.Runner.AttemptMinutes, "agent_concurrency": c.EffectiveAgentConcurrency(), "workflow_agent_concurrency": c.EffectiveWorkflowAgentConcurrency(), "max_tasks_per_workflow": c.EffectiveMaxTasksPerWorkflow(), "max_evidence_requests": c.EffectiveMaxEvidenceRequests()}}
}

func (c Config) EvaluationConfigDigest() string { return domain.Digest(c.EvaluationExecutionConfig()) }
