package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const actionMarkerPrefix = "<!-- ai-workflow-action-id:"

func actionMarker(id string) (string, error) {
	if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "<>\r\n") {
		return "", errors.New("github: invalid action id")
	}
	return actionMarkerPrefix + id + " -->", nil
}

// UpsertActionComment creates or updates the comment for actionID, matching only comments authored by this bot.
func (i *InstallationClient) UpsertActionComment(ctx context.Context, owner, repo string, number int, actionID, body string) (IssueComment, error) {
	marker, e := actionMarker(actionID)
	if e != nil {
		return IssueComment{}, e
	}
	u, e := i.GetAuthenticatedUser(ctx)
	if e != nil {
		return IssueComment{}, e
	}
	if u.Login == "" {
		return IssueComment{}, errors.New("github: authenticated bot login is unavailable")
	}
	comments, e := i.ListIssueComments(ctx, owner, repo, number)
	if e != nil {
		return IssueComment{}, e
	}
	payload := map[string]string{"body": marker + "\n" + body}
	for _, c := range comments {
		if c.User.Login == u.Login && strings.Contains(c.Body, marker) {
			var updated IssueComment
			e = i.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/issues/comments/%d", q(owner), q(repo), c.ID), payload, &updated)
			return updated, e
		}
	}
	var created IssueComment
	e = i.request(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", q(owner), q(repo), number), payload, &created)
	return created, e
}

type PullRequestInput struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}

func (i *InstallationClient) CreatePullRequest(ctx context.Context, owner, repo string, x PullRequestInput) (PullRequest, error) {
	var pr PullRequest
	if strings.TrimSpace(x.Title) == "" || strings.TrimSpace(x.Head) == "" || strings.TrimSpace(x.Base) == "" {
		return pr, errors.New("github: pull request title, head and base are required")
	}
	e := i.request(ctx, http.MethodPost, "/repos/"+q(owner)+"/"+q(repo)+"/pulls", x, &pr)
	return pr, e
}

func (i *InstallationClient) FindPullRequestByHead(ctx context.Context, owner, repo, head, base string) (*PullRequest, error) {
	bot, e := i.GetAuthenticatedUser(ctx)
	if e != nil {
		return nil, e
	}
	path := "/repos/" + q(owner) + "/" + q(repo) + "/pulls?state=all&per_page=100&head=" + q(owner+":"+head)
	var xs []PullRequest
	if e := i.request(ctx, http.MethodGet, path, nil, &xs); e != nil {
		return nil, e
	}
	for n := range xs {
		if xs[n].User.Login == bot.Login && xs[n].Head.Ref == head && (base == "" || xs[n].Base.Ref == base) {
			return &xs[n], nil
		}
	}
	return nil, nil
}

// ClaimPullRequest reuses an existing PR for the head/base pair or creates one.
func (i *InstallationClient) ClaimPullRequest(ctx context.Context, owner, repo string, x PullRequestInput) (PullRequest, error) {
	p, e := i.FindPullRequestByHead(ctx, owner, repo, x.Head, x.Base)
	if e != nil {
		return PullRequest{}, e
	}
	if p != nil {
		return *p, nil
	}
	created, e := i.CreatePullRequest(ctx, owner, repo, x)
	if e == nil {
		return created, nil
	}
	if p2, re := i.FindPullRequestByHead(ctx, owner, repo, x.Head, x.Base); re == nil && p2 != nil {
		return *p2, nil
	}
	return PullRequest{}, e
}

func (i *InstallationClient) SetPullRequestDraft(ctx context.Context, owner, repo string, number int, draft bool) (PullRequest, error) {
	var p PullRequest
	if number <= 0 {
		return p, errors.New("github: pull request number must be positive")
	}
	p, e := i.GetPullRequest(ctx, owner, repo, number)
	if e != nil {
		return p, e
	}
	if p.NodeID == "" {
		return p, errors.New("github: pull request node_id is unavailable")
	}
	if p.Draft == draft {
		return p, nil
	}
	mutation := "convertPullRequestToDraft"
	if !draft {
		mutation = "markPullRequestReadyForReview"
	}
	query := fmt.Sprintf("mutation($id: ID!) { %s(input: {pullRequestId: $id}) { pullRequest { id } } }", mutation)
	var response struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": query, "variables": map[string]string{"id": p.NodeID}}
	if e = i.request(ctx, http.MethodPost, "/graphql", body, &response); e != nil {
		return p, e
	}
	if len(response.Errors) != 0 {
		return p, fmt.Errorf("github: GraphQL %s failed: %s", mutation, response.Errors[0].Message)
	}
	updated, err := i.GetPullRequest(ctx, owner, repo, number)
	if err != nil {
		return p, err
	}
	if updated.Draft != draft {
		return updated, fmt.Errorf("github: pull request draft state did not change to %t", draft)
	}
	return updated, nil
}
func (i *InstallationClient) MarkPullRequestReady(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	return i.SetPullRequestDraft(ctx, owner, repo, number, false)
}

const OwnedPRBodyStartMarker = "<!-- ai-workflow:owned-pr-body:start -->"
const OwnedPRBodyEndMarker = "<!-- ai-workflow:owned-pr-body:end -->"

func mergeOwnedPRBody(current, generated string) (string, error) {
	start := strings.Index(current, OwnedPRBodyStartMarker)
	end := strings.Index(current, OwnedPRBodyEndMarker)
	block := OwnedPRBodyStartMarker + "\n" + generated + "\n" + OwnedPRBodyEndMarker
	if start < 0 && end < 0 {
		if strings.TrimSpace(current) == "" {
			return block, nil
		}
		return current + "\n\n" + block, nil
	}
	if start < 0 || end < start {
		return "", errors.New("github: malformed owned pull request body markers")
	}
	end += len(OwnedPRBodyEndMarker)
	return current[:start] + block + current[end:], nil
}

// UpdateOwnedPRBody replaces only the bot-marked region and preserves all human-authored text.
func (i *InstallationClient) UpdateOwnedPRBody(ctx context.Context, owner, repo string, number int, generated string) (PullRequest, error) {
	pr, err := i.GetPullRequest(ctx, owner, repo, number)
	if err != nil {
		return pr, err
	}
	bot, err := i.GetAuthenticatedUser(ctx)
	if err != nil {
		return pr, err
	}
	if pr.User.Login != bot.Login {
		return pr, errors.New("github: refusing to update a pull request not authored by this bot")
	}
	body, err := mergeOwnedPRBody(pr.Body, generated)
	if err != nil {
		return pr, err
	}
	returned := PullRequest{}
	err = i.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/pulls/%d", q(owner), q(repo), number), map[string]string{"body": body}, &returned)
	return returned, err
}

type FileChange struct {
	Path    string
	Content []byte
	Delete  bool
	Mode    string
}
type PublishResult struct {
	CommitSHA string `json:"sha"`
	TreeSHA   string `json:"tree_sha"`
	Ref       string `json:"ref"`
}

type CommitInfo struct {
	SHA     string
	Message string
	TreeSHA string
	Parents []string
}

func (i *InstallationClient) GetCommitInfo(ctx context.Context, owner, repo, sha string) (CommitInfo, error) {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" || strings.TrimSpace(sha) == "" {
		return CommitInfo{}, errors.New("github: owner, repo, and commit SHA are required")
	}
	var response struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Tree    struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	err := i.request(ctx, http.MethodGet, "/repos/"+q(owner)+"/"+q(repo)+"/git/commits/"+q(sha), nil, &response)
	if err != nil {
		return CommitInfo{}, err
	}
	if strings.TrimSpace(response.SHA) == "" || !strings.EqualFold(response.SHA, sha) {
		return CommitInfo{}, errors.New("github: commit response SHA is empty or does not match the requested SHA")
	}
	if strings.TrimSpace(response.Tree.SHA) == "" {
		return CommitInfo{}, errors.New("github: commit response tree SHA is empty")
	}
	if strings.TrimSpace(response.Message) == "" {
		return CommitInfo{}, errors.New("github: commit response message is empty")
	}
	info := CommitInfo{SHA: response.SHA, Message: response.Message, TreeSHA: response.Tree.SHA}
	for _, p := range response.Parents {
		if strings.TrimSpace(p.SHA) == "" {
			return CommitInfo{}, errors.New("github: commit response contains an empty parent SHA")
		}
		info.Parents = append(info.Parents, p.SHA)
	}
	return info, nil
}

type gitCommitResponse struct {
	SHA  string `json:"sha"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
}
type gitRefResponse struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}
type treeEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"`
}

// GetBranchSHA returns the current head commit for a branch.
func (i *InstallationClient) GetBranchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	if strings.TrimSpace(branch) == "" {
		return "", errors.New("github: branch is required")
	}
	var ref gitRefResponse
	path := "/repos/" + q(owner) + "/" + q(repo) + "/git/ref/heads/" + q(branch)
	if err := i.request(ctx, http.MethodGet, path, nil, &ref); err != nil {
		return "", err
	}
	return ref.Object.SHA, nil
}

// CreateBranch creates a branch at sha. If the response is lost, it confirms the result by rereading the ref.
func (i *InstallationClient) CreateBranch(ctx context.Context, owner, repo, branch, sha string) (string, error) {
	if strings.TrimSpace(branch) == "" || strings.TrimSpace(sha) == "" {
		return "", errors.New("github: branch and SHA are required")
	}
	path := "/repos/" + q(owner) + "/" + q(repo) + "/git/refs"
	var ref gitRefResponse
	err := i.request(ctx, http.MethodPost, path, map[string]string{"ref": "refs/heads/" + branch, "sha": sha}, &ref)
	if err == nil {
		return ref.Object.SHA, nil
	}
	var unknown *OutcomeUnknownError
	if errors.As(err, &unknown) {
		if current, readErr := i.GetBranchSHA(ctx, owner, repo, branch); readErr == nil && current == sha {
			return current, nil
		}
	}
	return "", err
}

// GetArchive downloads a repository ZIP snapshot for the requested branch, tag, or commit.
func (i *InstallationClient) GetArchive(ctx context.Context, owner, repo, ref string) ([]byte, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("github: archive ref is required")
	}
	path := "/repos/" + q(owner) + "/" + q(repo) + "/zipball/" + q(ref)
	return i.DoBytes(ctx, http.MethodGet, path, nil)
}

// PublishBranch commits the supplied file changes with expectedParent as the sole parent,
// verifies the remote branch still points at that parent, then advances it without force.
// It never checks out, executes, or runs hooks on repository contents.
func (i *InstallationClient) PublishBranch(ctx context.Context, owner, repo, branch, expectedParent, message string, changes []FileChange) (PublishResult, error) {
	var result PublishResult
	if owner == "" || repo == "" || branch == "" || expectedParent == "" || message == "" {
		return result, errors.New("github: owner, repo, branch, expected parent and commit message are required")
	}
	if len(changes) == 0 {
		return result, errors.New("github: at least one file change is required")
	}
	var ref gitRefResponse
	refPath := "/repos/" + q(owner) + "/" + q(repo) + "/git/ref/heads/" + q(branch)
	if e := i.request(ctx, http.MethodGet, refPath, nil, &ref); e != nil {
		return result, e
	}
	if ref.Object.SHA != expectedParent {
		return result, fmt.Errorf("github: branch moved: expected %s, found %s", expectedParent, ref.Object.SHA)
	}
	var parent gitCommitResponse
	if e := i.request(ctx, http.MethodGet, "/repos/"+q(owner)+"/"+q(repo)+"/git/commits/"+q(expectedParent), nil, &parent); e != nil {
		return result, e
	}
	if parent.Tree.SHA == "" {
		return result, errors.New("github: parent commit has no tree")
	}
	entries := make([]treeEntry, 0, len(changes))
	seen := map[string]bool{}
	for _, f := range changes {
		if f.Path == "" || strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") || strings.Contains(f.Path, "\\") || seen[f.Path] {
			return result, fmt.Errorf("github: invalid or duplicate file path %q", f.Path)
		}
		seen[f.Path] = true
		if f.Delete {
			entries = append(entries, treeEntry{Path: f.Path, Mode: "100644", Type: "blob", SHA: nil})
			continue
		}
		mode := f.Mode
		if mode == "" {
			mode = "100644"
		}
		if mode != "100644" && mode != "100755" {
			return result, fmt.Errorf("github: unsupported file mode %q", mode)
		}
		var blob struct {
			SHA string `json:"sha"`
		}
		payload := map[string]string{"content": base64.StdEncoding.EncodeToString(f.Content), "encoding": "base64"}
		if e := i.request(ctx, http.MethodPost, "/repos/"+q(owner)+"/"+q(repo)+"/git/blobs", payload, &blob); e != nil {
			return result, e
		}
		sha := blob.SHA
		entries = append(entries, treeEntry{Path: f.Path, Mode: mode, Type: "blob", SHA: &sha})
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if e := i.request(ctx, http.MethodPost, "/repos/"+q(owner)+"/"+q(repo)+"/git/trees", map[string]any{"base_tree": parent.Tree.SHA, "tree": entries}, &tree); e != nil {
		return result, e
	}
	if tree.SHA == "" {
		return result, errors.New("github: tree creation returned empty SHA")
	}
	var commit gitCommitResponse
	if e := i.request(ctx, http.MethodPost, "/repos/"+q(owner)+"/"+q(repo)+"/git/commits", map[string]any{"message": message, "tree": tree.SHA, "parents": []string{expectedParent}}, &commit); e != nil {
		return result, e
	}
	if commit.SHA == "" {
		return result, errors.New("github: commit creation returned empty SHA")
	}
	// Recheck immediately before ref update. The non-force API update rejects diverged histories.
	var latest gitRefResponse
	if e := i.request(ctx, http.MethodGet, refPath, nil, &latest); e != nil {
		return result, e
	}
	if latest.Object.SHA != expectedParent {
		return result, fmt.Errorf("github: branch moved before publish: expected %s, found %s", expectedParent, latest.Object.SHA)
	}
	updatePath := "/repos/" + q(owner) + "/" + q(repo) + "/git/refs/heads/" + q(branch)
	var updated gitRefResponse
	e := i.request(ctx, http.MethodPatch, updatePath, map[string]any{"sha": commit.SHA, "force": false}, &updated)
	if e != nil {
		var unknown *OutcomeUnknownError
		if errors.As(e, &unknown) {
			if current, readErr := i.GetBranchSHA(ctx, owner, repo, branch); readErr == nil && current == commit.SHA {
				return PublishResult{CommitSHA: commit.SHA, TreeSHA: tree.SHA, Ref: "refs/heads/" + branch}, nil
			}
		}
		return result, e
	}
	return PublishResult{CommitSHA: commit.SHA, TreeSHA: tree.SHA, Ref: updated.Ref}, nil
}
