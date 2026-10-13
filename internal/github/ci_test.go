package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ciHTTPClient(t *testing.T, handler http.HandlerFunc) *InstallationClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := &Client{baseURL: server.URL, http: server.Client(), now: time.Now, tokens: map[int64]tokenEntry{5: {value: "synthetic-token", expires: time.Now().Add(time.Hour)}}}
	return client.ForInstallation(context.Background(), 5)
}
func TestCIReadEndpointsAndIdentityFields(t *testing.T) {
	c := ciHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("CI 身份读取不应写入：%s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/pilot/actions/workflows/12":
			_ = json.NewEncoder(w).Encode(ActionsWorkflow{ID: 12, Path: ".github/workflows/ci.yml", State: "active"})
		case "/repos/acme/pilot/actions/runs/34":
			_ = json.NewEncoder(w).Encode(ActionsRun{ID: 34, WorkflowID: 12, RunAttempt: 2, Status: "completed", Conclusion: "success"})
		case "/repos/acme/pilot/actions/runs/34/attempts/2":
			_ = json.NewEncoder(w).Encode(ActionsRun{ID: 34, WorkflowID: 12, RunAttempt: 2, Status: "completed", Conclusion: "success"})
		case "/repos/acme/pilot/contents/.github/workflows/ci.yml":
			if r.URL.Query().Get("ref") != strings.Repeat("a", 40) {
				t.Error("文件来源必须固定 ref")
			}
			_ = json.NewEncoder(w).Encode(RepositoryFileIdentity{Type: "file", Path: ".github/workflows/ci.yml", SHA: strings.Repeat("b", 40)})
		case "/repos/acme/pilot/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"state":"open","mergeable":true,"merge_commit_sha":"merge","head":{"sha":"head","repo":{"id":1}},"base":{"sha":"base","repo":{"id":1}}}`))
		default:
			t.Errorf("非预期端点：%s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	if w, err := c.GetActionsWorkflow(ctx, "acme", "pilot", 12); err != nil || w.ID != 12 {
		t.Fatal(w, err)
	}
	if r, err := c.GetActionsRun(ctx, "acme", "pilot", 34); err != nil || r.RunAttempt != 2 {
		t.Fatal(r, err)
	}
	if r, err := c.GetActionsRunAttempt(ctx, "acme", "pilot", 34, 2); err != nil || r.Conclusion != "success" {
		t.Fatal(r, err)
	}
	if f, err := c.GetRepositoryFileIdentity(ctx, "acme", "pilot", ".github/workflows/ci.yml", strings.Repeat("a", 40)); err != nil || f.SHA == "" {
		t.Fatal(f, err)
	}
	if pr, err := c.GetCIPullRequestSnapshot(ctx, "acme", "pilot", 7); err != nil || pr.Number != 7 || pr.MergeCommitSHA != "merge" || pr.Head.Repo.ID != 1 {
		t.Fatal(pr, err)
	}
}
func TestCIReadRejectsMismatchedRunAttempt(t *testing.T) {
	c := ciHTTPClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":34,"run_attempt":1}`)) })
	if _, err := c.GetActionsRunAttempt(context.Background(), "acme", "pilot", 34, 2); err == nil {
		t.Fatal("旧重试不能冒充指定重试")
	}
}
func TestCIFileIdentityRejectsTraversalAndSymlink(t *testing.T) {
	calls := 0
	c := ciHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"type":"symlink","path":".github/workflows/ci.yml","sha":"blob"}`))
	})
	for _, name := range []string{"../ci.yml", "/ci.yml", "a/../ci.yml", "a\\b"} {
		if _, err := c.GetRepositoryFileIdentity(context.Background(), "acme", "pilot", name, "sha"); err == nil {
			t.Fatal("应拒绝不规范路径")
		}
	}
	if calls != 0 {
		t.Fatal("无效路径不应请求远端")
	}
	if _, err := c.GetRepositoryFileIdentity(context.Background(), "acme", "pilot", ".github/workflows/ci.yml", "sha"); err == nil {
		t.Fatal("符号链接不是可信普通文件")
	}
}
