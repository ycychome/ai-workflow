// The emulator injects response loss AFTER committing remote state. It is not
// evidence that GitHub branch-protection behavior has been tested live.
package testutil

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/ycychome/ai-workflow/internal/github"
)

type GitHub struct {
	mu                        sync.Mutex
	Server                    *httptest.Server
	Base, Head, IssueState    string
	PR                        gh.PR
	Refs                      map[string]string
	SourceFiles               map[string]string
	Comments                  []map[string]any
	Checks                    []map[string]any
	PRCreates, CommentCreates int
	LosePR, LoseComment       bool
	LoseCheck                 bool
	Conclusions               []string
	Allowed                   bool
}

func NewGitHub(t *testing.T) *GitHub {
	t.Helper()
	g := &GitHub{Base: "base-A", Head: "head-A", IssueState: "open", Refs: map[string]string{}, Allowed: true}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Server.Close)
	return g
}
func (g *GitHub) Client() *gh.Client {
	return &gh.Client{BaseURL: g.Server.URL, HTTP: g.Server.Client(), Token: "test-credential", AppID: 21, InstallationID: 22, Bot: "m0[bot]"}
}
func (g *GitHub) MoveHead(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Head = sha
	g.PR.Head.SHA = sha
	g.Refs[g.PR.Head.Ref] = sha
}
func (g *GitHub) SetIssueState(state string) { g.mu.Lock(); defer g.mu.Unlock(); g.IssueState = state }
func (g *GitHub) SetDraft(draft bool)        { g.mu.Lock(); defer g.mu.Unlock(); g.PR.Draft = draft }
func (g *GitHub) MoveBase(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Base = sha
	g.PR.Base.SHA = sha
}
func (g *GitHub) SetPRState(state string, merged bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.PR.State = state
	g.PR.Merged = merged
}
func (g *GitHub) Counts() (int, int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.PRCreates, g.CommentCreates, len(g.Checks)
}
func (g *GitHub) ConclusionsCheck(t *testing.T, want string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.Conclusions) == 0 || g.Conclusions[len(g.Conclusions)-1] != want {
		t.Fatalf("gate projection: %v, want final %s", g.Conclusions, want)
	}
	for _, conclusion := range g.Conclusions {
		if conclusion != "success" && conclusion != "failure" {
			t.Fatalf("unsafe conclusion: %s", conclusion)
		}
	}
}
func (g *GitHub) lost(w http.ResponseWriter) {
	h := w.(http.Hijacker)
	conn, _, e := h.Hijack()
	if e == nil {
		conn.Close()
	}
}
func (g *GitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	respond := func(v any) { json.NewEncoder(w).Encode(v) }
	path := r.URL.Path
	var body map[string]any
	if r.Method != "GET" {
		json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case strings.HasSuffix(path, "/permission"):
		permission := "read"
		if g.Allowed {
			permission = "maintain"
		}
		respond(map[string]any{"permission": permission})
	case path == "/repos/o/r/issues/1":
		respond(map[string]any{"number": 1, "title": "M0 spike", "body": "do not expand scope", "state": g.IssueState, "labels": []map[string]any{{"name": "ai:m0"}}})
	case path == "/repos/o/r/issues":
		respond([]map[string]any{{"number": 1, "state": g.IssueState, "labels": []map[string]any{{"name": "ai:m0"}}}})
	case strings.Contains(path, "/git/ref/heads/"):
		branch := strings.SplitN(path, "/git/ref/heads/", 2)[1]
		sha, ok := g.Refs[branch]
		if branch == "main" {
			sha = g.Base
			ok = true
		}
		if !ok {
			w.WriteHeader(404)
			respond(map[string]any{})
			return
		}
		respond(map[string]any{"object": map[string]any{"sha": sha}})
	case strings.Contains(path, "/contents/"):
		name := strings.SplitN(path, "/contents/", 2)[1]
		content := "fixture readme\n"
		if value, ok := g.SourceFiles[name]; ok {
			content = value
		}
		if name == "greeting.txt" {
			content = "hello\n"
			if r.URL.Query().Get("ref") != g.Base {
				content = "hello M0\n"
			}
		}
		respond(map[string]any{"encoding": "base64", "size": len(content), "content": base64.StdEncoding.EncodeToString([]byte(content))})
	case strings.Contains(path, "/git/commits/"):
		respond(map[string]any{"tree": map[string]any{"sha": "tree-base"}})
	case strings.HasSuffix(path, "/git/trees"):
		respond(map[string]any{"sha": "tree-candidate"})
	case strings.HasSuffix(path, "/git/commits"):
		respond(map[string]any{"sha": g.Head})
	case strings.HasSuffix(path, "/git/refs"):
		ref := strings.TrimPrefix(body["ref"].(string), "refs/heads/")
		if _, exists := g.Refs[ref]; exists {
			w.WriteHeader(422)
			respond(map[string]any{})
			return
		}
		g.Refs[ref] = body["sha"].(string)
		respond(map[string]any{"ref": ref})
	case strings.HasSuffix(path, "/pulls") && r.Method == "GET":
		if g.PR.Number == 0 {
			respond([]gh.PR{})
		} else {
			respond([]gh.PR{g.PR})
		}
	case strings.HasSuffix(path, "/pulls") && r.Method == "POST":
		g.PRCreates++
		g.PR = gh.PR{Number: 2, State: "open", Draft: true, Body: body["body"].(string)}
		g.PR.User.Login = "m0[bot]"
		g.PR.Head.Ref = body["head"].(string)
		g.PR.Head.SHA = g.Head
		g.PR.Base.SHA = g.Base
		if g.LosePR {
			g.LosePR = false
			g.lost(w)
			return
		}
		respond(g.PR)
	case strings.Contains(path, "/pulls/"):
		respond(g.PR)
	case strings.HasSuffix(path, "/comments") && r.Method == "GET":
		respond(g.Comments)
	case strings.HasSuffix(path, "/comments"):
		g.CommentCreates++
		comment := map[string]any{"id": g.CommentCreates, "body": body["body"], "user": map[string]any{"login": "m0[bot]"}}
		g.Comments = append(g.Comments, comment)
		if g.LoseComment {
			g.LoseComment = false
			g.lost(w)
			return
		}
		respond(comment)
	case strings.Contains(path, "/commits/") && strings.HasSuffix(path, "/check-runs"):
		respond(map[string]any{"check_runs": g.Checks})
	case strings.Contains(path, "/check-runs"):
		if conclusion, ok := body["conclusion"].(string); ok {
			g.Conclusions = append(g.Conclusions, conclusion)
		}
		if r.Method == "POST" {
			id := len(g.Checks) + 1
			body["id"] = id
			body["app"] = map[string]any{"id": 21}
			g.Checks = append(g.Checks, body)
			if g.LoseCheck {
				g.LoseCheck = false
				g.lost(w)
				return
			}
			respond(body)
		} else {
			id := 1
			fmt.Sscanf(path, "/repos/o/r/check-runs/%d", &id)
			old := g.Checks[id-1]
			for k, v := range body {
				old[k] = v
			}
			respond(old)
		}
	default:
		w.WriteHeader(404)
		respond(map[string]any{"unknown": path})
	}
}
