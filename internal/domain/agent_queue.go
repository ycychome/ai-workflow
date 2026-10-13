package domain

import "time"

const (
	AgentQueued   = "QUEUED"
	AgentActive   = "ACTIVE"
	AgentStopping = "STOPPING"
	AgentFinished = "FINISHED"
	AgentObsolete = "OBSOLETE"
)

// AgentRun is one model session. STOPPING still consumes capacity until the
// supervisor confirms that the process has exited; lease expiry is not exit.
type AgentRun struct {
	ID            string    `json:"id"`
	WorkflowID    string    `json:"workflow_id"`
	TaskID        string    `json:"task_id"`
	Role          string    `json:"role"`
	Generation    int64     `json:"generation"`
	PlanRevision  int       `json:"plan_revision"`
	LeaseEpoch    int64     `json:"lease_epoch"`
	Owner         string    `json:"owner"`
	Status        string    `json:"status"`
	EnqueuedAt    time.Time `json:"enqueued_at"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	HeartbeatAt   time.Time `json:"heartbeat_at,omitempty"`
	StoppedAt     time.Time `json:"stopped_at,omitempty"`
	LeaseUntil    time.Time `json:"lease_until,omitempty"`
	QueueReason   string    `json:"queue_reason,omitempty"`
	ContextHash   string    `json:"context_hash"`
	PromptVersion string    `json:"prompt_version"`
	StartReason   string    `json:"start_reason"`
}

func ValidAgentRole(role string) bool {
	switch role {
	case "planner", "triage", "dev_planner", "builder", "analyst", "verifier", "review_planner", "reviewer", "skeptic":
		return true
	}
	return false
}

func AgentExecutionHash(w Workflow, task Task, role, promptVersion string) string {
	review := role == "review_planner" || role == "reviewer" || role == "skeptic"
	if len(task.ID) >= 12 && task.ID[:12] == "evidence-ER-" {
		review = true
	}
	revision := w.PlanRevision
	if review {
		revision = w.Review.PlanRevision
	}
	if revision < 1 {
		revision = 1
	}
	return Digest([]any{w.ID, w.Generation, w.Spec.Digest, w.BaseSHA, w.HeadSHA, w.Review.Key, w.TestedSHA, w.TestedTreeSHA, revision, task.ID, task.LeaseEpoch, task.Paths, task.DependsOn, task.ObligationIDs, promptVersion})
}
