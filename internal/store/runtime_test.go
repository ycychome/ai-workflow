package store

import (
	"context"
	"testing"
)

func TestAgentRuntimePersistsUnionAndExcludesQueueWait(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "runtime", 941)
	enqueueTestAgent(t, s, "runtime-a", "runtime")
	enqueueTestAgent(t, s, "runtime-b", "runtime")
	enqueueTestAgent(t, s, "runtime-queued", "runtime")
	// Fixed historical intervals avoid wall-clock-dependent expectations:
	// [0,10.2) union [5,15.1) is 15.1 seconds, rounded once to 16.
	if _, err := s.DB.Exec(ctx, `UPDATE agent_runs SET status='FINISHED',started_at='2025-01-01 00:00:00+00',stopped_at='2025-01-01 00:00:10.2+00' WHERE id='runtime-a';
 UPDATE agent_runs SET status='FINISHED',started_at='2025-01-01 00:00:05+00',stopped_at='2025-01-01 00:00:15.1+00' WHERE id='runtime-b';
 UPDATE agent_runs SET enqueued_at='2020-01-01 00:00:00+00' WHERE id='runtime-queued'`); err != nil {
		t.Fatal(err)
	}
	total, err := s.AgentRuntimeSeconds(ctx, "runtime")
	if err != nil || total != 16 {
		t.Fatalf("union runtime=%d %v", total, err)
	}
	if err = s.ChargeControllerRuntime(ctx, "runtime", 7); err != nil {
		t.Fatal(err)
	}
	// A second Store is the restarted process: no in-memory clock is available.
	restarted := &Store{DB: s.DB}
	total, err = restarted.AgentRuntimeSeconds(ctx, "runtime")
	if err != nil || total != 23 {
		t.Fatalf("restart runtime=%d %v", total, err)
	}
	if err = s.Runtime(ctx, "runtime", 2); err != nil {
		t.Fatal(err)
	}
	total, err = restarted.AgentRuntimeSeconds(ctx, "runtime")
	if err != nil || total != 25 {
		t.Fatalf("legacy additive runtime=%d %v", total, err)
	}
	var persisted int64
	if err = s.DB.QueryRow(ctx, `SELECT runtime_seconds FROM workflow_metrics WHERE workflow_id='runtime'`).Scan(&persisted); err != nil || persisted != 25 {
		t.Fatalf("persisted runtime=%d %v", persisted, err)
	}
	if err = s.MigrateAgentQueue(ctx); err != nil {
		t.Fatal(err)
	}
	total, err = s.AgentRuntimeSeconds(ctx, "runtime")
	if err != nil || total != 25 {
		t.Fatalf("repeat migration double-counted=%d %v", total, err)
	}
}

func TestAgentRuntimeChargesStoppingUntilAcknowledged(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "stopping-clock", 942)
	enqueueTestAgent(t, s, "crashed-run", "stopping-clock")
	if _, err := s.DB.Exec(ctx, `UPDATE agent_runs SET status='STOPPING',owner='dead-controller',lease_epoch=1,started_at=clock_timestamp()-interval '20 seconds',lease_until=NULL WHERE id='crashed-run'`); err != nil {
		t.Fatal(err)
	}
	total, err := s.AgentRuntimeSeconds(ctx, "stopping-clock")
	if err != nil || total < 20 {
		t.Fatalf("unacked elapsed=%d %v", total, err)
	}
	// No resource/model was launched: this explicit acknowledgement closes the
	// persisted interval. Subsequent reads must not continue charging it.
	if err = s.FinishAgent(ctx, "crashed-run", "dead-controller", 1); err != nil {
		t.Fatal(err)
	}
	stopped, err := s.AgentRuntimeSeconds(ctx, "stopping-clock")
	if err != nil || stopped < total {
		t.Fatalf("stop reduced runtime=%d previous=%d %v", stopped, total, err)
	}
	for i := 0; i < 3; i++ {
		next, err := s.AgentRuntimeSeconds(ctx, "stopping-clock")
		if err != nil || next != stopped {
			t.Fatalf("finished runtime drift=%d expected=%d %v", next, stopped, err)
		}
	}
}

func TestRuntimeMigrationPreservesLegacyChargedTime(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	seedAgentWorkflow(t, s, "legacy-runtime", 943)
	if _, err := s.DB.Exec(ctx, `DELETE FROM schema_migrations WHERE version=8; UPDATE workflow_metrics SET runtime_seconds=37,controller_runtime_seconds=0,agent_runtime_seconds=0 WHERE workflow_id='legacy-runtime'`); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateAgentQueue(ctx); err != nil {
		t.Fatal(err)
	}
	total, err := s.AgentRuntimeSeconds(ctx, "legacy-runtime")
	if err != nil || total != 37 {
		t.Fatalf("legacy runtime lost=%d %v", total, err)
	}
	if err = s.MigrateAgentQueue(ctx); err != nil {
		t.Fatal(err)
	}
	total, err = s.AgentRuntimeSeconds(ctx, "legacy-runtime")
	if err != nil || total != 37 {
		t.Fatalf("legacy migration repeated=%d %v", total, err)
	}
}
