package orchestration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
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

// TestPrePRBaseMoveReplansOnNewBranchPostgresIntegration covers a target base
// moving after a known failed publish but before a PR exists. The mock GitHub
// server receives the subsequent publish; no external service or model is used.
func TestPrePRBaseMoveReplansOnNewBranchPostgresIntegration(t *testing.T) {
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
	schema := fmt.Sprintf("pre_pr_base_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated schema: %v", err)
		}
	}()
	dbURL, err := prePRSchemaURL(cfg.DatabaseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open isolated store: %v", err)
	}
	defer s.Close()

	mock := newPrePRBaseMock()
	server := httptest.NewServer(mock)
	defer server.Close()
	cfg.GitHub.AppID = 712
	cfg.GitHub.APIURL = server.URL
	cfg.GitHub.InstallationID = 42
	cfg.Runner.Mode = "fake"
	cfg.Runner.Image = "ai-workflow-pre-pr-integration-image-does-not-exist"
	cfg.Repositories = []config.Repository{{Name: "acme/pilot", ID: 77, BaseBranch: "main", AllowedPaths: []string{"**"}, Verification: [][]string{{"go", "test", "./..."}}, PolicyVersion: "integration-v1"}}
	e, err := New(s, cfg)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	e.app, err = gh.New(gh.Config{AppID: "712", PrivateKeyPEM: privatePEM, BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("create mock GitHub client: %v", err)
	}

	w := domain.Workflow{ID: domain.ID(770078, 501), RepositoryID: 77, Repository: "acme/pilot", InstallationID: 42, Issue: 501, Phase: "DEVELOPING", Control: "ACTIVE", Generation: 1, IssueTitle: "Repair Add", IssueBody: "Implement addition", BaseBranch: "main", BaseSHA: "old-base", HeadSHA: "old-base", Branch: "codex/issue-501", Spec: domain.Spec{Title: "Repair Add", Description: "Implement the accepted request", Criteria: []domain.Criterion{{ID: "AC-1", Description: "Add returns the sum"}}, Approved: true, ApprovedBy: "maintainer"}, Tasks: []domain.Task{{ID: "old-task", Kind: "code", Description: "old-base implementation", Paths: []string{"calculator.go"}, Status: "RETRY_WAIT", Attempts: 1, LeaseEpoch: 1}}, Evidence: []domain.Evidence{{ID: "old-evidence", Kind: "TOOL_VERIFIED", BaseSHA: "old-base", HeadSHA: "old-base"}}, Review: domain.Review{ID: "old-review", Key: "old-key", Verdict: "PASS", VerificationPassed: true, SkepticComplete: true}}
	w.Spec.SourceHash = normalizedIssueSourceHash(w.IssueTitle, w.IssueBody)
	w.Spec.Digest = domain.SpecDigest(w.Spec)
	if err := s.Create(ctx, w); err != nil {
		t.Fatal(err)
	}

	oldData := publishActionData{Changes: []gh.FileChange{{Path: "calculator.go", Content: []byte("package pilot\nfunc Add(a,b int) int { return a+b }\n"), Mode: "100644"}}, TaskIDs: []string{"old-task"}, Message: "old-base publish", ExpectedParent: w.HeadSHA, BaseBranch: w.BaseBranch, BaseSHA: w.BaseSHA, Branch: w.Branch}
	attachPublishIntegrationFixture(t, ctx, s, &w, &oldData, "old-candidate-tree")
	oldAction := store.NewAction(w, "publish", oldData)
	if err := s.Update(ctx, w.ID, func(*domain.Workflow) ([]store.Action, error) { return []store.Action{oldAction}, nil }); err != nil {
		t.Fatal(err)
	}
	oldAction = setPrePRActionRunning(t, ctx, s, oldAction.ID)
	if _, err, unknown := e.publishGitHub(ctx, w, oldAction, oldData); err == nil || unknown || !strings.Contains(err.Error(), "基线分支已移动") {
		t.Fatalf("stale publish should fail as a known base move: err=%v unknown=%v", err, unknown)
	}
	oldBranchSHA := mock.branchSHA(w.Branch)
	if _, err := s.DB.Exec(ctx, `UPDATE outbox SET state='unknown',lease_until=NULL,error='simulated crash before receipt' WHERE id=$1`, oldAction.ID); err != nil {
		t.Fatalf("mark publish outcome unknown: %v", err)
	}
	if err := e.reconcileOne(ctx, mustGetPrePRWorkflow(t, ctx, s, w.ID)); err != nil {
		t.Fatalf("reconcile while publish outcome is unknown: %v", err)
	}
	unresolved, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unresolved.Generation != w.Generation || unresolved.BaseSHA != w.BaseSHA || unresolved.Branch != w.Branch {
		t.Fatalf("reconcile discarded unresolved publish identity: generation=%d base=%s branch=%s", unresolved.Generation, unresolved.BaseSHA, unresolved.Branch)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE outbox SET state='retry_wait',next_attempt=now(),error='confirmed stale base failure' WHERE id=$1`, oldAction.ID); err != nil {
		t.Fatalf("settle known failed publish: %v", err)
	}
	if err := s.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		x.Control = "PAUSED"
		return nil, nil
	}); err != nil {
		t.Fatalf("pause before explicit resume: %v", err)
	}

	if err := e.reconcileOne(ctx, mustGetPrePRWorkflow(t, ctx, s, w.ID)); err != nil {
		t.Fatalf("reconcile moved base: %v", err)
	}
	refreshed, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.BaseSHA != "new-base" || refreshed.HeadSHA != "new-base" || refreshed.Branch == w.Branch || refreshed.Tasks != nil || len(refreshed.Evidence) != 0 || refreshed.Review.Verdict != "OBSOLETE" || refreshed.Control != "PAUSED" || refreshed.Phase != "PLANNING" {
		t.Fatalf("pre-PR base move did not invalidate old candidate while preserving pause: base=%s head=%s branch=%s tasks=%d evidence=%d review=%s control=%s phase=%s", refreshed.BaseSHA, refreshed.HeadSHA, refreshed.Branch, len(refreshed.Tasks), len(refreshed.Evidence), refreshed.Review.Verdict, refreshed.Control, refreshed.Phase)
	}
	if !strings.Contains(refreshed.Branch, "-g") {
		t.Fatalf("new generation must use an isolated branch, got %q", refreshed.Branch)
	}
	var oldState string
	if err := s.DB.QueryRow(ctx, "SELECT state FROM outbox WHERE id=$1", oldAction.ID).Scan(&oldState); err != nil {
		t.Fatal(err)
	}
	if oldState != "obsolete" {
		t.Fatalf("stale publish action state=%s, want obsolete", oldState)
	}

	if err := e.Command(ctx, w.ID, "maintainer", "/ai resume", true); err != nil {
		t.Fatalf("resume against new base: %v", err)
	}
	resumed, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Control != "ACTIVE" || resumed.Phase != "PLANNING" || resumed.BaseSHA != "new-base" || resumed.HeadSHA != "new-base" {
		t.Fatalf("resume did not preserve refreshed candidate: control=%s phase=%s base=%s head=%s", resumed.Control, resumed.Phase, resumed.BaseSHA, resumed.HeadSHA)
	}
	if _, err := s.DB.Exec(ctx, "UPDATE outbox SET state='done',lease_until=NULL WHERE workflow_id=$1 AND kind IN ('gate','comment')", w.ID); err != nil {
		t.Fatalf("clear unrelated status effects for scheduler fixture: %v", err)
	}
	claimed, err := s.Claim(ctx, e.owner)
	if err != nil || claimed.ID != w.ID {
		t.Fatalf("claim resumed workflow for model-free plan: id=%s err=%v", claimed.ID, err)
	}
	// This publication test supplies an accepted planner delta. Sandbox agent
	// execution is covered separately; the old fixture deliberately has no runner image.
	if err := applyDevelopmentPlan(&claimed, developmentPlanReport{ExpectedRevision: claimed.PlanRevision, Reason: "依据新基线重新规划", Tasks: []domain.Task{{ID: "new-base-repair", Kind: "code", Description: "修复新基线的加法", Paths: []string{"calculator.go"}}}}, e.Config().EffectiveMaxTasksPerWorkflow()); err != nil {
		t.Fatalf("apply new-base planner receipt: %v", err)
	}
	claimed.Phase = "DEVELOPING"
	if err := s.Save(ctx, &claimed); err != nil {
		t.Fatalf("persist new-base planner receipt: %v", err)
	}
	s.Release(ctx, claimed)
	planned, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if planned.BaseSHA != "new-base" || planned.Tasks == nil || len(planned.Tasks) == 0 || planned.Tasks[0].Status != "QUEUED" {
		t.Fatalf("tasks were not rebuilt from new base: base=%s tasks=%+v", planned.BaseSHA, planned.Tasks)
	}

	newData := publishActionData{Changes: []gh.FileChange{{Path: "calculator.go", Content: []byte("package pilot\nfunc Add(a,b int) int { return a+b }\n"), Mode: "100644"}}, TaskIDs: []string{planned.Tasks[0].ID}, Message: "new-base publish", ExpectedParent: planned.HeadSHA, BaseBranch: planned.BaseBranch, BaseSHA: planned.BaseSHA, Branch: planned.Branch}
	attachPublishIntegrationFixture(t, ctx, s, &planned, &newData, "new-tree")
	newAction := store.NewAction(planned, "publish", newData)
	if err := s.Update(ctx, w.ID, func(*domain.Workflow) ([]store.Action, error) { return []store.Action{newAction}, nil }); err != nil {
		t.Fatal(err)
	}
	newAction = setPrePRActionRunning(t, ctx, s, newAction.ID)
	if _, err, unknown := e.publishGitHub(ctx, planned, newAction, newData); err != nil || unknown {
		t.Fatalf("publish after rebase: err=%v unknown=%v", err, unknown)
	}
	if err := s.FinishAction(ctx, newAction, map[string]string{"head": "new-published-commit"}, nil, false); err != nil {
		t.Fatalf("finish new publish receipt: %v", err)
	}
	final, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.PR <= 0 || final.HeadSHA != "new-published-commit" || final.BaseSHA != "new-base" || final.Branch != newData.Branch || final.Tasks[0].Status != "INTEGRATED" || final.Tasks[0].IntegratedSHA != final.HeadSHA {
		t.Fatalf("new-base publish did not create and record PR: pr=%d base=%s head=%s branch=%s task=%+v", final.PR, final.BaseSHA, final.HeadSHA, final.Branch, final.Tasks[0])
	}
	if got := mock.branchSHA(w.Branch); got != oldBranchSHA {
		t.Fatalf("old branch was modified during rebase: before=%s after=%s", oldBranchSHA, got)
	}
}

func prePRSchemaURL(dbURL, schema string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("database URL must be a postgres URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func setPrePRActionRunning(t *testing.T, ctx context.Context, s *store.Store, id string) store.Action {
	t.Helper()
	var action store.Action
	if err := s.DB.QueryRow(ctx, `UPDATE outbox SET state='running',attempts=1,lease_until=now()+interval '90 seconds' WHERE id=$1 RETURNING id,workflow_id,generation,kind,data,attempts`, id).Scan(&action.ID, &action.WorkflowID, &action.Generation, &action.Kind, &action.Data, &action.Attempts); err != nil {
		t.Fatalf("mark action running: %v", err)
	}
	if err := s.StartAction(ctx, action); err != nil {
		t.Fatalf("authorize action: %v", err)
	}
	return action
}

func mustGetPrePRWorkflow(t *testing.T, ctx context.Context, s *store.Store, id string) domain.Workflow {
	t.Helper()
	w, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

type prePRBaseMock struct {
	mu       sync.Mutex
	baseSHA  string
	refs     map[string]string
	prs      []gh.PullRequest
	commits  map[string]gh.CommitInfo
	newPRNum int
}

func newPrePRBaseMock() *prePRBaseMock {
	return &prePRBaseMock{baseSHA: "new-base", refs: map[string]string{"codex/issue-501": "old-branch-head"}, commits: map[string]gh.CommitInfo{"old-base": {SHA: "old-base", Message: "old base", TreeSHA: "old-tree"}, "new-base": {SHA: "new-base", Message: "new base", TreeSHA: "new-base-tree"}}, newPRNum: 801}
}

func (m *prePRBaseMock) branchSHA(branch string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refs[branch]
}

func (m *prePRBaseMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/app/installations/42/access_tokens" && r.Method == http.MethodPost {
		writePrePRJSON(w, map[string]any{"token": "integration-token", "expires_at": time.Now().Add(time.Hour)})
		return
	}
	if path == "/app" {
		writePrePRJSON(w, map[string]any{"slug": "workflow"})
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/issues/") && r.Method == http.MethodGet {
		writePrePRJSON(w, map[string]any{"number": 501, "title": "Repair Add", "body": "Implement addition", "state": "open"})
		return
	}
	if path == "/repos/acme/pilot/pulls" && r.Method == http.MethodGet {
		m.mu.Lock()
		prs := append([]gh.PullRequest(nil), m.prs...)
		m.mu.Unlock()
		writePrePRJSON(w, prs)
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/git/ref/heads/") && r.Method == http.MethodGet {
		branch := strings.TrimPrefix(path, "/repos/acme/pilot/git/ref/heads/")
		m.mu.Lock()
		sha := m.baseSHA
		if branch != "main" {
			sha = m.refs[branch]
		}
		m.mu.Unlock()
		if sha == "" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writePrePRJSON(w, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha}})
		return
	}
	if path == "/repos/acme/pilot/git/refs" && r.Method == http.MethodPost {
		var in struct{ Ref, SHA string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		branch := strings.TrimPrefix(in.Ref, "refs/heads/")
		m.mu.Lock()
		m.refs[branch] = in.SHA
		m.mu.Unlock()
		writePrePRJSON(w, map[string]any{"ref": in.Ref, "object": map[string]any{"sha": in.SHA}})
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/git/commits/") && r.Method == http.MethodGet {
		sha := strings.TrimPrefix(path, "/repos/acme/pilot/git/commits/")
		m.mu.Lock()
		info := m.commits[sha]
		m.mu.Unlock()
		if info.SHA == "" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writePrePRJSON(w, map[string]any{"sha": info.SHA, "message": info.Message, "tree": map[string]any{"sha": info.TreeSHA}, "parents": prePRParents(info.Parents)})
		return
	}
	if path == "/repos/acme/pilot/git/blobs" && r.Method == http.MethodPost {
		writePrePRJSON(w, map[string]any{"sha": "new-blob"})
		return
	}
	if path == "/repos/acme/pilot/git/trees" && r.Method == http.MethodPost {
		writePrePRJSON(w, map[string]any{"sha": "new-tree"})
		return
	}
	if path == "/repos/acme/pilot/git/commits" && r.Method == http.MethodPost {
		var in struct {
			Message string   `json:"message"`
			Tree    string   `json:"tree"`
			Parents []string `json:"parents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		info := gh.CommitInfo{SHA: "new-published-commit", Message: in.Message, TreeSHA: in.Tree, Parents: in.Parents}
		m.mu.Lock()
		m.commits[info.SHA] = info
		m.mu.Unlock()
		writePrePRJSON(w, map[string]any{"sha": info.SHA, "tree": map[string]any{"sha": info.TreeSHA}})
		return
	}
	if strings.HasPrefix(path, "/repos/acme/pilot/git/refs/heads/") && r.Method == http.MethodPatch {
		branch := strings.TrimPrefix(path, "/repos/acme/pilot/git/refs/heads/")
		var in struct {
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		m.mu.Lock()
		m.refs[branch] = in.SHA
		m.mu.Unlock()
		writePrePRJSON(w, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": in.SHA}})
		return
	}
	if path == "/repos/acme/pilot/pulls" && r.Method == http.MethodPost {
		var in gh.PullRequestInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		pr := gh.PullRequest{Number: m.newPRNum, Title: in.Title, State: "open", Draft: in.Draft, Body: in.Body, User: gh.User{Login: "workflow[bot]"}}
		pr.Head.Ref, pr.Head.SHA = in.Head, "new-published-commit"
		pr.Base.Ref, pr.Base.SHA = in.Base, "new-base"
		pr.Head.Repo = &gh.Repository{ID: 77, FullName: "acme/pilot"}
		pr.Base.Repo = &gh.Repository{ID: 77, FullName: "acme/pilot"}
		m.mu.Lock()
		m.prs = append(m.prs, pr)
		m.mu.Unlock()
		writePrePRJSON(w, pr)
		return
	}
	http.NotFound(w, r)
}

func writePrePRJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func prePRParents(values []string) []map[string]string {
	out := make([]map[string]string, 0, len(values))
	for _, value := range values {
		out = append(out, map[string]string{"sha": value})
	}
	return out
}
