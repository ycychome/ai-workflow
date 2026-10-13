package store

import (
	"context"
	"errors"
)

func validAgentLimits(global, workflow int) bool {
	return global > 0 && workflow > 0 && workflow <= global
}

// InitializeAgentLimits seeds an empty database without overwriting an
// administrator's newer limits when another controller starts with old config.
func (s *Store) InitializeAgentLimits(ctx context.Context, global, workflow int) error {
	return s.writeAgentLimits(ctx, global, workflow, false)
}
func (s *Store) UpdateAgentLimits(ctx context.Context, global, workflow int) error {
	return s.writeAgentLimits(ctx, global, workflow, true)
}
func (s *Store) writeAgentLimits(ctx context.Context, global, workflow int, update bool) error {
	if !validAgentLimits(global, workflow) {
		return errors.New("invalid agent concurrency limits")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1634309990,40)`); err != nil {
		return err
	}
	conflict := ` DO NOTHING`
	if update {
		conflict = ` DO UPDATE SET global_cap=EXCLUDED.global_cap,workflow_cap=EXCLUDED.workflow_cap,updated_at=clock_timestamp()`
	}
	if _, err = tx.Exec(ctx, `INSERT INTO agent_limits(singleton,global_cap,workflow_cap) VALUES(true,$1,$2) ON CONFLICT(singleton)`+conflict, global, workflow); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) AgentLimits(ctx context.Context) (global, workflow int, err error) {
	err = s.DB.QueryRow(ctx, `SELECT global_cap,workflow_cap FROM agent_limits WHERE singleton`).Scan(&global, &workflow)
	return
}
