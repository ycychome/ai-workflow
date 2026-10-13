package store

import (
	"context"
	"errors"
)

// AgentRuntimeSeconds persists elapsed active time across controller crashes.
// Overlapping sessions consume the union of their intervals, while QUEUED
// sessions have no started_at and consume no execution time. STOPPING continues
// charging until the supervisor positively acknowledges the stop.
func (s *Store) AgentRuntimeSeconds(ctx context.Context, workflowID string) (int64, error) {
	var seconds int64
	err := s.DB.QueryRow(ctx, `WITH intervals AS (
 SELECT range_agg(tstzrange(started_at,GREATEST(started_at,COALESCE(stopped_at,clock_timestamp())),'[)')) AS spans
 FROM agent_runs WHERE workflow_id=$1 AND started_at IS NOT NULL
 ), elapsed AS (
 SELECT COALESCE(ceil(sum(extract(epoch FROM upper(span)-lower(span)))),0)::bigint AS seconds
 FROM intervals CROSS JOIN LATERAL unnest(spans) AS pieces(span)
 )
 UPDATE workflow_metrics m SET
 agent_runtime_seconds=GREATEST(m.agent_runtime_seconds,elapsed.seconds),
 runtime_seconds=GREATEST(m.runtime_seconds,m.controller_runtime_seconds+GREATEST(m.agent_runtime_seconds,elapsed.seconds))
 FROM elapsed WHERE m.workflow_id=$1 RETURNING m.runtime_seconds`, workflowID).Scan(&seconds)
	return seconds, err
}

// ChargeControllerRuntime is additive controller-only work (for example the
// credential-free integrator). It is independent of overlapping Agent sessions.
func (s *Store) ChargeControllerRuntime(ctx context.Context, workflowID string, seconds int64) error {
	if seconds < 0 {
		return errors.New("runtime increment cannot be negative")
	}
	tag, err := s.DB.Exec(ctx, `UPDATE workflow_metrics SET controller_runtime_seconds=controller_runtime_seconds+$2,
 runtime_seconds=GREATEST(runtime_seconds,agent_runtime_seconds+controller_runtime_seconds)+$2 WHERE workflow_id=$1`, workflowID, seconds)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return nil
}
