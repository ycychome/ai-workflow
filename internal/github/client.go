// Package github owns authenticated HTTP access. It never executes repository code.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	c "github.com/ycychome/ai-workflow/internal/contracts"
	"github.com/ycychome/ai-workflow/internal/policies"
)

type Client struct {
	BaseURL               string
	HTTP                  *http.Client
	AppID, InstallationID int64
	Bot                   string
	Key                   *rsa.PrivateKey
	Token                 string // Test-only transport credential; production uses an installation token.
	mu                    sync.Mutex
	expires               time.Time
}

func ParseKey(b []byte) (*rsa.PrivateKey, error) {
	p, _ := pem.Decode(b)
	if p == nil {
		return nil, errors.New("invalid PEM")
	}
	if k, e := x509.ParsePKCS1PrivateKey(p.Bytes); e == nil {
		return k, nil
	}
	k, e := x509.ParsePKCS8PrivateKey(p.Bytes)
	if e != nil {
		return nil, e
	}
	r, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GitHub App needs RSA key")
	}
	return r, nil
}
func (g *Client) token(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Token != "" && (g.Key == nil || time.Now().Before(g.expires.Add(-time.Minute))) {
		return g.Token, nil
	}
	if g.Key == nil {
		return "", errors.New("GitHub App key not configured")
	}
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	b, _ := json.Marshal(map[string]any{"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(8 * time.Minute).Unix(), "iss": strconv.FormatInt(g.AppID, 10)})
	signing := h + "." + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(signing))
	sig, e := rsa.SignPKCS1v15(rand.Reader, g.Key, crypto.SHA256, sum[:])
	if e != nil {
		return "", e
	}
	jwt := signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	var result struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	e = g.request(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", g.InstallationID), map[string]any{}, &result, jwt)
	if e != nil {
		return "", e
	}
	g.Token = result.Token
	g.expires = result.Expires
	return g.Token, nil
}

type APIError struct {
	Status       int
	Method, Path string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %s %s returned %d", e.Method, e.Path, e.Status)
}
func IsStatus(err error, status int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == status
}
func (g *Client) request(ctx context.Context, method, path string, in, out any, token string) error {
	var b []byte
	var e error
	if in != nil {
		b, e = json.Marshal(in)
		if e != nil {
			return e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.BaseURL, "/")+path, bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, e := g.HTTP.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{resp.StatusCode, method, path}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}
func (g *Client) Do(ctx context.Context, method, path string, in, out any) error {
	token, e := g.token(ctx)
	if e != nil {
		return e
	}
	return g.request(ctx, method, path, in, out, token)
}
func (g *Client) Permission(ctx context.Context, repo, user string) (bool, error) {
	var r struct {
		Permission string `json:"permission"`
	}
	e := g.Do(ctx, "GET", "/repos/"+repo+"/collaborators/"+url.PathEscape(user)+"/permission", nil, &r)
	if IsStatus(e, 404) {
		return false, nil
	}
	return r.Permission == "admin" || r.Permission == "maintain", e
}

type Issue struct {
	PR     json.RawMessage `json:"pull_request"`
	Number int             `json:"number"`
	Title  string          `json:"title"`
	Body   string          `json:"body"`
	State  string          `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (g *Client) OpenIssues(ctx context.Context, repo, label string) ([]Issue, error) {
	var all []Issue
	for page := 1; page <= 5; page++ {
		var issues []Issue
		e := g.Do(ctx, "GET", fmt.Sprintf("/repos/%s/issues?state=open&labels=%s&per_page=100&page=%d", repo, url.QueryEscape(label), page), nil, &issues)
		if e != nil {
			return nil, e
		}
		for _, issue := range issues {
			if len(issue.PR) == 0 {
				all = append(all, issue)
			}
		}
		if len(issues) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("M0 issue discovery exceeded 500-item reconciliation bound")
}

func (g *Client) Issue(ctx context.Context, repo string, n int) (Issue, error) {
	var r Issue
	e := g.Do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d", repo, n), nil, &r)
	return r, e
}
func (g *Client) Head(ctx context.Context, repo, branch string) (string, error) {
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	e := g.Do(ctx, "GET", "/repos/"+repo+"/git/ref/heads/"+url.PathEscape(branch), nil, &r)
	return r.Object.SHA, e
}

type PR struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Merged bool   `json:"merged"`
	Body   string `json:"body"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
}

func (g *Client) Pull(ctx context.Context, repo string, n int) (PR, error) {
	var r PR
	e := g.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &r)
	return r, e
}
func (g *Client) Snapshot(ctx context.Context, repo, sha string) (map[string]string, error) {
	return g.SnapshotFiles(ctx, repo, sha, []string{"greeting.txt", "README.md"})
}
func (g *Client) SnapshotFiles(ctx context.Context, repo, sha string, names []string) (map[string]string, error) {
	// M0 deliberately admits only the tiny fixture repository, never arbitrary source trees.
	files := map[string]string{}
	for _, name := range names {
		if !policies.SnapshotPath(name) {
			return nil, errors.New("untrusted snapshot path")
		}
		var r struct {
			Content, Encoding string
			Size              int
		}
		e := g.Do(ctx, "GET", "/repos/"+repo+"/contents/"+name+"?ref="+url.QueryEscape(sha), nil, &r)
		if e != nil {
			return nil, e
		}
		if r.Encoding != "base64" || r.Size > 65536 {
			return nil, errors.New("M0 fixture file is too large or unsupported")
		}
		b, e := base64.StdEncoding.DecodeString(strings.ReplaceAll(r.Content, "\n", ""))
		if e != nil {
			return nil, e
		}
		files[name] = string(b)
	}
	return files, nil
}

type comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func marker(id string) string { return "<!-- ai-workflow:" + id + " -->" }
func (g *Client) Comment(ctx context.Context, repo string, n int, id, body string, guard func() error) (int64, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, n)
	for page := 1; page <= 10; page++ {
		var comments []comment
		if e := g.Do(ctx, "GET", fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil, &comments); e != nil {
			return 0, e
		}
		for _, r := range comments {
			if r.User.Login == g.Bot && strings.Contains(r.Body, marker(id)) {
				return r.ID, nil
			}
		}
		if len(comments) < 100 {
			if e := guard(); e != nil {
				return 0, e
			}
			var r comment
			e := g.Do(ctx, "POST", path, map[string]any{"body": body + "\n" + marker(id)}, &r)
			return r.ID, e
		}
	}
	return 0, errors.New("comment reconciliation exceeded bound")
}

// Publish creates a deterministic bot branch using the Git Data API. No force push,
// shell, hooks or candidate-supplied remote configuration enters this broker.
func (g *Client) Publish(ctx context.Context, w c.Workflow, a c.Action, candidate c.Candidate, baseBranch string, guard func() error, recordHead func(string) error) (PR, error) {
	var zero PR
	var e error
	branch := fmt.Sprintf("ai/m0/issue-%d-g%d-e%d", w.Issue, a.Generation, a.Epoch)
	// Reconcile PR first. Search markers only in bot-owned PRs on the exact branch.
	for page := 1; page <= 10; page++ {
		var prs []PR
		path := fmt.Sprintf("/repos/%s/pulls?state=all&head=%s&per_page=100&page=%d", w.Repository, url.QueryEscape(strings.Split(w.Repository, "/")[0]+":"+branch), page)
		if e = g.Do(ctx, "GET", path, nil, &prs); e != nil {
			return zero, e
		}
		for _, pr := range prs {
			if pr.User.Login == g.Bot && pr.Head.Ref == branch && strings.Contains(pr.Body, marker(a.ID)) {
				if candidate.ExpectedHead == "" || pr.Head.SHA != candidate.ExpectedHead {
					return zero, c.ErrStale
				}
				return pr, nil
			}
		}
		if len(prs) < 100 {
			break
		}
		if page == 10 {
			return zero, errors.New("PR reconciliation exceeded bound")
		}
	}
	current, e := g.Head(ctx, w.Repository, baseBranch)
	if e != nil {
		return zero, e
	}
	if current != candidate.BaseSHA {
		return zero, c.ErrStale
	}
	if candidate.ArtifactDigest != c.Digest(candidate.Files) {
		return zero, errors.New("candidate digest mismatch")
	}
	var parent struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if e = g.Do(ctx, "GET", "/repos/"+w.Repository+"/git/commits/"+candidate.BaseSHA, nil, &parent); e != nil {
		return zero, e
	}
	entries := []map[string]any{}
	for name, content := range candidate.Files {
		if name != "greeting.txt" {
			return zero, errors.New("candidate exceeds M0 scope")
		}
		entries = append(entries, map[string]any{"path": name, "mode": "100644", "type": "blob", "content": content})
	}
	if len(entries) == 0 {
		return zero, errors.New("empty candidate")
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if e = guard(); e != nil {
		return zero, e
	}
	if e = g.Do(ctx, "POST", "/repos/"+w.Repository+"/git/trees", map[string]any{"base_tree": parent.Tree.SHA, "tree": entries}, &tree); e != nil {
		return zero, e
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	// Fixed author time makes retry commits content-addressed, not duplicate history.
	identity := map[string]any{"name": "AI Workflow M0", "email": "m0@users.noreply.github.com", "date": "2026-10-12T00:00:00Z"}
	if e = guard(); e != nil {
		return zero, e
	}
	if e = g.Do(ctx, "POST", "/repos/"+w.Repository+"/git/commits", map[string]any{"message": "M0 fixed patch\n\n" + marker(a.ID), "tree": tree.SHA, "parents": []string{candidate.BaseSHA}, "author": identity, "committer": identity}, &commit); e != nil {
		return zero, e
	}
	if candidate.ExpectedHead != "" && candidate.ExpectedHead != commit.SHA {
		return zero, c.ErrStale
	}
	if e = recordHead(commit.SHA); e != nil {
		return zero, e
	}
	remote, e := g.Head(ctx, w.Repository, branch)
	if IsStatus(e, 404) {
		if e = guard(); e != nil {
			return zero, e
		}
		e = g.Do(ctx, "POST", "/repos/"+w.Repository+"/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": commit.SHA}, nil)
		if e != nil && !IsStatus(e, 422) {
			return zero, e
		}
		remote, e = g.Head(ctx, w.Repository, branch)
	}
	if e != nil {
		return zero, e
	}
	if remote != commit.SHA {
		return zero, c.ErrStale
	}
	if e = guard(); e != nil {
		return zero, e
	}
	var pr PR
	e = g.Do(ctx, "POST", "/repos/"+w.Repository+"/pulls", map[string]any{"title": fmt.Sprintf("M0 Spike: issue #%d", w.Issue), "head": branch, "base": baseBranch, "draft": true, "body": "Fixed-patch M0; human merge only.\n" + marker(a.ID)}, &pr)
	return pr, e
}

type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	ExternalID string `json:"external_id"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

func (g *Client) Check(ctx context.Context, w c.Workflow, v c.Verdict, key c.VerificationKey, reason string, guard func() error) (int64, error) {
	if w.HeadSHA == "" {
		return 0, nil
	}
	prefix := w.ID + ":gate"
	var chosen int64
	for page := 1; page <= 10; page++ {
		var list struct {
			Checks []checkRun `json:"check_runs"`
		}
		if e := g.Do(ctx, "GET", fmt.Sprintf("/repos/%s/commits/%s/check-runs?check_name=workflow%%2Fmerge-gate&filter=all&per_page=100&page=%d", w.Repository, w.HeadSHA, page), nil, &list); e != nil {
			return 0, e
		}
		for _, r := range list.Checks {
			if r.Name == "workflow/merge-gate" && r.App.ID == g.AppID && r.ExternalID == prefix && r.ID > chosen {
				chosen = r.ID
			}
		}
		if len(list.Checks) < 100 {
			break
		}
		if page == 10 {
			return 0, errors.New("check reconciliation exceeded bound")
		}
	}
	check := c.CheckFor(v)
	payload := map[string]any{"name": "workflow/merge-gate", "head_sha": w.HeadSHA, "external_id": prefix, "status": check.Status, "output": map[string]any{"title": string(v), "summary": reason + "\nVerificationKey: " + c.Digest(key)}}
	if check.Conclusion != "" {
		payload["conclusion"] = check.Conclusion
	}
	method, path := "POST", "/repos/"+w.Repository+"/check-runs"
	if chosen != 0 {
		method = "PATCH"
		path += fmt.Sprintf("/%d", chosen)
		delete(payload, "head_sha")
	}
	if e := guard(); e != nil {
		return 0, e
	}
	var r checkRun
	e := g.Do(ctx, method, path, payload, &r)
	return r.ID, e
}
