package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

var ErrRepositoryMaintenance = errors.New("仓库维护期间停止接收和派发新工作")
var ErrMaintenanceIdentity = errors.New("仓库维护令牌、代次或计划摘要不匹配")

// The caller generates and durably stores Token before acquisition so an
// unknown commit result can be retried without claiming a different operation.
type RepositoryMaintenance struct {
	RepositoryID int64  `json:"repository_id"`
	Token        string `json:"-"`
	OldEpoch     string `json:"old_epoch"`
	NewEpoch     string `json:"new_epoch"`
	PlanDigest   string `json:"plan_digest"`
}
type RepositoryMaintenanceStatus struct {
	RepositoryMaintenance
	Held               bool   `json:"held"`
	IntakeStopped      bool   `json:"intake_stopped"`
	Retired            bool   `json:"retired"`
	Epoch              string `json:"epoch"`
	ActiveAgents       int    `json:"active_agents"`
	StoppingAgents     int    `json:"stopping_agents"`
	LiveWorkflowLeases int    `json:"live_workflow_leases"`
	RunningOutbox      int    `json:"running_outbox"`
	UnknownOutbox      int    `json:"unknown_outbox"`
}

func (s RepositoryMaintenanceStatus) Quiescent() bool {
	return s.ActiveAgents == 0 && s.StoppingAgents == 0 && s.LiveWorkflowLeases == 0 && s.RunningOutbox == 0 && s.UnknownOutbox == 0
}
func validMaintenance(p RepositoryMaintenance) bool {
	return p.RepositoryID > 0 && len(p.Token) >= 32 && strings.TrimSpace(p.OldEpoch) != "" && strings.TrimSpace(p.NewEpoch) != "" && p.NewEpoch != "legacy" && p.OldEpoch != p.NewEpoch && strings.TrimSpace(p.PlanDigest) != ""
}
func sameMaintenance(a, b RepositoryMaintenance) bool {
	return a.RepositoryID == b.RepositoryID && a.Token == b.Token && a.OldEpoch == b.OldEpoch && a.NewEpoch == b.NewEpoch && a.PlanDigest == b.PlanDigest
}

func maintenanceStatus(ctx context.Context, tx pgx.Tx, repositoryID int64) (RepositoryMaintenanceStatus, error) {
	out := RepositoryMaintenanceStatus{RepositoryMaintenance: RepositoryMaintenance{RepositoryID: repositoryID}, Epoch: "legacy"}
	err := tx.QueryRow(ctx, `SELECT token,old_epoch,new_epoch,plan_digest,held,intake_stopped,retired FROM repository_maintenance WHERE repository_id=$1`, repositoryID).Scan(&out.Token, &out.OldEpoch, &out.NewEpoch, &out.PlanDigest, &out.Held, &out.IntakeStopped, &out.Retired)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT epoch FROM repository_epochs WHERE repository_id=$1),'legacy'),
 (SELECT count(*) FROM agent_runs a JOIN workflows w ON w.id=a.workflow_id WHERE w.repository_id=$1 AND a.status='ACTIVE'),
 (SELECT count(*) FROM agent_runs a JOIN workflows w ON w.id=a.workflow_id WHERE w.repository_id=$1 AND a.status='STOPPING'),
 (SELECT count(*) FROM workflows WHERE repository_id=$1 AND lease_owner<>'' AND lease_until>clock_timestamp()),
 (SELECT count(*) FROM outbox o JOIN workflows w ON w.id=o.workflow_id WHERE w.repository_id=$1 AND o.state='running'),
 (SELECT count(*) FROM outbox o JOIN workflows w ON w.id=o.workflow_id WHERE w.repository_id=$1 AND o.state='unknown')`, repositoryID).Scan(&out.Epoch, &out.ActiveAgents, &out.StoppingAgents, &out.LiveWorkflowLeases, &out.RunningOutbox, &out.UnknownOutbox)
	return out, err
}
func (s *Store) RepositoryMaintenanceStatus(ctx context.Context, repositoryID int64) (RepositoryMaintenanceStatus, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RepositoryMaintenanceStatus{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockRepositoryEpoch(ctx, tx, repositoryID); err != nil {
		return RepositoryMaintenanceStatus{}, err
	}
	status, err := maintenanceStatus(ctx, tx, repositoryID)
	if err != nil {
		return status, err
	}
	return status, tx.Commit(ctx)
}
func (s *Store) AcquireRepositoryMaintenance(ctx context.Context, plan RepositoryMaintenance) (RepositoryMaintenanceStatus, error) {
	if !validMaintenance(plan) {
		return RepositoryMaintenanceStatus{}, ErrMaintenanceIdentity
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RepositoryMaintenanceStatus{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockRepositoryEpoch(ctx, tx, plan.RepositoryID); err != nil {
		return RepositoryMaintenanceStatus{}, err
	}
	status, err := maintenanceStatus(ctx, tx, plan.RepositoryID)
	if err != nil {
		return status, err
	}
	if status.Held {
		if !sameMaintenance(status.RepositoryMaintenance, plan) {
			return status, ErrMaintenanceIdentity
		}
		return status, tx.Commit(ctx)
	}
	// A failed adapter may have released the lock while deliberately keeping
	// intake stopped. Only that exact durable operation can resume in place.
	if status.IntakeStopped && sameMaintenance(status.RepositoryMaintenance, plan) {
		expected := plan.OldEpoch
		if status.Retired {
			expected = plan.NewEpoch
		}
		if status.Epoch != expected {
			return status, ErrStale
		}
		if _, err = tx.Exec(ctx, `UPDATE repository_maintenance SET held=true,updated_at=clock_timestamp() WHERE repository_id=$1`, plan.RepositoryID); err != nil {
			return status, err
		}
		status.Held = true
		return status, tx.Commit(ctx)
	}
	if status.Epoch != plan.OldEpoch {
		return status, ErrStale
	}
	if status.Token == plan.Token {
		return status, ErrMaintenanceIdentity
	}
	_, err = tx.Exec(ctx, `INSERT INTO repository_maintenance(repository_id,token,old_epoch,new_epoch,plan_digest,held,intake_stopped,retired) VALUES($1,$2,$3,$4,$5,true,true,false)
 ON CONFLICT(repository_id) DO UPDATE SET token=EXCLUDED.token,old_epoch=EXCLUDED.old_epoch,new_epoch=EXCLUDED.new_epoch,plan_digest=EXCLUDED.plan_digest,held=true,intake_stopped=true,retired=false,updated_at=clock_timestamp()`, plan.RepositoryID, plan.Token, plan.OldEpoch, plan.NewEpoch, plan.PlanDigest)
	if err != nil {
		return status, err
	}
	status.RepositoryMaintenance = plan
	status.Held = true
	status.IntakeStopped = true
	status.Retired = false
	return status, tx.Commit(ctx)
}
func (s *Store) RetireRepositoryMaintenance(ctx context.Context, plan RepositoryMaintenance) error {
	if !validMaintenance(plan) {
		return ErrMaintenanceIdentity
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Maintenance already excludes admissions for this repository. Do not
	// hold the global Agent capacity lock while retiring its historical rows.
	if err = lockRepositoryEpoch(ctx, tx, plan.RepositoryID); err != nil {
		return err
	}
	status, err := maintenanceStatus(ctx, tx, plan.RepositoryID)
	if err != nil {
		return err
	}
	if !sameMaintenance(status.RepositoryMaintenance, plan) {
		return ErrMaintenanceIdentity
	}
	if status.Retired {
		if status.Epoch != plan.NewEpoch {
			return ErrStale
		}
		return tx.Commit(ctx)
	}
	if !status.Held {
		return ErrMaintenanceIdentity
	}
	if status.Epoch != plan.OldEpoch {
		return ErrStale
	}
	if !status.Quiescent() {
		return ErrRepositoryBusy
	}
	if err = retireRepositoryTx(ctx, tx, plan.RepositoryID, plan.NewEpoch); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE repository_maintenance SET retired=true,updated_at=clock_timestamp() WHERE repository_id=$1`, plan.RepositoryID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) ReleaseRepositoryMaintenance(ctx context.Context, plan RepositoryMaintenance, keepIntakeStopped bool) error {
	if !validMaintenance(plan) {
		return ErrMaintenanceIdentity
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockRepositoryEpoch(ctx, tx, plan.RepositoryID); err != nil {
		return err
	}
	status, err := maintenanceStatus(ctx, tx, plan.RepositoryID)
	if err != nil {
		return err
	}
	if !sameMaintenance(status.RepositoryMaintenance, plan) {
		return ErrMaintenanceIdentity
	}
	expected := plan.OldEpoch
	if status.Retired {
		expected = plan.NewEpoch
	}
	if status.Epoch != expected {
		return ErrStale
	}
	if _, err = tx.Exec(ctx, `UPDATE repository_maintenance SET held=false,intake_stopped=$2,updated_at=clock_timestamp() WHERE repository_id=$1`, plan.RepositoryID, keepIntakeStopped); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Call only while holding the same repository advisory lock used by Acquire.
func checkRepositoryDispatch(ctx context.Context, tx pgx.Tx, repositoryID int64) error {
	var stopped bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repository_maintenance WHERE repository_id=$1 AND (held OR intake_stopped))`, repositoryID).Scan(&stopped); err != nil {
		return err
	}
	if stopped {
		return ErrRepositoryMaintenance
	}
	return nil
}
func lockWorkflowDispatch(ctx context.Context, tx pgx.Tx, workflowID string) error {
	var repositoryID int64
	if err := tx.QueryRow(ctx, `SELECT repository_id FROM workflows WHERE id=$1`, workflowID).Scan(&repositoryID); err != nil {
		return err
	}
	if err := lockRepositoryEpoch(ctx, tx, repositoryID); err != nil {
		return err
	}
	if err := checkRepositoryDispatch(ctx, tx, repositoryID); err != nil {
		return err
	}
	var retired bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM retired_workflow_ids WHERE workflow_id=$1)`, workflowID).Scan(&retired); err != nil {
		return err
	}
	if retired {
		return ErrStale
	}
	return nil
}

func tryLockRepositoryEpoch(ctx context.Context, tx pgx.Tx, repositoryID int64) (bool, error) {
	var locked bool
	err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('repository-epoch:' || $1::bigint::text,0))`, repositoryID).Scan(&locked)
	return locked, err
}
func tryLockWorkflowDispatch(ctx context.Context, tx pgx.Tx, workflowID string) error {
	var repositoryID int64
	if err := tx.QueryRow(ctx, `SELECT repository_id FROM workflows WHERE id=$1`, workflowID).Scan(&repositoryID); err != nil {
		return err
	}
	locked, err := tryLockRepositoryEpoch(ctx, tx, repositoryID)
	if err != nil {
		return err
	}
	if !locked {
		return ErrAgentQueued
	}
	return lockWorkflowDispatch(ctx, tx, workflowID)
}

// Check after locking the workflow so a pre-reset event cannot pass a stale
// read-side retirement check and then rewrite the preserved historical row.
func checkWorkflowNotRetired(ctx context.Context, tx pgx.Tx, workflowID string) error {
	var retired bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM retired_workflow_ids WHERE workflow_id=$1)`, workflowID).Scan(&retired); err != nil {
		return err
	}
	if retired {
		return ErrStale
	}
	return nil
}
