package orchestration

import (
	"context"
	"errors"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/store"
)

// Turning the optional manual gate off releases existing AI-accepted proposals.
// Paused/cancelled workflows and stale proposals are deliberately left for their
// existing control/reconciliation paths; configuration never revives them.
func (e *Engine) releaseAcceptedSpecs(ctx context.Context) error {
	if e.Config().Runner.RequireAcceptForDevelopment {
		return nil
	}
	rows, err := e.Store.DB.Query(ctx, `SELECT id FROM workflows WHERE phase='SPEC_READY' AND control='ACTIVE' ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if e.Config().Runner.RequireAcceptForDevelopment {
			return nil
		}
		err = e.Store.Update(ctx, id, func(w *domain.Workflow) ([]store.Action, error) {
			if w.Phase != "SPEC_READY" || w.Control != "ACTIVE" || e.Config().Runner.RequireAcceptForDevelopment {
				return nil, store.ErrNoChange
			}
			if validateProposalSpec(w, w.ProposalSpec) != nil {
				return nil, store.ErrNoChange
			}
			p := w.ProposalSpec
			if err := applyTriageReport(w, triageReport{Decision: "accepted", Title: p.Title, Description: p.Description, Criteria: p.Criteria, NonGoals: p.NonGoals, Risk: p.Risk}, false); err != nil {
				return nil, err
			}
			w.Generation++
			return []store.Action{statusAction(*w)}, nil
		})
		if err != nil && !errors.Is(err, store.ErrStale) {
			return err
		}
	}
	return nil
}
