package orchestration

import (
	"context"
	"time"
)

// waitWorkflowAgentCleanup waits for this controller's active callbacks to
// finish their cleanup attempt. STOPPING can remain in the database when Docker
// cannot confirm exit: capacity and elapsed time stay charged in that case.
func (e *Engine) waitWorkflowAgentCleanup(ctx context.Context, workflowID string) error {
	timer := time.NewTicker(25 * time.Millisecond)
	defer timer.Stop()
	for {
		runs, err := e.Store.ListAgentRuns(ctx, workflowID)
		if err != nil {
			return err
		}
		cancels := []context.CancelFunc{}
		e.mu.RLock()
		for _, run := range runs {
			if run.Owner == e.owner {
				if cancel, live := e.agentActive[run.ID]; live {
					cancels = append(cancels, cancel)
				}
			}
		}
		e.mu.RUnlock()
		if len(cancels) == 0 {
			return nil
		}
		for _, cancel := range cancels {
			cancel()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
