package server

import (
	"net/http"
	"strings"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
)

// Stopping runs retain capacity until the supervisor confirms process exit.
// Lease expiration alone must never make the dashboard advertise a free slot.
type agentQueueSummary struct {
	Queued   int `json:"queued"`
	Active   int `json:"active"`
	Stopping int `json:"stopping"`
	Occupied int `json:"occupied"`
	Limit    int `json:"limit"`
}

func summarizeAgentQueue(runs []domain.AgentRun, limit int) agentQueueSummary {
	summary := agentQueueSummary{Limit: limit}
	for _, run := range runs {
		switch run.Status {
		case domain.AgentQueued:
			summary.Queued++
		case domain.AgentActive:
			summary.Active++
		case domain.AgentStopping:
			summary.Stopping++
		}
	}
	summary.Occupied = summary.Active + summary.Stopping
	return summary
}
func runnerPublicConfig(cfg config.Config) map[string]any {
	return map[string]any{"mode": cfg.Runner.Mode, "image": cfg.Runner.Image, "builders": cfg.Runner.Builders, "reviewers": cfg.Runner.Reviewers, "workflow_concurrency": cfg.Runner.WorkflowConcurrency, "agent_concurrency": cfg.EffectiveAgentConcurrency(), "workflow_agent_concurrency": cfg.EffectiveWorkflowAgentConcurrency(), "max_tasks_per_workflow": cfg.EffectiveMaxTasksPerWorkflow(), "max_evidence_requests": cfg.EffectiveMaxEvidenceRequests(), "require_accept_for_analysis": cfg.Runner.RequireAcceptForAnalysis, "require_accept_for_development": cfg.Runner.RequireAcceptForDevelopment, "attempt_minutes": cfg.Runner.AttemptMinutes, "workspace_bytes": cfg.EffectiveWorkspaceBytes()}
}
func (h *Handlers) workflowDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "未知工作流地址")
		return
	}
	workflow, err := h.engine.Store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "未找到工作流")
		return
	}
	allRuns, err := h.engine.Store.ListAgentRuns(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法读取智能体队列")
		return
	}
	runs := []domain.AgentRun{}
	for _, run := range allRuns {
		if run.WorkflowID == id {
			runs = append(runs, run)
		}
	}
	cfg, err := h.engine.ExecutionConfig(r.Context())
	if err != nil {
		writeError(w, 503, "无法读取实际并发限额")
		return
	}
	workflowLimit := cfg.Runner.WorkflowConcurrency
	if workflowLimit == 0 {
		workflowLimit = 2
	}
	scheduling, err := h.engine.Store.SchedulingFor(r.Context(), []domain.Workflow{workflow}, workflowLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法读取工作流调度状态")
		return
	}
	view := workflowStateView{Workflow: workflow, AgentRuns: runs, AgentQueue: summarizeAgentQueue(runs, cfg.EffectiveWorkflowAgentConcurrency()), Scheduling: scheduling[id], ForceApprovalCurrent: h.engine.ForceApprovalCurrent(r.Context(), workflow), IssueMatchesApprovedSpec: h.engine.IssueMatchesApprovedSpec(workflow)}
	writeJSON(w, http.StatusOK, map[string]any{"workflow": view, "config": publicConfig(cfg), "agent_queue": summarizeAgentQueue(allRuns, cfg.EffectiveAgentConcurrency()), "prompt_version": agent.PromptVersion, "role_catalog": agent.RoleCatalog()})
}
