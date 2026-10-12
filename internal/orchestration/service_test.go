package orchestration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	c "github.com/ycychome/ai-workflow/internal/contracts"
	"github.com/ycychome/ai-workflow/internal/execution"
	gh "github.com/ycychome/ai-workflow/internal/github"
	p "github.com/ycychome/ai-workflow/internal/persistence"
	"github.com/ycychome/ai-workflow/internal/policies"
	"github.com/ycychome/ai-workflow/internal/testutil"
)

func setup(t *testing.T) (*Service, *testutil.GitHub) {
	t.Helper()
	store := testutil.Store(t)
	binary := os.Getenv("M0_TEST_DOCKER")
	if binary == "" {
		t.Skip("set M0_TEST_DOCKER for end-to-end acceptance")
	}
	runner, e := execution.NewDocker(binary, "alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8")
	if e != nil {
		t.Fatal(e)
	}
	runner.Namespace = fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	t.Cleanup(runner.Close)
	remote := testutil.NewGitHub(t)
	s := &Service{Store: store, GitHub: remote.Client(), Runner: runner, Repository: "o/r", RepositoryID: 1, BaseBranch: "main", Label: "ai:m0", WorkspaceRoot: t.TempDir(), MaxAttempts: 4}
	return s, remote
}
func intake(t *testing.T, s *Service) {
	t.Helper()
	secret := strings.Repeat("s", 32)
	gateway := gh.Gateway{Store: s.Store, Secret: []byte(secret), Repository: "o/r", RepositoryID: 1, InstallationID: 22}
	body := []byte(`{"action":"opened","repository":{"id":1,"full_name":"o/r"},"installation":{"id":22},"issue":{"number":1}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("POST", "/webhooks/github", bytes.NewReader(body))
		r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		r.Header.Set("X-GitHub-Event", "issues")
		r.Header.Set("X-GitHub-Delivery", "same-delivery")
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, r)
		if w.Code != 202 {
			t.Fatalf("webhook %d: %s", w.Code, w.Body.String())
		}
	}
	if e := s.ProcessInbox(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e := s.Handle(t.Context(), p.Event{ID: "different-delivery-same-issue", Kind: "issues", Payload: body}); e != nil {
		t.Fatal(e)
	}
	all, e := s.Store.All(t.Context())
	if e != nil || len(all) != 1 {
		t.Fatalf("duplicate workflows: %v %v", all, e)
	}
}
func retryNow(t *testing.T, s *Service) {
	t.Helper()
	if _, e := s.Store.Pool.Exec(t.Context(), `UPDATE outbox SET available_at=now() WHERE status='unknown'`); e != nil {
		t.Fatal(e)
	}
}
func drain(t *testing.T, s *Service) {
	t.Helper()
	for i := 0; i < 15; i++ {
		var n int
		s.Store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox WHERE status IN ('pending','unknown')`).Scan(&n)
		if n == 0 {
			return
		}
		e := s.Publish(t.Context())
		if e != nil && !errors.Is(e, c.ErrStale) {
			t.Fatal(e)
		}
	}
	t.Fatal("outbox never settled")
}
func TestSignedIssueToFixedPatchPRGateAndLostResponses(t *testing.T) {
	s, remote := setup(t)
	intake(t, s)
	remote.LosePR = true
	remote.LoseComment = true
	if e := s.Work(t.Context()); e != nil {
		t.Fatal(e)
	}
	w, _ := s.Store.Get(t.Context(), "r1-i1")
	if w.Attempt.Status != "OUTPUT_READY" || w.PullRequest != 0 {
		t.Fatal("execution incorrectly considered integrated")
	}
	if e := s.Publish(t.Context()); e == nil {
		t.Fatal("PR response loss not injected")
	}
	// Drop pooled connections and recover from durable contracts, with no process memory receipt.
	s.Store.Pool.Reset()
	if e := s.Store.RecoverExecutions(t.Context()); e != nil {
		t.Fatal(e)
	}
	retryNow(t, s)
	if e := s.Publish(t.Context()); e != nil {
		t.Fatal(e)
	}
	w, _ = s.Store.Get(t.Context(), "r1-i1")
	if w.Phase != c.Reviewing || w.PullRequest != 2 || w.HeadSHA != "head-A" {
		t.Fatalf("candidate not integrated: %+v", w)
	}
	if e := s.Publish(t.Context()); e != nil {
		t.Fatal(e)
	} // Pending check must block before review.
	if e := s.Work(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e := s.Publish(t.Context()); e != nil {
		t.Fatal(e)
	} // PASS
	if e := s.Publish(t.Context()); e == nil {
		t.Fatal("comment response loss not injected")
	}
	retryNow(t, s)
	drain(t, s)
	prs, comments, checks := remote.Counts()
	if prs != 1 || comments != 1 || checks != 1 {
		t.Fatalf("uncontrolled duplicate effects: PR=%d comments=%d checks=%d", prs, comments, checks)
	}
	w, _ = s.Store.Get(t.Context(), "r1-i1")
	if w.Phase != c.AwaitingMaintainer || w.Key.HeadSHA != w.HeadSHA {
		t.Fatal("missing current verification identity")
	}
	remote.ConclusionsCheck(t, "success")
	var reports int
	if e := s.Store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM run_evidence`).Scan(&reports); e != nil || reports != 2 {
		t.Fatalf("durable execution evidence missing: %d %v", reports, e)
	}
}
func greetingFixture(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{"app.py", "test_app.py"} {
		b, e := os.ReadFile(filepath.Join("..", "..", "fixtures", "repository", name))
		if e != nil {
			t.Fatal(e)
		}
		files[name] = string(b)
	}
	return files
}
func TestGreetingServiceHTTPAcceptanceDrivesMergeGate(t *testing.T) {
	for _, faulty := range []bool{false, true} {
		name := "working HTTP service"
		if faulty {
			name = "forged tests with broken HTTP service"
		}
		t.Run(name, func(t *testing.T) {
			s, remote := setup(t)
			s.VerificationProfile = "greeting-service"
			s.Runner.Image = policies.ServiceImage
			remote.SourceFiles = greetingFixture(t)
			if faulty {
				// A candidate can claim its own regression suite passed. The trusted controller's
				// HTTP assertions must still detect a service that ignores the greeting config.
				remote.SourceFiles["app.py"] = strings.Replace(remote.SourceFiles["app.py"], `greeting = self.server.greeting_file.read_text(encoding="utf-8").strip()`, `greeting = "wrong"`, 1)
				remote.SourceFiles["test_app.py"] = "import unittest\n"
			}
			intake(t, s)
			if e := s.Work(t.Context()); e != nil {
				t.Fatal(e)
			}
			drain(t, s)
			if e := s.Work(t.Context()); e != nil {
				t.Fatal(e)
			}
			drain(t, s)
			w, e := s.Store.Get(t.Context(), "r1-i1")
			if e != nil {
				t.Fatal(e)
			}
			want := c.AwaitingMaintainer
			conclusion := "success"
			if faulty {
				want = c.Blocked
				conclusion = "failure"
			}
			if w.Phase != want {
				t.Fatalf("phase %s want %s, reason %s", w.Phase, want, w.Reason)
			}
			remote.ConclusionsCheck(t, conclusion)
			if !faulty {
				var payload []byte
				if e = s.Store.Pool.QueryRow(t.Context(), `SELECT data->'payload' FROM outbox WHERE kind='gate' AND data->'payload'->>'verdict'='PASS'`).Scan(&payload); e != nil {
					t.Fatal(e)
				}
				var gate p.GatePayload
				if e = json.Unmarshal(payload, &gate); e != nil {
					t.Fatal(e)
				}
				if gate.Evidence == nil || len(gate.Evidence.Coverage) != 5 {
					t.Fatal("HTTP obligations missing from coverage ledger")
				}
				for _, obligation := range gate.Evidence.Coverage {
					var n int
					if e = s.Store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM run_evidence WHERE digest=$1`, obligation.Evidence).Scan(&n); e != nil || n != 1 {
						t.Fatal("HTTP evidence cannot be resolved")
					}
				}
			}
		})
	}
}
func TestTrustedProfileChangeInvalidatesSameSHA(t *testing.T) {
	s, remote := setup(t)
	intake(t, s)
	if e := s.Work(t.Context()); e != nil {
		t.Fatal(e)
	}
	drain(t, s)
	if e := s.Work(t.Context()); e != nil {
		t.Fatal(e)
	}
	drain(t, s)
	old, _ := s.Store.Get(t.Context(), "r1-i1")
	s.VerificationProfile = "greeting-service"
	s.Runner.Image = policies.ServiceImage
	if e := s.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	drain(t, s)
	current, _ := s.Store.Get(t.Context(), "r1-i1")
	if current.HeadSHA != old.HeadSHA || current.Generation <= old.Generation || current.Key.ProfileDigest == old.Key.ProfileDigest || current.Phase != c.Reviewing {
		t.Fatal("profile change silently reused previous PASS")
	}
	remote.ConclusionsCheck(t, "failure")
}
func TestMissedInitialWebhookIsDiscovered(t *testing.T) {
	s, _ := setup(t)
	if e := s.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	w, e := s.Store.Get(t.Context(), "r1-i1")
	if e != nil || w.Phase != c.Intake {
		t.Fatalf("missing webhook task not recovered: %+v %v", w, e)
	}
}
func TestMissedIssueCloseAndPRDraftStopTheWorkflow(t *testing.T) {
	t.Run("Issue close", func(t *testing.T) {
		s, remote := setup(t)
		intake(t, s)
		remote.SetIssueState("closed")
		if e := s.Reconcile(t.Context()); e != nil {
			t.Fatal(e)
		}
		s.Reconcile(t.Context())
		w, _ := s.Store.Get(t.Context(), "r1-i1")
		if w.Control != c.Cancelled || w.Phase == c.Merged {
			t.Fatal("missed closed Issue did not stop")
		}
	})
	t.Run("PR draft", func(t *testing.T) {
		s, remote := setup(t)
		intake(t, s)
		if e := s.Work(t.Context()); e != nil {
			t.Fatal(e)
		}
		drain(t, s)
		remote.SetDraft(false)
		if e := s.Reconcile(t.Context()); e != nil {
			t.Fatal(e)
		}
		remote.SetDraft(true)
		if e := s.Reconcile(t.Context()); e != nil {
			t.Fatal(e)
		}
		w, _ := s.Store.Get(t.Context(), "r1-i1")
		if w.Control != c.Pausing {
			t.Fatal("return to draft did not revoke execution")
		}
	})
}
func TestUnknownPRResponseAfterPauseIsReconciledWithoutNewWrites(t *testing.T) {
	s, remote := setup(t)
	intake(t, s)
	remote.LosePR = true
	if e := s.Work(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e := s.Publish(t.Context()); e == nil {
		t.Fatal("response loss not injected")
	}
	if e := s.Store.Control(t.Context(), "r1-i1", "pause"); e != nil {
		t.Fatal(e)
	}
	if s.Store.ConfirmStop(t.Context(), "r1-i1") == nil {
		t.Fatal("unknown write hidden")
	}
	remote.MoveBase("base-after-pause")
	retryNow(t, s)
	drain(t, s)
	if e := s.Store.ConfirmStop(t.Context(), "r1-i1"); e != nil {
		t.Fatal(e)
	}
	w, _ := s.Store.Get(t.Context(), "r1-i1")
	if w.Control != c.Paused || w.PullRequest != 0 {
		t.Fatal("old result advanced paused workflow")
	}
	prs, _, _ := remote.Counts()
	if prs != 1 {
		t.Fatal("old candidate recreated")
	}
}
func TestHumanPushAfterUnknownPRResponseIsNeverOverwritten(t *testing.T) {
	s, remote := setup(t)
	intake(t, s)
	remote.LosePR = true
	s.Work(t.Context())
	if e := s.Publish(t.Context()); e == nil {
		t.Fatal("response loss not injected")
	}
	remote.MoveHead("human-commit")
	retryNow(t, s)
	if e := s.Publish(t.Context()); !errors.Is(e, c.ErrStale) {
		t.Fatalf("human commit adopted as bot artifact: %v", e)
	}
	w, _ := s.Store.Get(t.Context(), "r1-i1")
	if w.Phase != c.Blocked {
		t.Fatal("conflicting publisher did not stop")
	}
	prs, _, _ := remote.Counts()
	if prs != 1 {
		t.Fatal("duplicate PR or overwrite")
	}
}
func TestCurrentSnapshotWinsOverOutOfOrderEventsAndSameSHARereview(t *testing.T) {
	s, remote := setup(t)
	intake(t, s)
	if e := s.Work(t.Context()); e != nil {
		t.Fatal(e)
	}
	drain(t, s)
	if e := s.Work(t.Context()); e != nil {
		t.Fatal(e)
	}
	drain(t, s)
	previous, _ := s.Store.Get(t.Context(), "r1-i1")
	// Simulate a missed base push. Periodic reconciliation sees it without a webhook.
	remote.MoveBase("base-B")
	if e := s.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	drain(t, s)
	current, _ := s.Store.Get(t.Context(), "r1-i1")
	if current.Generation <= previous.Generation || current.Key.BaseSHA != "base-B" || current.HeadSHA != previous.HeadSHA {
		t.Fatal("same-SHA identity failed to invalidate")
	}
	remote.ConclusionsCheck(t, "failure")
	if e := s.Work(t.Context()); e != nil {
		t.Fatal(e)
	}
	drain(t, s)
	_, _, checks := remote.Counts()
	if checks != 1 {
		t.Fatal("same-SHA re-review created ambiguous duplicate checks")
	}
	remote.MoveHead("head-B")
	stale := []byte(`{"action":"synchronize","pull_request":{"number":2,"head":{"sha":"head-A"}}}`)
	if e := s.Handle(t.Context(), p.Event{Kind: "pull_request", Payload: stale}); e != nil {
		t.Fatal(e)
	}
	current, _ = s.Store.Get(t.Context(), "r1-i1")
	if current.HeadSHA != "head-B" || current.Key.HeadSHA != "head-B" {
		t.Fatal("event payload rolled back current snapshot")
	}
	if v, _ := c.Gate(current.Key, c.Active, c.Evidence{Key: previous.Key}, Required); v != c.Obsolete {
		t.Fatal("old review gave new HEAD green")
	}
}
func TestHumanCommandsAndClosedPRDoNotClaimMerge(t *testing.T) {
	s, remote := setup(t)
	intake(t, s)
	command := func(id int, kind string) p.Event {
		b, _ := json.Marshal(map[string]any{"action": "created", "issue": map[string]any{"number": 1}, "comment": map[string]any{"id": id, "body": "/ai pause", "user": map[string]any{"login": "person", "type": kind}}})
		return p.Event{Kind: "issue_comment", Payload: b}
	}
	remote.Allowed = false
	if e := s.Handle(t.Context(), command(10, "User")); e != nil {
		t.Fatal(e)
	}
	if e := s.Handle(t.Context(), command(11, "Bot")); e != nil {
		t.Fatal(e)
	}
	w, _ := s.Store.Get(t.Context(), "r1-i1")
	if w.Generation != 1 || w.Control != c.Active {
		t.Fatal("unauthorized control applied")
	}
	remote.Allowed = true
	if e := s.Handle(t.Context(), command(12, "User")); e != nil {
		t.Fatal(e)
	}
	s.Handle(t.Context(), command(12, "User"))
	w, _ = s.Store.Get(t.Context(), "r1-i1")
	if w.Generation != 2 || w.Control != c.Pausing {
		t.Fatal("authorized command not fenced/deduplicated")
	}
	if e := s.Store.ConfirmStop(t.Context(), w.ID); e != nil {
		t.Fatal(e)
	}
	s.Store.Control(t.Context(), w.ID, "resume")
	if e := s.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e := s.Work(t.Context()); e != nil {
		t.Fatal(e)
	}
	drain(t, s)
	remote.SetPRState("closed", false)
	if e := s.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	w, _ = s.Store.Get(t.Context(), w.ID)
	if w.Phase == c.Merged {
		t.Fatal("closed unmerged PR treated as delivered")
	}
	remote.SetPRState("closed", true)
	s.Store.Update(context.Background(), w.ID, func(w *c.Workflow) ([]c.Action, error) { w.Control = c.Active; return nil, nil })
	if e := s.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	w, _ = s.Store.Get(t.Context(), w.ID)
	if w.Phase != c.Merged {
		t.Fatal("real merged fact ignored")
	}
}
