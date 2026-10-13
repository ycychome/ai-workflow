package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRepositoryBusy = errors.New("repository has active agents or unconfirmed external writes")

func lockRepositoryEpoch(ctx context.Context, tx pgx.Tx, repositoryID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('repository-epoch:' || $1::bigint::text,0))`, repositoryID)
	return err
}

// RetireRepository is an administrative reset boundary, not a deletion. The
// caller must first stop controllers and obtain backups. Uncertain external
// writes and unacknowledged processes make retirement fail closed.
// Deprecated: retirement requires an acquired maintenance token and plan.
func (s *Store) RetireRepository(ctx context.Context, repositoryID int64, newEpoch string) error {
	return ErrMaintenanceIdentity
}

func retireRepositoryTx(ctx context.Context, tx pgx.Tx, repositoryID int64, newEpoch string) error {
	var previous string
	err := tx.QueryRow(ctx, `SELECT epoch FROM repository_epochs WHERE repository_id=$1`, repositoryID).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if previous == newEpoch {
		return nil
	}
	// Never cycle back to an old epoch, even if no workflows remain active.
	var reused bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM retired_workflow_ids WHERE repository_id=$1 AND epoch=$2)`, repositoryID, newEpoch).Scan(&reused); err != nil {
		return err
	}
	if reused {
		return errors.New("repository epoch has already been retired")
	}
	rows, err := tx.Query(ctx, workflowSelect+` WHERE w.repository_id=$1 ORDER BY w.id FOR UPDATE OF w`, repositoryID)
	if err != nil {
		return err
	}
	var ids []string
	// Keep full JSON snapshots consistent with the materialized fence columns.
	type retiredRow struct {
		id                  string
		generation, version int64
		data                []byte
		epoch               string
	}
	var updates []retiredRow
	for rows.Next() {
		w, e := scan(rows)
		if e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, w.ID)
		epoch := w.RepositoryEpoch
		if epoch == "" {
			epoch = "legacy"
		}
		w.Control = "CANCELLED"
		w.Generation++
		w.Version++
		w.LeaseOwner = ""
		w.LeaseUntil = time.Time{}
		w.UpdatedAt = time.Now().UTC()
		data, e := json.Marshal(w)
		if e != nil {
			rows.Close()
			return e
		}
		updates = append(updates, retiredRow{w.ID, w.Generation, w.Version, data, epoch})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflows WHERE repository_id=$1 AND lease_owner<>'' AND lease_until>clock_timestamp())`, repositoryID).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrRepositoryBusy
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runs WHERE workflow_id=ANY($1::text[]) AND status IN ('ACTIVE','STOPPING'))`, ids).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrRepositoryBusy
	}
	// Lock all existing actions before checking state, closing the ClaimAction race.
	actions, err := tx.Query(ctx, `SELECT state FROM outbox WHERE workflow_id=ANY($1::text[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return err
	}
	for actions.Next() {
		var state string
		if err = actions.Scan(&state); err != nil {
			actions.Close()
			return err
		}
		if state == "running" || state == "unknown" {
			busy = true
		}
	}
	err = actions.Err()
	actions.Close()
	if err != nil {
		return err
	}
	if busy {
		return ErrRepositoryBusy
	}
	for _, w := range updates {
		if _, err = tx.Exec(ctx, `INSERT INTO retired_workflow_ids(workflow_id,repository_id,epoch) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, w.id, repositoryID, w.epoch); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE workflows SET control='CANCELLED',generation=$2,version=$3,data=$4,lease_owner='',lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1`, w.id, w.generation, w.version, w.data); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE outbox SET state='obsolete',lease_until=NULL,error='仓库运行代次已退役' WHERE workflow_id=ANY($1::text[]) AND state NOT IN ('done','obsolete')`, ids); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_runs SET status='OBSOLETE',queue_reason='仓库运行代次已退役' WHERE workflow_id=ANY($1::text[]) AND status='QUEUED'`, ids); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO repository_epochs(repository_id,epoch) VALUES($1,$2) ON CONFLICT(repository_id) DO UPDATE SET epoch=EXCLUDED.epoch,updated_at=clock_timestamp()`, repositoryID, newEpoch); err != nil {
		return err
	}
	return nil
}

func (s *Store) IsRetiredWorkflow(ctx context.Context, id string) (bool, error) {
	var retired bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM retired_workflow_ids WHERE workflow_id=$1)`, id).Scan(&retired)
	return retired, err
}
