package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
)

// Old hashes discarded semantic whitespace, so their approvals cannot safely
// be rebound to current text. Preserve the accepted spec for audit, fence work,
// and revoke its gate until the issue is analyzed and approved again.
func (s *Store) migrateIssueSourceIdentity(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1634309990,3)"); err != nil {
		return err
	}
	var applied bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=3)").Scan(&applied); err != nil {
		return err
	}
	if applied {
		return tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, workflowSelect+` WHERE w.phase<>'MERGED' AND (
 (COALESCE(w.data->'spec'->>'source_hash','')<>'' AND w.data->'spec'->>'source_hash' NOT LIKE 'issue-v3:%') OR
 (COALESCE(w.data->'proposal_spec'->>'source_hash','')<>'' AND w.data->'proposal_spec'->>'source_hash' NOT LIKE 'issue-v3:%')) FOR UPDATE OF w`)
	if err != nil {
		return err
	}
	var workflows []domain.Workflow
	for rows.Next() {
		w, scanErr := scan(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		workflows = append(workflows, w)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	const reason = "Issue source identity upgraded to preserve semantic whitespace; use /ai analyze and approve a new specification before resuming"
	for _, w := range workflows {
		oldGeneration, oldControl := w.Generation, w.Control
		w.Generation++
		w.Version++
		w.UpdatedAt = time.Now().UTC()
		if w.Control == "ACTIVE" {
			w.Control = "PAUSING"
		}
		w.Review.Verdict = "OBSOLETE"
		w.ForceApproval = nil
		w.ProposalSpec = nil
		w.LastError = reason
		b, err := json.Marshal(w)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE workflows SET data=$2,control=$3,generation=$4,version=$5,updated_at=now() WHERE id=$1", w.ID, b, w.Control, w.Generation, w.Version); err != nil {
			return err
		}
		if err = obsoleteUnstartedActions(ctx, tx, w.ID, oldGeneration, oldControl, w.Generation, w.Control); err != nil {
			return err
		}
		gate := NewAction(w, "gate", map[string]any{"pass": false, "reason": reason, "key": w.Review.Key, "sha": w.HeadSHA})
		gate.ID = w.ID + ":issue-source-v3:gate"
		if err = insertActions(ctx, tx, []Action{gate}); err != nil {
			return err
		}
		if err = queueLabelSync(ctx, tx, w); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO audit(workflow_id,kind,data) VALUES($1,'issue_source_identity_invalidated',$2)", w.ID, gate.Data); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(3)"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
