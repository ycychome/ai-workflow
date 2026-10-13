package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
)

var ErrAgentQueued = errors.New("agent is waiting for capacity or its fair turn")

const AgentLeaseDuration = 90 * time.Second

// MigrateAgentQueue only adds queue storage; it never rewrites workflow JSON.
func (s *Store) MigrateAgentQueue(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `
SELECT pg_advisory_xact_lock(1634309990,4);
CREATE TABLE IF NOT EXISTS agent_runs (
 id text PRIMARY KEY, workflow_id text NOT NULL REFERENCES workflows(id),
 task_id text NOT NULL, role text NOT NULL CHECK(role IN ('planner','triage','dev_planner','builder','analyst','verifier','review_planner','reviewer','skeptic')),
 generation bigint NOT NULL, plan_revision integer NOT NULL DEFAULT 0,
 lease_epoch bigint NOT NULL DEFAULT 0, owner text NOT NULL DEFAULT '',
 status text NOT NULL CHECK(status IN ('QUEUED','ACTIVE','STOPPING','FINISHED','OBSOLETE')),
 enqueued_at timestamptz NOT NULL DEFAULT now(), started_at timestamptz,
 heartbeat_at timestamptz, stopped_at timestamptz, lease_until timestamptz,
 queue_reason text NOT NULL DEFAULT '', context_hash text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS agent_runs_capacity ON agent_runs(status,workflow_id);
CREATE INDEX IF NOT EXISTS agent_runs_queue ON agent_runs(workflow_id,enqueued_at,id) WHERE status='QUEUED';
CREATE TABLE IF NOT EXISTS agent_workflow_turns(workflow_id text PRIMARY KEY REFERENCES workflows(id), last_claimed_at timestamptz NOT NULL);
INSERT INTO schema_migrations(version) VALUES(4) ON CONFLICT DO NOTHING;
SELECT pg_advisory_xact_lock(1634309990,5);
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS prompt_version text NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS start_reason text NOT NULL DEFAULT '';
INSERT INTO schema_migrations(version) VALUES(5) ON CONFLICT DO NOTHING;
SELECT pg_advisory_xact_lock(1634309990,6);
CREATE TABLE IF NOT EXISTS repository_epochs(repository_id bigint PRIMARY KEY,epoch text NOT NULL,updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS retired_workflow_ids(workflow_id text PRIMARY KEY REFERENCES workflows(id),repository_id bigint NOT NULL,epoch text NOT NULL,retired_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS retired_workflow_repository ON retired_workflow_ids(repository_id,epoch);
INSERT INTO schema_migrations(version) VALUES(6) ON CONFLICT DO NOTHING;
SELECT pg_advisory_xact_lock(1634309990,7);
CREATE TABLE IF NOT EXISTS agent_limits(singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),global_cap integer NOT NULL CHECK(global_cap>0),workflow_cap integer NOT NULL CHECK(workflow_cap>0 AND workflow_cap<=global_cap),updated_at timestamptz NOT NULL DEFAULT now());
INSERT INTO schema_migrations(version) VALUES(7) ON CONFLICT DO NOTHING;
SELECT pg_advisory_xact_lock(1634309990,8);
ALTER TABLE workflow_metrics ADD COLUMN IF NOT EXISTS agent_runtime_seconds bigint NOT NULL DEFAULT 0;
ALTER TABLE workflow_metrics ADD COLUMN IF NOT EXISTS controller_runtime_seconds bigint NOT NULL DEFAULT 0;
UPDATE workflow_metrics SET controller_runtime_seconds=runtime_seconds WHERE NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=8);
INSERT INTO schema_migrations(version) VALUES(8) ON CONFLICT DO NOTHING;
SELECT pg_advisory_xact_lock(1634309990,9);
CREATE TABLE IF NOT EXISTS repository_maintenance(repository_id bigint PRIMARY KEY,token text NOT NULL,old_epoch text NOT NULL,new_epoch text NOT NULL,plan_digest text NOT NULL,held boolean NOT NULL DEFAULT true,intake_stopped boolean NOT NULL DEFAULT true,retired boolean NOT NULL DEFAULT false,updated_at timestamptz NOT NULL DEFAULT now());
INSERT INTO schema_migrations(version) VALUES(9) ON CONFLICT DO NOTHING;
`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnqueueAgent is idempotent only for the same immutable session identity.
func (s *Store) EnqueueAgent(ctx context.Context, r domain.AgentRun) error {
	if r.ID == "" || r.WorkflowID == "" || r.Generation < 1 || !domain.ValidAgentRole(r.Role) {
		return errors.New("invalid agent identity")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockWorkflowDispatch(ctx, tx, r.WorkflowID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO agent_runs(id,workflow_id,task_id,role,generation,plan_revision,context_hash,prompt_version,start_reason,owner,status)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'QUEUED' FROM workflows WHERE id=$2 AND generation=$5 AND control='ACTIVE'
 AND ($10='' OR (lease_owner=$10 AND lease_until>clock_timestamp()))
 ON CONFLICT(id) DO UPDATE SET id=EXCLUDED.id
 WHERE agent_runs.workflow_id=EXCLUDED.workflow_id AND agent_runs.task_id=EXCLUDED.task_id
 AND agent_runs.role=EXCLUDED.role AND agent_runs.generation=EXCLUDED.generation
 AND agent_runs.plan_revision=EXCLUDED.plan_revision AND agent_runs.context_hash=EXCLUDED.context_hash AND agent_runs.prompt_version=EXCLUDED.prompt_version AND agent_runs.owner=EXCLUDED.owner`, r.ID, r.WorkflowID, r.TaskID, r.Role, r.Generation, r.PlanRevision, r.ContextHash, r.PromptVersion, r.StartReason, r.Owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return tx.Commit(ctx)
}

const agentColumns = `id,workflow_id,task_id,role,generation,plan_revision,lease_epoch,owner,status,enqueued_at,
 COALESCE(started_at,'0001-01-01'::timestamptz),COALESCE(heartbeat_at,'0001-01-01'::timestamptz),
 COALESCE(stopped_at,'0001-01-01'::timestamptz),COALESCE(lease_until,'0001-01-01'::timestamptz),queue_reason,context_hash,prompt_version,start_reason`

func scanAgent(row pgx.Row) (r domain.AgentRun, err error) {
	err = row.Scan(&r.ID, &r.WorkflowID, &r.TaskID, &r.Role, &r.Generation, &r.PlanRevision, &r.LeaseEpoch, &r.Owner, &r.Status, &r.EnqueuedAt, &r.StartedAt, &r.HeartbeatAt, &r.StoppedAt, &r.LeaseUntil, &r.QueueReason, &r.ContextHash, &r.PromptVersion, &r.StartReason)
	return
}

// ClaimAgent serializes capacity decisions across processes. Queued time is
// deliberately separate from StartedAt and must not count against run deadlines.
func (s *Store) ClaimAgent(ctx context.Context, id, owner string, globalCap, workflowCap int) (domain.AgentRun, error) {
	var zero domain.AgentRun
	if owner == "" || globalCap < 1 || workflowCap < 1 || workflowCap > globalCap {
		return zero, errors.New("invalid agent capacity or owner")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1634309990,40)`); err != nil {
		return zero, err
	}
	// Passed limits seed only an empty database. Every controller then uses the
	// same authoritative row, even when its in-memory configuration is stale.
	if _, err = tx.Exec(ctx, `INSERT INTO agent_limits(singleton,global_cap,workflow_cap) VALUES(true,$1,$2) ON CONFLICT DO NOTHING`, globalCap, workflowCap); err != nil {
		return zero, err
	}
	if err = tx.QueryRow(ctx, `SELECT global_cap,workflow_cap FROM agent_limits WHERE singleton`).Scan(&globalCap, &workflowCap); err != nil {
		return zero, err
	}
	var workflowID string
	if err = tx.QueryRow(ctx, `SELECT workflow_id FROM agent_runs WHERE id=$1`, id).Scan(&workflowID); err != nil {
		return zero, err
	}
	if err = tryLockWorkflowDispatch(ctx, tx, workflowID); err != nil {
		return zero, err
	}
	// Lock the workflow row so pause/cancel and generation changes cannot race admission.
	var control string
	var generation int64
	err = tx.QueryRow(ctx, `SELECT w.control,w.generation FROM workflows w JOIN agent_runs a ON a.workflow_id=w.id WHERE a.id=$1 FOR SHARE OF w`, id).Scan(&control, &generation)
	if err != nil {
		return zero, err
	}
	r, err := scanAgent(tx.QueryRow(ctx, `SELECT `+agentColumns+` FROM agent_runs WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return zero, err
	}
	if r.Status != domain.AgentQueued || (r.Owner != "" && r.Owner != owner) {
		return zero, ErrStale
	}
	var queuedOwnerLive bool
	if err = tx.QueryRow(ctx, `SELECT $2='' OR (lease_owner=$2 AND lease_until>clock_timestamp()) FROM workflows WHERE id=$1`, r.WorkflowID, r.Owner).Scan(&queuedOwnerLive); err != nil {
		return zero, err
	}
	if control != "ACTIVE" || generation != r.Generation || !queuedOwnerLive {
		if _, err = tx.Exec(ctx, `UPDATE agent_runs SET status='OBSOLETE',queue_reason='工作流代次、控制状态或排队控制器租约已失效' WHERE id=$1`, id); err != nil {
			return zero, err
		}
		if err = tx.Commit(ctx); err != nil {
			return zero, err
		}
		return zero, ErrStale
	}
	var used int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE status IN ('ACTIVE','STOPPING')`).Scan(&used); err != nil {
		return zero, err
	}
	var next string
	if used < globalCap {
		err = tx.QueryRow(ctx, `SELECT a.id FROM agent_runs a JOIN workflows w ON w.id=a.workflow_id
   LEFT JOIN agent_workflow_turns t ON t.workflow_id=a.workflow_id
   WHERE a.status='QUEUED' AND w.control='ACTIVE' AND w.generation=a.generation
 AND NOT EXISTS(SELECT 1 FROM repository_maintenance rm WHERE rm.repository_id=w.repository_id AND (rm.held OR rm.intake_stopped))
 AND NOT EXISTS(SELECT 1 FROM retired_workflow_ids rw WHERE rw.workflow_id=w.id)
   AND (a.owner='' OR (w.lease_owner=a.owner AND w.lease_until>clock_timestamp()))
   AND (SELECT count(*) FROM agent_runs active WHERE active.workflow_id=a.workflow_id AND active.status IN ('ACTIVE','STOPPING'))<$1
   ORDER BY t.last_claimed_at NULLS FIRST,a.enqueued_at,a.id LIMIT 1`, workflowCap).Scan(&next)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return zero, err
		}
	}
	if next != id {
		reason := "等待公平轮转或当前任务的并发额度"
		if used >= globalCap {
			reason = "等待全局并发额度"
		}
		if _, err = tx.Exec(ctx, `UPDATE agent_runs SET queue_reason=$2 WHERE id=$1`, id, reason); err != nil {
			return zero, err
		}
		if err = tx.Commit(ctx); err != nil {
			return zero, err
		}
		return zero, ErrAgentQueued
	}
	r, err = scanAgent(tx.QueryRow(ctx, `UPDATE agent_runs SET status='ACTIVE',owner=$2,lease_epoch=lease_epoch+1,
 started_at=clock_timestamp(),heartbeat_at=clock_timestamp(),lease_until=clock_timestamp()+interval '90 seconds',
 start_reason=CASE WHEN queue_reason<>'' THEN '排队后取得并发额度，启动前须重新检查工作流状态并绑定最新上下文' ELSE start_reason END
 WHERE id=$1 RETURNING `+agentColumns, id, owner))
	if err != nil {
		return zero, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO agent_workflow_turns(workflow_id,last_claimed_at) VALUES($1,clock_timestamp()) ON CONFLICT(workflow_id) DO UPDATE SET last_claimed_at=EXCLUDED.last_claimed_at`, r.WorkflowID); err != nil {
		return zero, err
	}
	return r, tx.Commit(ctx)
}

// RenewAgent never resurrects an expired lease or an obsolete generation.
func (s *Store) RenewAgent(ctx context.Context, id, owner string, epoch int64) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockAgentForMutation(ctx, tx, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_runs SET heartbeat_at=clock_timestamp(),lease_until=clock_timestamp()+interval '90 seconds'
 WHERE id=$1 AND owner=$2 AND lease_epoch=$3 AND status='ACTIVE' AND lease_until>clock_timestamp()`, id, owner, epoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return tx.Commit(ctx)
}

func (s *Store) CheckAgentLease(ctx context.Context, id, owner string, epoch int64) error {
	var valid bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runs a JOIN workflows w ON w.id=a.workflow_id
 WHERE a.id=$1 AND a.owner=$2 AND a.lease_epoch=$3 AND a.status='ACTIVE' AND a.lease_until>clock_timestamp()
 AND w.control='ACTIVE' AND w.generation=a.generation)`, id, owner, epoch).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrStale
	}
	return nil
}

func (s *Store) MarkAgentStopping(ctx context.Context, id, owner string, epoch int64) error {
	tag, err := s.DB.Exec(ctx, `UPDATE agent_runs SET status='STOPPING',lease_until=NULL WHERE id=$1 AND owner=$2 AND lease_epoch=$3 AND status IN ('ACTIVE','STOPPING')`, id, owner, epoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return nil
}

// FinishAgent requires positive confirmation of process exit, including on
// restart. Never call this merely because a deadline/heartbeat expired.
func (s *Store) FinishAgent(ctx context.Context, id, owner string, epoch int64) error {
	tag, err := s.DB.Exec(ctx, `UPDATE agent_runs SET status='FINISHED',stopped_at=COALESCE(stopped_at,now()),lease_until=NULL
 WHERE id=$1 AND owner=$2 AND lease_epoch=$3 AND status IN ('STOPPING','FINISHED')`, id, owner, epoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return nil
}

func (s *Store) ListAgentRuns(ctx context.Context, wfID string) ([]domain.AgentRun, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+agentColumns+` FROM agent_runs WHERE ($1='' OR workflow_id=$1) ORDER BY enqueued_at,id`, wfID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AgentRun{}
	for rows.Next() {
		r, e := scanAgent(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OrphanAgentRuns revokes expired/obsolete sessions but keeps their capacity.
// The supervisor must stop each returned process and then call FinishAgent.
func (s *Store) OrphanAgentRuns(ctx context.Context) ([]domain.AgentRun, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1634309990,40)`); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_runs a SET status='OBSOLETE',queue_reason='工作流代次、控制状态或排队控制器租约已失效'
 WHERE status='QUEUED' AND NOT EXISTS(SELECT 1 FROM workflows w WHERE w.id=a.workflow_id AND w.generation=a.generation AND w.control='ACTIVE'
 AND (a.owner='' OR (w.lease_owner=a.owner AND w.lease_until>clock_timestamp())))`); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_runs a SET status='STOPPING',lease_until=NULL,queue_reason='租约已过期或工作流已失效，等待确认进程退出'
 WHERE status='ACTIVE' AND (lease_until<=clock_timestamp() OR NOT EXISTS(SELECT 1 FROM workflows w WHERE w.id=a.workflow_id AND w.generation=a.generation AND w.control='ACTIVE'))`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+agentColumns+` FROM agent_runs WHERE status='STOPPING' ORDER BY enqueued_at,id`)
	if err != nil {
		return nil, err
	}
	out := []domain.AgentRun{}
	for rows.Next() {
		r, e := scanAgent(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("list orphan agents: %w", err)
	}
	return out, tx.Commit(ctx)
}

// CancelQueuedAgent retires a session whose queue consumer has gone away.
// It must never release capacity occupied by ACTIVE or STOPPING processes.
func (s *Store) CancelQueuedAgent(ctx context.Context, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE agent_runs SET status='OBSOLETE',queue_reason='排队请求已撤销，消费者不再等待' WHERE id=$1 AND status IN ('QUEUED','OBSOLETE')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return nil
}

// BindAgentContext binds the actual execution snapshot once capacity has been
// claimed. Binding is immutable and fenced against cancellation and expiry.
func (s *Store) BindAgentContext(ctx context.Context, id, owner string, epoch int64, hash string) error {
	if hash == "" {
		return ErrStale
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockAgentForMutation(ctx, tx, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_runs SET context_hash=$4 WHERE id=$1 AND owner=$2 AND lease_epoch=$3 AND status='ACTIVE' AND lease_until>clock_timestamp() AND (context_hash='' OR context_hash=$4)`, id, owner, epoch, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return tx.Commit(ctx)
}

// Separate lock acquisition from time predicates: a SQL target expression can
// be evaluated before waiting for a row lock, making an expired lease look live.
func lockAgentForMutation(ctx context.Context, tx pgx.Tx, id string) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT w.control='ACTIVE' AND w.generation=a.generation FROM workflows w JOIN agent_runs a ON a.workflow_id=w.id WHERE a.id=$1 FOR SHARE OF w`, id).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStale
	}
	if err != nil {
		return err
	}
	if !valid {
		return ErrStale
	}
	var lockedID string
	err = tx.QueryRow(ctx, `SELECT id FROM agent_runs WHERE id=$1 FOR UPDATE`, id).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStale
	}
	return err
}
