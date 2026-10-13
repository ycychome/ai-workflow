package store

import (
	"context"
	"errors"
	"testing"
)

func TestAgentLimitsRemainAuthoritativeAcrossControllers(t *testing.T) {
	s := agentQueueTestStore(t)
	ctx := context.Background()
	if err := s.InitializeAgentLimits(ctx, 4, 2); err != nil {
		t.Fatal(err)
	}
	seedAgentWorkflow(t, s, "limits-a", 931)
	seedAgentWorkflow(t, s, "limits-b", 932)
	enqueueTestAgent(t, s, "limits-1", "limits-a")
	enqueueTestAgent(t, s, "limits-2", "limits-b")
	first, err := s.ClaimAgent(ctx, "limits-1", "controller-a", 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateAgentLimits(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	// A restarted controller must not overwrite the explicit new limits.
	if err = s.InitializeAgentLimits(ctx, 100, 100); err != nil {
		t.Fatal(err)
	}
	g, w, err := s.AgentLimits(ctx)
	if err != nil || g != 1 || w != 1 {
		t.Fatalf("limits=%d/%d %v", g, w, err)
	}
	if _, err = s.ClaimAgent(ctx, "limits-2", "stale-controller", 100, 100); !errors.Is(err, ErrAgentQueued) {
		t.Fatalf("stale config bypassed global cap: %v", err)
	}
	if err = s.MarkAgentStopping(ctx, first.ID, first.Owner, first.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimAgent(ctx, "limits-2", "stale-controller", 100, 100); !errors.Is(err, ErrAgentQueued) {
		t.Fatalf("unacked process bypassed reduced cap: %v", err)
	}
	if err = s.FinishAgent(ctx, first.ID, first.Owner, first.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimAgent(ctx, "limits-2", "stale-controller", 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if second.WorkflowID != "limits-b" {
		t.Fatal("wrong workflow admitted")
	}
	if err = s.UpdateAgentLimits(ctx, 0, 1); err == nil {
		t.Fatal("invalid limits accepted")
	}
	g, w, err = s.AgentLimits(ctx)
	if err != nil || g != 1 || w != 1 {
		t.Fatalf("invalid update mutated limits=%d/%d %v", g, w, err)
	}
}
