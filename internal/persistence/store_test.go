package persistence_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	c "github.com/ycychome/ai-workflow/internal/contracts"
	p "github.com/ycychome/ai-workflow/internal/persistence"
	"github.com/ycychome/ai-workflow/internal/testutil"
)

func workflow() c.Workflow {
	return c.Workflow{ID: "r1-i1", Repository: "o/r", RepositoryID: 1, Issue: 1, SpecDigest: "spec", Generation: 1, Version: 1, Phase: c.Intake, Control: c.Active}
}
func TestInboxDedupRetryAndLeaseRecovery(t *testing.T) {
	s := testutil.Store(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if e := s.Receive(ctx, "delivery", "issues", []byte(`{}`)); e != nil {
			t.Fatal(e)
		}
	}
	ev, e := s.ClaimEvent(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ClaimEvent(ctx); !errors.Is(e, pgx.ErrNoRows) {
		t.Fatal("duplicate claimed")
	}
	if e = s.FinishEvent(ctx, ev, errors.New("crash before completion")); e != nil {
		t.Fatal(e)
	}
	s.Pool.Exec(ctx, `UPDATE inbox SET available_at=now()`)
	second, e := s.ClaimEvent(ctx)
	if e != nil || second.Epoch <= ev.Epoch {
		t.Fatalf("failed delivery lost: %v", e)
	}
	s.Pool.Exec(ctx, `UPDATE inbox SET lease_until=now()-interval '1 second'`)
	third, e := s.ClaimEvent(ctx)
	if e != nil || third.Epoch <= second.Epoch {
		t.Fatal("expired processing not recovered")
	}
	s.FinishEvent(ctx, second, nil)
	var status string
	s.Pool.QueryRow(ctx, `SELECT status FROM inbox`).Scan(&status)
	if status != "processing" {
		t.Fatal("old consumer completed new lease")
	}
	s.FinishEvent(ctx, third, nil)
	if _, e = s.ClaimEvent(ctx); !errors.Is(e, pgx.ErrNoRows) {
		t.Fatal("processed delivery replayed")
	}
}
func TestOneLeaseAndStaleWorkerFencing(t *testing.T) {
	s := testutil.Store(t)
	ctx := context.Background()
	if e := s.Create(ctx, workflow()); e != nil {
		t.Fatal(e)
	}
	var count atomic.Int32
	var wg sync.WaitGroup
	var winner c.Workflow
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, e := s.ClaimWork(ctx, "r1-i1", time.Now(), 4)
			if e == nil {
				count.Add(1)
				mu.Lock()
				winner = w
				mu.Unlock()
			} else if !errors.Is(e, c.ErrStale) {
				t.Errorf("claim: %v", e)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("%d lease owners", count.Load())
	}
	s.Update(ctx, winner.ID, func(w *c.Workflow) ([]c.Action, error) {
		w.Attempt.ExpiresAt = time.Now().Add(-time.Second)
		return nil, nil
	})
	next, e := s.ClaimWork(ctx, winner.ID, time.Now(), 4)
	if e != nil {
		t.Fatal(e)
	}
	if next.Attempt.Epoch != winner.Attempt.Epoch+1 {
		t.Fatal("epoch not advanced")
	}
	if next.Accept(winner.Attempt, time.Now()) == nil {
		t.Fatal("old worker accepted")
	}
}
func TestPauseUnknownWritesAndCommandEffectDedup(t *testing.T) {
	s := testutil.Store(t)
	ctx := context.Background()
	w := workflow()
	s.Create(ctx, w)
	s.Update(ctx, w.ID, func(w *c.Workflow) ([]c.Action, error) {
		return []c.Action{p.Action(*w, "candidate", "patch", c.Candidate{})}, nil
	})
	a, e := s.ClaimAction(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s.FinishAction(ctx, a, nil, errors.New("remote succeeded; response lost"))
	if e = s.ControlOnce(ctx, w.ID, "pause", 99); e != nil {
		t.Fatal(e)
	}
	s.ControlOnce(ctx, w.ID, "pause", 99)
	current, _ := s.Get(ctx, w.ID)
	if current.Generation != 2 {
		t.Fatal("same human command applied twice")
	}
	if s.Guard(ctx, a) == nil {
		t.Fatal("paused writer allowed")
	}
	if s.ConfirmStop(ctx, w.ID) == nil {
		t.Fatal("uncertain remote write hidden")
	}
	current, _ = s.Get(ctx, w.ID)
	if current.Control != c.Pausing {
		t.Fatal("premature PAUSED")
	}
	s.FinishAction(ctx, a, map[string]any{"pr": 1}, nil)
	if e = s.ConfirmStop(ctx, w.ID); e != nil {
		t.Fatal(e)
	}
	current, _ = s.Get(ctx, w.ID)
	if current.Control != c.Paused {
		t.Fatal("stop never confirmed")
	}
	if current.Accept(c.Attempt{Generation: 1}, time.Now()) == nil {
		t.Fatal("old result allowed after stop")
	}
}
func TestBusinessIdempotencyBudgetAndRestart(t *testing.T) {
	s := testutil.Store(t)
	ctx := context.Background()
	w := workflow()
	if e := s.Create(ctx, w); e != nil {
		t.Fatal(e)
	}
	if e := s.Create(ctx, w); e != nil {
		t.Fatal(e)
	}
	other := w
	other.ID = "r1-i2"
	other.Issue = 2
	if s.Create(ctx, other) == nil {
		t.Fatal("repository concurrency exceeded")
	}
	first, e := s.ClaimWork(ctx, w.ID, time.Now(), 2)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RecoverExecutions(ctx); e != nil {
		t.Fatal(e)
	}
	second, e := s.ClaimWork(ctx, w.ID, time.Now(), 2)
	if e != nil {
		t.Fatal(e)
	}
	if second.Attempt.Epoch <= first.Attempt.Epoch {
		t.Fatal("restart reused old execution")
	}
	s.Control(ctx, w.ID, "pause")
	s.MarkStopped(ctx, w.ID, second.Attempt.ID)
	s.ConfirmStop(ctx, w.ID)
	s.Control(ctx, w.ID, "resume")
	s.Update(ctx, w.ID, func(w *c.Workflow) ([]c.Action, error) {
		w.Control = c.Active
		w.Attempt.Status = "SUPERSEDED"
		return nil, nil
	})
	if _, e = s.ClaimWork(ctx, w.ID, time.Now(), 2); !errors.Is(e, c.ErrStale) {
		t.Fatal("global budget reset")
	}
	current, _ := s.Get(ctx, w.ID)
	if current.Runs != 2 || current.Phase != c.Blocked {
		t.Fatal("budget exhaustion passed")
	}
}
func TestDispatchLeaseFencesOldPublisher(t *testing.T) {
	s := testutil.Store(t)
	ctx := context.Background()
	w := workflow()
	s.Create(ctx, w)
	s.Update(ctx, w.ID, func(w *c.Workflow) ([]c.Action, error) {
		return []c.Action{p.Action(*w, "comment", "status", "hello")}, nil
	})
	old, _ := s.ClaimAction(ctx)
	s.Pool.Exec(ctx, `UPDATE outbox SET lease_until=now()-interval '1 second'`)
	current, e := s.ClaimAction(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if s.Guard(ctx, old) == nil {
		t.Fatal("old publisher permitted")
	}
	if e = s.Guard(ctx, current); e != nil {
		t.Fatal(e)
	}
	s.FinishAction(ctx, old, nil, nil)
	var status string
	s.Pool.QueryRow(ctx, `SELECT status FROM outbox`).Scan(&status)
	if status != "sending" {
		t.Fatal("stale publisher erased current dispatch")
	}
}
func TestDelayedBlockingProjectionCannotOverwriteCurrentPASS(t *testing.T) {
	s := testutil.Store(t)
	ctx := t.Context()
	w := workflow()
	w.Phase = c.Reviewing
	s.Create(ctx, w)
	s.Update(ctx, w.ID, func(w *c.Workflow) ([]c.Action, error) {
		return []c.Action{p.Action(*w, "gate", "revoke", p.GatePayload{Verdict: c.Fail})}, nil
	})
	old, e := s.ClaimAction(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s.Update(ctx, w.ID, func(w *c.Workflow) ([]c.Action, error) { w.Phase = c.AwaitingMaintainer; return nil, nil })
	if s.Guard(ctx, old) == nil {
		t.Fatal("obsolete blocking projection may overwrite current PASS")
	}
}
