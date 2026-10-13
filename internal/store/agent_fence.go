package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
)

type agentFenceContextKey struct{}

// WithAgentFence binds a result to the exact session and execution snapshot.
// The context must remain attached until the result transaction commits.
func WithAgentFence(ctx context.Context, run domain.AgentRun) context.Context {
	return context.WithValue(ctx, agentFenceContextKey{}, run)
}

func AgentFenceFromContext(ctx context.Context) (domain.AgentRun, bool) {
	run, ok := ctx.Value(agentFenceContextKey{}).(domain.AgentRun)
	return run, ok
}

// checkAgentFence is called after the caller locks the target workflow row.
// The caller must also verify that run.WorkflowID equals its write target.
// Holding SHARE until commit blocks lease revocation while publishing a result.
// clock_timestamp (not now) rejects a lease that expired while acquiring locks.
func checkAgentFence(ctx context.Context, tx pgx.Tx) error {
	r, ok := AgentFenceFromContext(ctx)
	if !ok {
		return nil
	}
	if r.ID == "" || r.WorkflowID == "" || r.Owner == "" || r.ContextHash == "" || r.LeaseEpoch < 1 || r.Generation < 1 {
		return ErrStale
	}
	var lockedID string
	if err := tx.QueryRow(ctx, `SELECT id FROM agent_runs WHERE id=$1 FOR SHARE`, r.ID).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStale
		}
		return err
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT a.status='ACTIVE' AND a.owner=$2 AND a.lease_epoch=$3
 AND a.workflow_id=$4 AND a.generation=$5 AND a.context_hash=$6
 AND a.plan_revision=$7 AND a.task_id=$8 AND a.role=$9 AND a.prompt_version=$10
 AND a.lease_until>clock_timestamp() AND w.control='ACTIVE' AND w.generation=a.generation
 FROM agent_runs a JOIN workflows w ON w.id=a.workflow_id WHERE a.id=$1`,
		r.ID, r.Owner, r.LeaseEpoch, r.WorkflowID, r.Generation, r.ContextHash, r.PlanRevision, r.TaskID, r.Role, r.PromptVersion).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStale
	}
	if err != nil {
		return err
	}
	if !valid {
		return ErrStale
	}
	// Old rows had no prompt protocol. New worker results must still refer to
	// the current task attempt and exact snapshot at the transaction boundary.
	// Planners/triage legitimately change the spec/plan within their own Save.
	worker := r.Role == "builder" || r.Role == "analyst" || r.Role == "verifier" || r.Role == "reviewer" || r.Role == "skeptic"
	if r.PromptVersion != "" && worker && r.TaskID != "trusted-verification" {
		w, err := scan(tx.QueryRow(ctx, workflowSelect+` WHERE w.id=$1`, r.WorkflowID))
		if err != nil {
			return err
		}
		tasks := w.Tasks
		revision := w.PlanRevision
		if r.Role == "reviewer" || r.Role == "skeptic" || strings.HasPrefix(r.TaskID, "evidence-ER-") {
			tasks = w.Review.Tasks
			revision = w.Review.PlanRevision
		}
		if max(1, revision) != r.PlanRevision {
			return ErrStale
		}
		found := false
		for _, task := range tasks {
			if task.ID != r.TaskID {
				continue
			}
			found = true
			if task.Status == "SUPERSEDED" || task.PlanRevision != r.PlanRevision || domain.AgentExecutionHash(w, task, r.Role, r.PromptVersion) != r.ContextHash {
				return ErrStale
			}
		}
		if !found {
			return ErrStale
		}
	}
	return nil
}
