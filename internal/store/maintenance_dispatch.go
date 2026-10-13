package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Claim selection does not hold workflow/outbox rows before the repository
// lock. This matches retirement and prevents an inverse lock-order deadlock.
func (s *Store) claimRepositoryAction(ctx context.Context, labels bool) (Action, error) {
	var out Action
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var repositoryID int64
	var id, workflowID string
	err = tx.QueryRow(ctx, `SELECT o.id,o.workflow_id,w.repository_id FROM outbox o JOIN workflows w ON w.id=o.workflow_id
 WHERE (o.kind='labels')=$1 AND o.state IN ('pending','retry_wait','unknown','running')
 AND o.next_attempt<=clock_timestamp() AND (o.lease_until IS NULL OR o.lease_until<clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM repository_maintenance m WHERE m.repository_id=w.repository_id AND (m.held OR m.intake_stopped))
 AND NOT EXISTS(SELECT 1 FROM retired_workflow_ids r WHERE r.workflow_id=w.id)
 AND NOT EXISTS(SELECT 1 FROM outbox earlier WHERE earlier.workflow_id=o.workflow_id AND (earlier.kind='labels')=$1
 AND (earlier.created_at<o.created_at OR (earlier.created_at=o.created_at AND earlier.id<o.id))
 AND earlier.state IN ('pending','retry_wait','unknown','running'))
 ORDER BY o.created_at,o.id LIMIT 1`, labels).Scan(&id, &workflowID, &repositoryID)
	if err != nil {
		return out, err
	}
	if err = lockRepositoryEpoch(ctx, tx, repositoryID); err != nil {
		return out, err
	}
	if err = checkRepositoryDispatch(ctx, tx, repositoryID); err != nil {
		return out, err
	}
	// Recheck after the lock: a competing claimant may have acquired the row.
	err = tx.QueryRow(ctx, `UPDATE outbox SET state='running',lease_until=clock_timestamp()+interval '90 seconds',attempts=attempts+1
 WHERE id=$1 AND workflow_id=$2 AND state IN ('pending','retry_wait','unknown','running')
 AND next_attempt<=clock_timestamp() AND (lease_until IS NULL OR lease_until<clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM retired_workflow_ids WHERE workflow_id=$2)
 RETURNING id,workflow_id,generation,kind,data,attempts`, id, workflowID).Scan(&out.ID, &out.WorkflowID, &out.Generation, &out.Kind, &out.Data, &out.Attempts)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func checkRepositoryIntakeEpoch(ctx context.Context, tx pgx.Tx, repositoryID int64, epoch string) error {
	if epoch == "" {
		epoch = "legacy"
	}
	if err := checkRepositoryDispatch(ctx, tx, repositoryID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repository_epochs(repository_id,epoch) VALUES($1,$2) ON CONFLICT DO NOTHING`, repositoryID, epoch); err != nil {
		return err
	}
	var current string
	if err := tx.QueryRow(ctx, `SELECT epoch FROM repository_epochs WHERE repository_id=$1`, repositoryID).Scan(&current); err != nil {
		return err
	}
	if current != epoch {
		return ErrStale
	}
	return nil
}
