package orchestration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/store"
)

// TestGateIdentityAndPublishWebhookPostgresIntegration uses a mock GitHub HTTP
// server and an isolated PostgreSQL schema. It never calls a live GitHub API or
// a model endpoint.
func TestGateIdentityAndPublishWebhookPostgresIntegration(t *testing.T) {
	if os.Getenv("AI_WORKFLOW_FORCE_INTEGRATION") != "1" {
		t.Skip("set AI_WORKFLOW_FORCE_INTEGRATION=1 in the controlled integration environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		t.Fatal("CONFIG_FILE must point to the controlled runtime config")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load controlled config: %v", err)
	}
	admin, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("connect controlled database: %v", err)
	}
	defer admin.Close()

	schema := fmt.Sprintf("gate_identity_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create isolated test schema: %v", err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
	}()
	dbURL, err := gateIdentitySchemaURL(cfg.DatabaseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open isolated test store: %v", err)
	}
	defer s.Close()

	mock := newGateGitHubMock()
	server := httptest.NewServer(mock)
	defer server.Close()
	cfg.GitHub.AppID = 711
	cfg.GitHub.APIURL = server.URL
	cfg.GitHub.InstallationID = 42
	cfg.Runner.Image = "ai-workflow-gate-integration-image-does-not-exist"
	cfg.Repositories = []config.Repository{{Name: "acme/pilot", ID: 77, BaseBranch: "main", AllowedPaths: []string{"**"}, Verification: [][]string{{"go", "test", "./..."}}, PolicyVersion: "integration-v1"}}
	e, err := New(s, cfg)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	e.app, err = gh.New(gh.Config{AppID: "711", PrivateKeyPEM: privatePEM, BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("create mock GitHub App client: %v", err)
	}

	t.Run("automatic gate succeeds only for exact remote identity", func(t *testing.T) {
		w := gateIdentityFixture(e, 71)
		if err := s.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
		mock.setPR(gateMockPR(w), nil)
		mock.setArchive(w.BaseSHA, map[string]string{"file.txt": "old"})
		mock.setArchive(w.HeadSHA, map[string]string{"file.txt": "new"})
		a := enqueueAndStartGate(t, ctx, s, w)
		d := gateActionData{Pass: true, Reason: "all checks passed", Key: w.Review.Key, SHA: w.HeadSHA}
		receipt, err, unknown := e.publishGate(ctx, w, a, d)
		if err != nil || unknown {
			t.Fatalf("publish current gate: receipt=%v err=%v unknown=%v", receipt, err, unknown)
		}
		if err := s.FinishAction(ctx, a, receipt, nil, false); err != nil {
			t.Fatalf("finish successful gate action: %v", err)
		}
		stored, err := s.Get(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.GateConclusion != "success" || stored.CheckID == 0 || stored.Generation != w.Generation {
			t.Fatalf("current gate was not recorded as success: conclusion=%q check=%d generation=%d", stored.GateConclusion, stored.CheckID, stored.Generation)
		}
		if got := mock.successfulChecks(); got != 1 {
			t.Fatalf("successful check count=%d, want 1", got)
		}
	})

	for i, tc := range []struct {
		name string
		edit func(*gh.PullRequest)
	}{
		{"base moved", func(p *gh.PullRequest) { p.Base.SHA = "new-base" }},
		{"head changed", func(p *gh.PullRequest) { p.Head.SHA = "new-head" }},
		{"head ref changed", func(p *gh.PullRequest) { p.Head.Ref = "other-branch" }},
		{"closed", func(p *gh.PullRequest) { p.State = "closed" }},
		{"draft", func(p *gh.PullRequest) { p.Draft = true }},
		{"wrong repo id", func(p *gh.PullRequest) { p.Head.Repo.ID++ }},
		{"wrong repo name", func(p *gh.PullRequest) { p.Base.Repo.FullName = "fork/pilot" }},
		{"missing repo identity", func(p *gh.PullRequest) { p.Head.Repo = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := gateIdentityFixture(e, 72+i)
			if err := s.Create(ctx, w); err != nil {
				t.Fatal(err)
			}
			pr := gateMockPR(w)
			tc.edit(&pr)
			mock.setPR(pr, nil)
			a := enqueueAndStartGate(t, ctx, s, w)
			d := gateActionData{Pass: true, Reason: "all checks passed", Key: w.Review.Key, SHA: w.HeadSHA}
			_, err, _ := e.publishGate(ctx, w, a, d)
			if err == nil {
				t.Fatal("gate unexpectedly accepted changed remote PR")
			}
			stored, getErr := s.Get(ctx, w.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if stored.Generation <= w.Generation || stored.Review.Verdict != "OBSOLETE" || (tc.name != "base moved" && stored.Control == "ACTIVE") || (tc.name == "base moved" && (stored.Control != "ACTIVE" || stored.Phase != "VERIFYING")) {
				t.Fatalf("remote drift did not invalidate and stop review: generation=%d verdict=%s control=%s", stored.Generation, stored.Review.Verdict, stored.Control)
			}
			if err := s.FinishAction(ctx, a, nil, nil, false); err != nil {
				t.Fatalf("finish rejected gate action: %v", err)
			}
		})
	}

	t.Run("invalid review key is rejected before check publication", func(t *testing.T) {
		w := gateIdentityFixture(e, 81)
		if err := s.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
		mock.setPR(gateMockPR(w), nil)
		mock.setArchive(w.BaseSHA, map[string]string{"file.txt": "old"})
		mock.setArchive(w.HeadSHA, map[string]string{"file.txt": "new"})
		a := enqueueAndStartGate(t, ctx, s, w)
		d := gateActionData{Pass: true, Reason: "all checks passed", Key: "wrong-key", SHA: w.HeadSHA}
		_, err, _ := e.publishGate(ctx, w, a, d)
		if err == nil {
			t.Fatal("invalid review key unexpectedly passed")
		}
		if got := mock.successfulChecks(); got != 1 {
			t.Fatalf("invalid key created a green check; successful count=%d", got)
		}
		stored, err := s.Get(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Review.Verdict != "OBSOLETE" || stored.Generation == w.Generation {
			t.Fatalf("invalid key did not invalidate review: generation=%d verdict=%s", stored.Generation, stored.Review.Verdict)
		}
		_ = s.FinishAction(ctx, a, nil, nil, false)
	})

	t.Run("remote drift after check creation revokes green check", func(t *testing.T) {
		w := gateIdentityFixture(e, 82)
		if err := s.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
		valid := gateMockPR(w)
		changed := gateMockPR(w)
		changed.Base.SHA = "raced-base"
		mock.setPR(valid, []gh.PullRequest{valid, valid, changed})
		mock.setArchive(w.BaseSHA, map[string]string{"file.txt": "old"})
		mock.setArchive(w.HeadSHA, map[string]string{"file.txt": "new"})
		a := enqueueAndStartGate(t, ctx, s, w)
		d := gateActionData{Pass: true, Reason: "all checks passed", Key: w.Review.Key, SHA: w.HeadSHA}
		_, err, _ := e.publishGate(ctx, w, a, d)
		if err == nil {
			t.Fatal("gate should report the post-publication identity race")
		}
		if got := mock.lastCheckConclusion(); got != "failure" {
			t.Fatalf("green check was not revoked, final conclusion=%q", got)
		}
		stored, getErr := s.Get(ctx, w.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if stored.Generation <= w.Generation || stored.Review.Verdict != "OBSOLETE" {
			t.Fatalf("post-publication drift not persisted: generation=%d verdict=%s", stored.Generation, stored.Review.Verdict)
		}
		_ = s.FinishAction(ctx, a, nil, nil, false)
	})

	t.Run("exact running publish webhook defers and receipt completes", func(t *testing.T) {
		w := publishWebhookFixture(e, 91)
		if err := s.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
		d := publishActionData{Changes: []gh.FileChange{{Path: "file.txt", Content: []byte("new"), Mode: "100644"}}, TaskIDs: []string{"T1"}, Message: "finish T1", ExpectedParent: w.HeadSHA, BaseBranch: w.BaseBranch, BaseSHA: w.BaseSHA, Branch: w.Branch}
		attachPublishIntegrationFixture(t, ctx, s, &w, &d, "tree-published")
		a := store.NewAction(w, "publish", d)
		if err := s.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) { return []store.Action{a}, nil }); err != nil {
			t.Fatal(err)
		}
		a = enqueueAndStartAction(t, ctx, s, a.ID)
		pr := gateMockPR(w)
		pr.State = "open"
		pr.Draft = false
		pr.Head.SHA = "published-head"
		pr.Base.SHA = w.BaseSHA
		mock.setPR(pr, nil)
		mock.setArchive(w.HeadSHA, map[string]string{"file.txt": "old"})
		mock.setArchive("published-head", map[string]string{"file.txt": "new"})
		mock.setCommit("published-head", gh.CommitInfo{SHA: "published-head", Message: d.Message + "\n\nAI Workflow Action: " + a.ID, TreeSHA: "tree-published", Parents: []string{w.HeadSHA}})

		if err := e.pullRequestEvent(ctx, w, gh.WebhookEvent{Action: "synchronize"}); err != nil {
			t.Fatalf("process exact own publish webhook: %v", err)
		}
		unchanged, err := s.Get(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.Generation != w.Generation || unchanged.Control != "ACTIVE" || unchanged.HeadSHA != w.HeadSHA {
			t.Fatalf("own webhook advanced workflow before receipt: gen=%d control=%s head=%s", unchanged.Generation, unchanged.Control, unchanged.HeadSHA)
		}
		_, effectErr, unknown := e.publishGitHub(ctx, unchanged, a, d)
		if effectErr != nil || unknown {
			t.Fatalf("complete exact publish receipt: err=%v unknown=%v", effectErr, unknown)
		}
		if err := s.FinishAction(ctx, a, map[string]string{"head": "published-head"}, nil, false); err != nil {
			t.Fatalf("finish exact publish: %v", err)
		}
		completed, err := s.Get(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.HeadSHA != "published-head" || completed.PR != w.PR || completed.Tasks[0].Status != "INTEGRATED" || completed.Tasks[0].IntegratedSHA != "published-head" {
			t.Fatalf("publish receipt was not fully committed: head=%s pr=%d task=%+v", completed.HeadSHA, completed.PR, completed.Tasks[0])
		}
	})

	t.Run("external head while publish is running pauses workflow", func(t *testing.T) {
		w := publishWebhookFixture(e, 92)
		if err := s.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
		d := publishActionData{Changes: []gh.FileChange{{Path: "file.txt", Content: []byte("new"), Mode: "100644"}}, TaskIDs: []string{"T1"}, Message: "finish T1", ExpectedParent: w.HeadSHA, BaseBranch: w.BaseBranch, BaseSHA: w.BaseSHA, Branch: w.Branch}
		attachPublishIntegrationFixture(t, ctx, s, &w, &d, "tree-published")
		a := store.NewAction(w, "publish", d)
		if err := s.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) { return []store.Action{a}, nil }); err != nil {
			t.Fatal(err)
		}
		a = enqueueAndStartAction(t, ctx, s, a.ID)
		pr := gateMockPR(w)
		pr.Head.SHA = "external-head"
		mock.setPR(pr, nil)
		mock.setCommit("external-head", gh.CommitInfo{SHA: "external-head", Message: "outside contributor", TreeSHA: "tree-external", Parents: []string{w.HeadSHA}})
		mock.setArchive("external-head", map[string]string{"file.txt": "outside"})
		mock.setArchive(w.HeadSHA, map[string]string{"file.txt": "old"})
		if err := e.pullRequestEvent(ctx, w, gh.WebhookEvent{Action: "synchronize"}); err != nil {
			t.Fatalf("process external synchronize: %v", err)
		}
		changed, err := s.Get(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if changed.Generation <= w.Generation || changed.Control != "PAUSING" || changed.HeadSHA != "external-head" || changed.Review.Verdict != "OBSOLETE" {
			t.Fatalf("external head was not fenced despite pending publish: gen=%d control=%s head=%s verdict=%s", changed.Generation, changed.Control, changed.HeadSHA, changed.Review.Verdict)
		}
	})

	t.Run("external head preserves settled cancellation and human ownership", func(t *testing.T) {
		for i, control := range []string{"CANCELLED", "HUMAN_OWNED"} {
			w := publishWebhookFixture(e, 93+i)
			w.Control = control
			w.Review = domain.Review{Key: "prior-review", Verdict: "PASS"}
			if err := s.Create(ctx, w); err != nil {
				t.Fatal(err)
			}
			pr := gateMockPR(w)
			pr.Head.SHA = "external-" + strings.ToLower(control)
			mock.setPR(pr, nil)
			if i == 0 {
				if err := e.reconcileOne(ctx, w); err != nil {
					t.Fatalf("reconcile external head for %s: %v", control, err)
				}
			} else if err := e.pullRequestEvent(ctx, w, gh.WebhookEvent{Action: "synchronize"}); err != nil {
				t.Fatalf("process external head for %s: %v", control, err)
			}
			stored, err := s.Get(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Control != control || stored.Generation <= w.Generation || stored.HeadSHA != pr.Head.SHA || stored.Review.Verdict != "OBSOLETE" {
				t.Fatalf("external head did not preserve %s control while invalidating review: control=%s gen=%d head=%s verdict=%s", control, stored.Control, stored.Generation, stored.HeadSHA, stored.Review.Verdict)
			}
		}
	})
}

func gateIdentitySchemaURL(dbURL, schema string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("database URL must be postgres URL: %w", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func gateIdentityFixture(e *Engine, issue int) domain.Workflow {
	w := domain.Workflow{ID: domain.ID(770077, issue), RepositoryID: 77, Repository: "acme/pilot", InstallationID: 42, Issue: issue, Phase: "READY", Control: "ACTIVE", Generation: 1, IssueTitle: "Test gate", IssueBody: "Verify current PR identity", PR: 700 + issue, PRReady: true, Branch: fmt.Sprintf("codex/issue-%d", issue), BaseBranch: "main", BaseSHA: "base-sha", HeadSHA: fmt.Sprintf("head-%d", issue)}
	w.Spec = domain.Spec{Title: "Requirement", Description: "Verify candidate", Criteria: []domain.Criterion{{ID: "AC-1", Description: "candidate is current"}}, Approved: true, ApprovedBy: "ai:dev-triage"}
	w.Spec.Digest = domain.SpecDigest(w.Spec)
	w.Review = domain.Review{ID: fmt.Sprintf("review-%d", issue), VerificationPassed: true, SkepticComplete: true, Verdict: "PASS", Obligations: []domain.Obligation{{ID: "O-1", CriterionID: "AC-1", Result: "satisfied", Evidence: []string{"evidence-1"}}}}
	w.Review.Tasks = []domain.Task{{ID: "reviewer-1", Status: "ACCEPTED", ObligationIDs: []string{"O-1"}}, {ID: "skeptic", Status: "ACCEPTED"}}
	w.Review.Key = e.key(context.Background(), w)
	return w
}

func enqueueAndStartGate(t *testing.T, ctx context.Context, s *store.Store, w domain.Workflow) store.Action {
	t.Helper()
	a := gateAction(w, true, "integration gate")
	if err := s.Update(ctx, w.ID, func(*domain.Workflow) ([]store.Action, error) { return []store.Action{a}, nil }); err != nil {
		t.Fatalf("enqueue gate action: %v", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE outbox SET state='running',attempts=1,lease_until=now()+interval '90 seconds' WHERE id=$1`, a.ID); err != nil {
		t.Fatalf("mark gate action running: %v", err)
	}
	a.Attempts = 1
	if err := s.StartAction(ctx, a); err != nil {
		t.Fatalf("authorize gate action: %v", err)
	}
	return a
}

func enqueueAndStartAction(t *testing.T, ctx context.Context, s *store.Store, actionID string) store.Action {
	t.Helper()
	var action store.Action
	if err := s.DB.QueryRow(ctx, `UPDATE outbox SET state='running',attempts=1,lease_until=now()+interval '90 seconds' WHERE id=$1 RETURNING id,workflow_id,generation,kind,data,attempts`, actionID).Scan(&action.ID, &action.WorkflowID, &action.Generation, &action.Kind, &action.Data, &action.Attempts); err != nil {
		t.Fatalf("mark action running: %v", err)
	}
	if err := s.StartAction(ctx, action); err != nil {
		t.Fatalf("authorize action: %v", err)
	}
	return action
}

func publishWebhookFixture(e *Engine, issue int) domain.Workflow {
	w := gateIdentityFixture(e, issue)
	w.Phase = "DEVELOPING"
	w.PRReady = true
	w.Tasks = []domain.Task{{ID: "T1", Kind: "code", Description: "change one file", Paths: []string{"file.txt"}, Status: "RUNNING", Attempts: 1, LeaseEpoch: 1}}
	w.HeadSHA = fmt.Sprintf("parent-%d", issue)
	w.Review = domain.Review{}
	return w
}

type gateGitHubMock struct {
	mu           sync.Mutex
	pr           gh.PullRequest
	prSequence   []gh.PullRequest
	prGets       int
	checkPosts   []gh.CheckRunInput
	checkPatches []gh.CheckRunInput
	nextCheckID  int64
	commits      map[string]gh.CommitInfo
	archives     map[string]map[string]string
}

func newGateGitHubMock() *gateGitHubMock {
	return &gateGitHubMock{nextCheckID: 1000, commits: map[string]gh.CommitInfo{}, archives: map[string]map[string]string{}}
}

func (m *gateGitHubMock) setPR(pr gh.PullRequest, sequence []gh.PullRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pr, m.prSequence, m.prGets = pr, append([]gh.PullRequest(nil), sequence...), 0
}

func (m *gateGitHubMock) setCommit(sha string, info gh.CommitInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commits[sha] = info
}

func (m *gateGitHubMock) setArchive(sha string, files map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	copyFiles := make(map[string]string, len(files))
	for path, data := range files {
		copyFiles[path] = data
	}
	m.archives[sha] = copyFiles
}

func (m *gateGitHubMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/app/installations/42/access_tokens" && r.Method == http.MethodPost {
		writeGateJSON(w, map[string]any{"token": "integration-token", "expires_at": time.Now().Add(time.Hour)})
		return
	}
	if path == "/app" {
		writeGateJSON(w, map[string]any{"slug": "workflow"})
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/issues/") && r.Method == http.MethodGet {
		writeGateJSON(w, map[string]any{"number": 1, "title": "Test gate", "body": "Verify current PR identity", "state": "open"})
		return
	}
	if path == "/repos/acme/pilot/pulls" && r.Method == http.MethodGet {
		m.mu.Lock()
		pr := m.pr
		m.mu.Unlock()
		writeGateJSON(w, []gh.PullRequest{pr})
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/pulls/") && !strings.Contains(path, "check-runs") && r.Method == http.MethodGet {
		m.mu.Lock()
		pr := m.pr
		if len(m.prSequence) > 0 {
			idx := m.prGets
			if idx >= len(m.prSequence) {
				idx = len(m.prSequence) - 1
			}
			pr = m.prSequence[idx]
			m.prGets++
		}
		m.mu.Unlock()
		writeGateJSON(w, pr)
		return
	}
	if strings.HasSuffix(path, "/check-runs") && r.Method == http.MethodGet {
		writeGateJSON(w, map[string]any{"check_runs": []any{}})
		return
	}
	if path == "/repos/acme/pilot/check-runs" && r.Method == http.MethodPost {
		var in gh.CheckRunInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.checkPosts = append(m.checkPosts, in)
		m.nextCheckID++
		id := m.nextCheckID
		m.mu.Unlock()
		writeGateJSON(w, gateCheckRun(id, in, 711))
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/check-runs/") && r.Method == http.MethodPatch {
		var in gh.CheckRunInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.checkPatches = append(m.checkPatches, in)
		id := m.nextCheckID
		m.mu.Unlock()
		writeGateJSON(w, gateCheckRun(id, in, 711))
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/pulls/") && r.Method == http.MethodPatch {
		m.mu.Lock()
		pr := m.pr
		m.mu.Unlock()
		writeGateJSON(w, pr)
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/git/ref/heads/") {
		sha := "base-sha"
		if strings.Contains(path, "issue-91") {
			sha = "published-head"
		} else if strings.Contains(path, "issue-92") {
			sha = "external-head"
		}
		writeGateJSON(w, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": sha}})
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/git/commits/") {
		sha := strings.TrimPrefix(path, "/repos/acme/pilot/git/commits/")
		m.mu.Lock()
		info, ok := m.commits[sha]
		m.mu.Unlock()
		if !ok {
			info = gh.CommitInfo{SHA: sha, Message: "test commit", TreeSHA: "tree-" + sha, Parents: []string{"parent"}}
		}
		writeGateJSON(w, map[string]any{"sha": info.SHA, "message": info.Message, "tree": map[string]any{"sha": info.TreeSHA}, "parents": gateParents(info.Parents)})
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/zipball/") {
		sha := strings.TrimPrefix(path, "/repos/acme/pilot/zipball/")
		m.mu.Lock()
		files := m.archives[sha]
		copied := make(map[string]string, len(files))
		for name, value := range files {
			copied[name] = value
		}
		m.mu.Unlock()
		blob, err := gateZip(copied)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(blob)
		return
	}
	http.NotFound(w, r)
}

func (m *gateGitHubMock) successfulChecks() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, check := range m.checkPosts {
		if check.Conclusion == "success" {
			n++
		}
	}
	return n
}

func (m *gateGitHubMock) lastCheckConclusion() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.checkPatches) > 0 {
		return m.checkPatches[len(m.checkPatches)-1].Conclusion
	}
	if len(m.checkPosts) > 0 {
		return m.checkPosts[len(m.checkPosts)-1].Conclusion
	}
	return ""
}

func gateMockPR(w domain.Workflow) gh.PullRequest {
	pr := gh.PullRequest{Number: w.PR, State: "open", Title: "Integration PR", Body: "<!-- ai-workflow:owned-pr-body:start -->\nold\n<!-- ai-workflow:owned-pr-body:end -->", User: gh.User{Login: "workflow[bot]"}}
	pr.Head.Ref, pr.Head.SHA = w.Branch, w.HeadSHA
	pr.Base.Ref, pr.Base.SHA = w.BaseBranch, w.BaseSHA
	pr.Head.Repo = &gh.Repository{ID: w.RepositoryID, FullName: w.Repository}
	pr.Base.Repo = &gh.Repository{ID: w.RepositoryID, FullName: w.Repository}
	return pr
}

func writeGateJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func gateCheckRun(id int64, in gh.CheckRunInput, appID int64) map[string]any {
	return map[string]any{"id": id, "name": in.Name, "status": in.Status, "conclusion": in.Conclusion, "head_sha": in.HeadSHA, "output": in.Output, "app": map[string]any{"id": appID}}
}

func gateParents(parents []string) []map[string]string {
	out := make([]map[string]string, 0, len(parents))
	for _, parent := range parents {
		out = append(out, map[string]string{"sha": parent})
	}
	return out
}

func gateZip(files map[string]string) ([]byte, error) {
	var buffer bytes.Buffer
	zw := zip.NewWriter(&buffer)
	for path, content := range files {
		header := &zip.FileHeader{Name: "acme-pilot/" + path}
		header.SetMode(0644)
		entry, err := zw.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(entry, content); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
