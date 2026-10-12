package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Repository struct {
	ID            int64           `json:"id"`
	Name          string          `json:"name"`
	FullName      string          `json:"full_name"`
	Private       bool            `json:"private"`
	DefaultBranch string          `json:"default_branch"`
	HTMLURL       string          `json:"html_url"`
	Permissions   map[string]bool `json:"permissions"`
}

func (i *InstallationClient) Repository(ctx context.Context, owner, repo string) (Repository, error) {
	var r Repository
	e := i.request(ctx, http.MethodGet, "/repos/"+q(owner)+"/"+q(repo), nil, &r)
	return r, e
}

type RepositoryPermissions struct {
	Permissions map[string]bool `json:"permissions"`
	RoleName    string          `json:"role_name"`
	User        Account         `json:"user"`
}

func (i *InstallationClient) Permissions(ctx context.Context, owner, repo string) (RepositoryPermissions, error) {
	u, e := i.GetAuthenticatedUser(ctx)
	if e != nil {
		return RepositoryPermissions{}, e
	}
	var p RepositoryPermissions
	e = i.request(ctx, http.MethodGet, "/repos/"+q(owner)+"/"+q(repo)+"/collaborators/"+q(u.Login)+"/permission", nil, &p)
	return p, e
}

// GetCollaboratorPermission returns GitHub's effective permission string for a repository user.
func (i *InstallationClient) GetCollaboratorPermission(ctx context.Context, owner, repo, username string) (string, error) {
	if strings.TrimSpace(username) == "" {
		return "", errors.New("github: collaborator username is required")
	}
	var result struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	err := i.request(ctx, http.MethodGet, "/repos/"+q(owner)+"/"+q(repo)+"/collaborators/"+q(username)+"/permission", nil, &result)
	if result.RoleName != "" {
		return result.RoleName, err
	}
	return result.Permission, err
}

type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}
type Issue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	User        User   `json:"user"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
}

func (i *InstallationClient) GetIssue(ctx context.Context, owner, repo string, number int) (Issue, error) {
	var x Issue
	e := i.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/%d", q(owner), q(repo), number), nil, &x)
	return x, e
}

type PullRequest struct {
	Number   int        `json:"number"`
	NodeID   string     `json:"node_id"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	State    string     `json:"state"`
	Draft    bool       `json:"draft"`
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref  string      `json:"ref"`
		SHA  string      `json:"sha"`
		Repo *Repository `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string      `json:"ref"`
		SHA  string      `json:"sha"`
		Repo *Repository `json:"repo"`
	} `json:"base"`
	HTMLURL string `json:"html_url"`
	User    User   `json:"user"`
}

func (i *InstallationClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	var x PullRequest
	e := i.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", q(owner), q(repo), number), nil, &x)
	return x, e
}

type IssueComment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	User    User   `json:"user"`
	HTMLURL string `json:"html_url"`
}

func (i *InstallationClient) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]IssueComment, error) {
	var out []IssueComment
	for page := 1; page <= 100; page++ {
		var xs []IssueComment
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100&page=%d", q(owner), q(repo), number, page)
		e := i.request(ctx, http.MethodGet, path, nil, &xs)
		if e != nil {
			return nil, e
		}
		out = append(out, xs...)
		if len(xs) < 100 {
			return out, nil
		}
	}
	return out, nil
}

type CheckRun struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	Status     string      `json:"status"`
	Conclusion string      `json:"conclusion"`
	DetailsURL string      `json:"details_url"`
	Output     CheckOutput `json:"output"`
	HeadSHA    string      `json:"head_sha"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}
type CheckOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text,omitempty"`
}
type CheckRunInput struct {
	Name       string      `json:"name"`
	HeadSHA    string      `json:"head_sha"`
	Status     string      `json:"status,omitempty"`
	Conclusion string      `json:"conclusion,omitempty"`
	DetailsURL string      `json:"details_url,omitempty"`
	Output     CheckOutput `json:"output,omitempty"`
	Gate       bool        `json:"-"`
}

var conclusions = map[string]bool{"success": true, "failure": true, "neutral": true, "cancelled": true, "skipped": true, "timed_out": true, "action_required": true}

func (x CheckRunInput) validate() error {
	if strings.TrimSpace(x.Name) == "" || strings.TrimSpace(x.HeadSHA) == "" {
		return errors.New("github: check name and head SHA are required")
	}
	if x.Status != "" && x.Status != "queued" && x.Status != "in_progress" && x.Status != "completed" {
		return fmt.Errorf("github: invalid check status %q", x.Status)
	}
	if x.Conclusion != "" {
		if x.Status != "" && x.Status != "completed" {
			return errors.New("github: conclusion requires completed status")
		}
		if !conclusions[x.Conclusion] {
			return fmt.Errorf("github: invalid check conclusion %q", x.Conclusion)
		}
		if x.Gate && (x.Conclusion == "neutral" || x.Conclusion == "skipped") {
			return fmt.Errorf("github: conclusion %q cannot be used for a gate", x.Conclusion)
		}
	}
	return nil
}
func (i *InstallationClient) CreateCheckRun(ctx context.Context, owner, repo string, x CheckRunInput) (CheckRun, error) {
	var r CheckRun
	if e := x.validate(); e != nil {
		return r, e
	}
	x.Gate = false
	e := i.request(ctx, http.MethodPost, "/repos/"+q(owner)+"/"+q(repo)+"/check-runs", x, &r)
	return r, e
}
func (i *InstallationClient) UpdateCheckRun(ctx context.Context, owner, repo string, id int64, x CheckRunInput) (CheckRun, error) {
	var r CheckRun
	if id <= 0 {
		return r, errors.New("github: check run id must be positive")
	}
	if e := x.validate(); e != nil {
		return r, e
	}
	x.Gate = false
	e := i.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/check-runs/%d", q(owner), q(repo), id), x, &r)
	return r, e
}

// ListCheckRuns returns check runs attached to a commit SHA. The caller must
// additionally match app ID and check name before claiming an ambiguous result.
func (i *InstallationClient) ListCheckRuns(ctx context.Context, owner, repo, sha string) ([]CheckRun, error) {
	var response struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	if strings.TrimSpace(sha) == "" {
		return nil, errors.New("github: check run commit SHA is required")
	}
	err := i.request(ctx, http.MethodGet, "/repos/"+q(owner)+"/"+q(repo)+"/commits/"+q(sha)+"/check-runs?per_page=100", nil, &response)
	return response.CheckRuns, err
}

// GateConclusion validates a gate result and rejects values that GitHub treats as non-failing.
func GateConclusion(value string) error {
	if !conclusions[value] {
		return fmt.Errorf("github: invalid check conclusion %q", value)
	}
	if value == "neutral" || value == "skipped" {
		return fmt.Errorf("github: conclusion %q cannot be used for a gate", value)
	}
	return nil
}

// GetAuthenticatedUser discovers the bot login used when matching comments.
func (i *InstallationClient) GetAuthenticatedUser(ctx context.Context) (User, error) {
	i.mu.Lock()
	login := i.login
	i.mu.Unlock()
	if login != "" {
		return User{Login: login, Type: "Bot"}, nil
	}
	var app struct {
		Slug string `json:"slug"`
	}
	e := i.app.appRequest(ctx, http.MethodGet, "/app", nil, &app)
	if e != nil {
		return User{}, e
	}
	if app.Slug == "" {
		return User{}, errors.New("github: app slug is unavailable")
	}
	u := User{Login: app.Slug + "[bot]", Type: "Bot"}
	i.mu.Lock()
	i.login = u.Login
	i.mu.Unlock()
	return u, nil
}
