package orchestration

import (
	"context"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

// Called inside the ready receipt transaction. A replay uses the same child ID,
// so a lost receipt acknowledgement cannot enqueue a second successful gate.
func (e *Engine) afterReadyGate(ctx context.Context, w domain.Workflow, a store.Action, d readyActionData) ([]store.Action, error) {
	if d.GateAfterReady == nil {
		return nil, nil
	}
	gate := *d.GateAfterReady
	if !gate.Pass || !gate.ManualOverride || !e.gateActionMatchesCurrent(ctx, w, a, gate, true) {
		return nil, store.ErrStale
	}
	next := store.NewAction(w, "gate", gate)
	next.ID = a.ID + ":after-ready:gate"
	return []store.Action{next}, nil
}
