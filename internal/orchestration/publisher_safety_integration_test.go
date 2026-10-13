package orchestration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/store"
)

func safetyGitHubClient(t *testing.T, base string, client *http.Client) *gh.Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := gh.New(gh.Config{AppID: "711", BaseURL: base, HTTPClient: client, PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestRemoteReadyReceiptCreatesAndPublishesManualGate(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	mock := newGateGitHubMock()
	readyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			mock.mu.Lock()
			mock.pr.Draft = false
			readyCalls++
			mock.mu.Unlock()
			writeGateJSON(rw, map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"pullRequest": map[string]string{"id": "node"}}}})
			return
		}
		mock.ServeHTTP(rw, r)
	}))
	defer server.Close()
	e.app = safetyGitHubClient(t, server.URL, server.Client())
	e.cfg.Repositories = []config.Repository{{Name: "acme/pilot", ID: 77, BaseBranch: "main", AllowedPaths: []string{"file.txt"}, Verification: [][]string{{"go", "test", "./..."}}, PolicyVersion: "safety-test"}}
	w := gateIdentityFixture(e, 201)
	w.Phase = "BLOCKED"
	w.Control = "PAUSED"
	w.PRReady = false
	w.Spec.SourceHash = normalizedIssueSourceHash(w.IssueTitle, w.IssueBody)
	w.Spec.Digest = domain.SpecDigest(w.Spec)
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	pr := gateMockPR(w)
	pr.Draft = true
	pr.NodeID = "node"
	mock.setPR(pr, nil)
	mock.setArchive(w.BaseSHA, map[string]string{"file.txt": "old"})
	mock.setArchive(w.HeadSHA, map[string]string{"file.txt": "new"})
	if err := e.Command(ctx, w.ID, "maintainer", "/ai forceaccept merge", true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := e.Store.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id=$1 AND kind='gate' AND data->>'pass'='true'`, w.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("gate created before ready: %d %v", count, err)
	}
	var readyID string
	if err := e.Store.DB.QueryRow(ctx, `SELECT id FROM outbox WHERE workflow_id=$1 AND kind='ready' AND state='pending'`, w.ID).Scan(&readyID); err != nil {
		t.Fatal(err)
	}
	ready := setPrePRActionRunning(t, ctx, e.Store, readyID)
	for i := 0; i < 2; i++ {
		if _, err, unknown := e.performAction(ctx, ready); err != nil || unknown {
			t.Fatalf("ready receipt/replay failed: %v unknown=%v", err, unknown)
		}
	}
	if err := e.Store.FinishAction(ctx, ready, map[string]bool{"ready": true}, nil, false); err != nil {
		t.Fatal(err)
	}
	mock.mu.Lock()
	calls := readyCalls
	draft := mock.pr.Draft
	mock.mu.Unlock()
	if calls != 1 || draft {
		t.Fatalf("ready mutation was not idempotent: calls=%d draft=%v", calls, draft)
	}
	if err := e.Store.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE workflow_id=$1 AND kind='gate' AND data->>'pass'='true'`, w.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ready child count=%d err=%v", count, err)
	}
	gate := setPrePRActionRunning(t, ctx, e.Store, readyID+":after-ready:gate")
	receipt, err, unknown := e.performAction(ctx, gate)
	if err != nil || unknown {
		t.Fatalf("child gate did not publish: %v unknown=%v", err, unknown)
	}
	if err := e.Store.FinishAction(ctx, gate, receipt, nil, false); err != nil {
		t.Fatal(err)
	}
	got, err := e.Store.Get(ctx, w.ID)
	if err != nil || !got.PRReady || got.GateConclusion != "success" || got.Review.Verdict != "FORCED" || mock.successfulChecks() != 1 {
		t.Fatalf("manual gate not persisted: %+v err=%v", got, err)
	}
}

func TestPublisherCancellationPersistsUnknownAndReleasesNextAction(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := domain.Workflow{ID: "publisher-timeout", RepositoryID: 992345, Repository: "acme/pilot", InstallationID: 42, Issue: 1, Control: "ACTIVE", Phase: "INTAKE"}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	action := store.NewAction(w, "comment", commentAction{Body: "bounded write"})
	if err := e.Store.Update(ctx, w.ID, func(*domain.Workflow) ([]store.Action, error) { return []store.Action{action}, nil }); err != nil {
		t.Fatal(err)
	}
	action = setPrePRActionRunning(t, ctx, e.Store, action.ID)
	writeStarted := make(chan struct{})
	e.app = safetyGitHubClient(t, "http://fixture.invalid", &http.Client{Transport: safetyRegressionTransport(func(r *http.Request) (*http.Response, error) {
		body := "{}"
		switch r.URL.Path {
		case "/app":
			body = `{"slug":"workflow"}`
		case "/app/installations/42/access_tokens":
			body = `{"token":"fixture","expires_at":"2099-01-01T00:00:00Z"}`
		case "/repos/acme/pilot/issues/1/comments":
			if r.Method == http.MethodGet {
				body = "[]"
			} else {
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 61*time.Second {
					t.Error("publisher has no bounded effect deadline")
				}
				close(writeStarted)
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
		default:
			t.Errorf("unexpected fixture request: %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { e.processOutboxAction(runCtx, action); close(done) }()
	select {
	case <-writeStarted:
		cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("publisher did not reach write")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("publisher cancellation did not release action")
	}
	var state, message string
	if err := e.Store.DB.QueryRow(ctx, "SELECT state,error FROM outbox WHERE id=$1", action.ID).Scan(&state, &message); err != nil || state != "unknown" || message == "" {
		t.Fatalf("canceled write lost unknown receipt: state=%s error=%s err=%v", state, message, err)
	}
	local := domain.Workflow{ID: "publisher-next", RepositoryID: 992345, Issue: 2, Repository: "local/pilot", Control: "ACTIVE", Phase: "INTAKE"}
	if err := e.Store.Create(ctx, local); err != nil {
		t.Fatal(err)
	}
	next := store.NewAction(local, "comment", commentAction{Body: "next action"})
	if err := e.Store.Update(ctx, local.ID, func(*domain.Workflow) ([]store.Action, error) { return []store.Action{next}, nil }); err != nil {
		t.Fatal(err)
	}
	next = setPrePRActionRunning(t, ctx, e.Store, next.ID)
	e.processOutboxAction(ctx, next)
	if err := e.Store.DB.QueryRow(ctx, "SELECT state FROM outbox WHERE id=$1", next.ID).Scan(&state); err != nil || state != "done" {
		t.Fatalf("subsequent action did not complete: %s %v", state, err)
	}
}
