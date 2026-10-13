package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunCancellationPreservesOutputWithoutSuccessDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Create(ctx, t.Name(), map[string]File{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Destroy(context.Background(), w)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		result RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, e := m.Run(runCtx, w, []string{"sh", "-ceu", `printf 'candidate-stdout-marker\n'; printf 'candidate-stderr-marker\n' >&2; printf ready > ready.txt; exec sleep 60`}, 90*time.Second)
		done <- outcome{r, e}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if data, err := m.ReadFile(ctx, w, "ready.txt"); err == nil && string(data) == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never reached cancellation checkpoint")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case out := <-done:
		if !errors.Is(out.err, context.Canceled) || out.result.ExitCode == 0 || out.result.TimedOut || !strings.Contains(out.result.Stdout, "candidate-stdout-marker") || !strings.Contains(out.result.Stderr, "candidate-stderr-marker") {
			t.Fatalf("cancelled diagnostics lost or treated as success: %+v %v", out.result, out.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelled worker did not stop")
	}
	if err = m.StopRun(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
}

func TestJudgePeerSurvivesCandidateKillAllDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := m.CreateWithServices(ctx, t.Name(), map[string]File{"candidate-only": {Data: []byte("candidate")}}, []string{"postgres"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.StopRun(context.Background(), candidate.ID)
	if err = m.SealReadOnly(ctx, candidate, false); err != nil {
		t.Fatal(err)
	}
	peer, err := m.CreateJudgePeer(ctx, candidate, map[string]File{"trusted-marker": {Data: []byte("peer-survived")}})
	if err != nil {
		t.Fatal(err)
	}
	attacker, attackErr := m.Run(ctx, candidate, []string{"sh", "-c", `test ! -e trusted-marker || exit 99; printf 'private-source-hidden\n'; kill -KILL -1; exit 23`}, 10*time.Second)
	if attackErr != nil || !strings.Contains(attacker.Stdout, "private-source-hidden") || attacker.ExitCode == 99 {
		t.Fatalf("candidate did not execute namespace attack: %+v %v", attacker, attackErr)
	}
	result, err := m.Run(ctx, peer, []string{"sh", "-ceu", `test ! -e candidate-only; test "$(cat trusted-marker)" = peer-survived; psql "$DATABASE_URL" -X -At -c 'SELECT 42'; printf peer-survived`}, 10*time.Second)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "42") || !strings.Contains(result.Stdout, "peer-survived") {
		t.Fatalf("candidate kill crossed namespace: %+v %v", result, err)
	}
	if err = m.StopRun(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
}
