package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
)

// CreateIntake atomically creates the workflow, metrics and initial status action.
// Replays also repair legacy rows created before the initial action was durable.
// The callback receives the persisted snapshot, never the stale webhook payload.
func (s *Store) CreateIntake(ctx context.Context, w domain.Workflow, comment func(domain.Workflow) Action) (bool, error) {
	if comment == nil {
		return false, errors.New("initial status callback is required")
	}
	w.Version, w.Generation = 1, 1
	w.CreatedAt = time.Now().UTC()
	w.UpdatedAt = w.CreatedAt
	if w.Control == "" {
		w.Control = "ACTIVE"
	}
	if w.Phase == "" {
		w.Phase = "INTAKE"
	}
	if w.RepositoryEpoch == "" {
		w.RepositoryEpoch = "legacy"
	}
	b, err := json.Marshal(w)
	if err != nil {
		return false, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err = lockRepositoryEpoch(ctx, tx, w.RepositoryID); err != nil {
		return false, err
	}
	if err = checkRepositoryDispatch(ctx, tx, w.RepositoryID); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO repository_epochs(repository_id,epoch) VALUES($1,$2) ON CONFLICT DO NOTHING`, w.RepositoryID, w.RepositoryEpoch); err != nil {
		return false, err
	}
	var epoch string
	if err = tx.QueryRow(ctx, `SELECT epoch FROM repository_epochs WHERE repository_id=$1`, w.RepositoryID).Scan(&epoch); err != nil {
		return false, err
	}
	if epoch != w.RepositoryEpoch {
		return false, ErrStale
	}
	var retired bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM retired_workflow_ids WHERE workflow_id=$1)`, w.ID).Scan(&retired); err != nil {
		return false, err
	}
	if retired {
		return false, ErrStale
	}
	tag, err := tx.Exec(ctx, `INSERT INTO workflows(id,repository_id,issue,phase,control,generation,version,data) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, w.ID, w.RepositoryID, w.Issue, w.Phase, w.Control, w.Generation, w.Version, b)
	if err != nil {
		return false, err
	}
	created := tag.RowsAffected() == 1
	if created {
		if _, err = tx.Exec(ctx, `INSERT INTO workflow_metrics(workflow_id) VALUES($1)`, w.ID); err != nil {
			return false, err
		}
		if err = queueLabelSync(ctx, tx, w); err != nil {
			return false, err
		}
	}
	current, err := scan(tx.QueryRow(ctx, workflowSelect+` WHERE w.repository_id=$1 AND w.issue=$2 FOR UPDATE OF w`, w.RepositoryID, w.Issue))
	if err != nil {
		return false, err
	}
	currentEpoch := current.RepositoryEpoch
	if currentEpoch == "" {
		currentEpoch = "legacy"
	}
	if current.ID != w.ID || currentEpoch != epoch {
		return false, ErrStale
	}
	var hasComment bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE workflow_id=$1 AND kind='comment')`, current.ID).Scan(&hasComment); err != nil {
		return false, err
	}
	if !hasComment {
		a := comment(current)
		if a.WorkflowID != current.ID || a.Generation != current.Generation || a.Kind != "comment" || !json.Valid(a.Data) {
			return false, errors.New("invalid initial status action")
		}
		a.ID = current.ID + ":initial-status"
		if err = insertActions(ctx, tx, []Action{a}); err != nil {
			return false, err
		}
	}
	return created, tx.Commit(ctx)
}
