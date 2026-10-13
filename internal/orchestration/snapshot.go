package orchestration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

// verificationSnapshot returns the source tree that verification must run
// against. For a real pull request this is GitHub's synthetic merge commit,
// which combines the exact base and head currently recorded on the workflow.
func (e *Engine) verificationSnapshot(ctx context.Context, w *domain.Workflow) (files map[string]sandbox.File, pending bool, err error) {
	if w == nil {
		return nil, false, errors.New("workflow is required")
	}
	if w.Repository == "local/pilot" {
		files, err = e.source(ctx, *w, w.HeadSHA)
		if err != nil {
			return nil, false, err
		}
		w.TestedSHA = w.HeadSHA
		w.TestedTreeSHA = contentDigest(files)
		return files, false, nil
	}
	if w.PR < 1 || w.HeadSHA == "" || w.BaseSHA == "" {
		return nil, false, errors.New("pull request, head SHA and base SHA are required for verification")
	}

	c, owner, repo, err := e.client(ctx, *w)
	if err != nil {
		return nil, false, err
	}
	var pr struct {
		State          string `json:"state"`
		Merged         bool   `json:"merged"`
		Mergeable      *bool  `json:"mergeable"`
		MergeableState string `json:"mergeable_state"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), w.PR)
	if err := c.Do(ctx, http.MethodGet, path, nil, &pr); err != nil {
		return nil, false, err
	}
	if pr.Head.SHA != w.HeadSHA || pr.Base.SHA != w.BaseSHA {
		return nil, false, store.ErrStale
	}
	if !strings.EqualFold(pr.State, "open") || pr.Merged {
		return nil, false, store.ErrStale
	}
	if pr.Mergeable == nil {
		return nil, true, nil
	}
	if !*pr.Mergeable {
		state := strings.TrimSpace(pr.MergeableState)
		if state == "" {
			state = "unknown"
		}
		return nil, false, fmt.Errorf("pull request cannot be merged cleanly (mergeable_state=%s)", state)
	}
	if strings.TrimSpace(pr.MergeCommitSHA) == "" {
		return nil, true, nil
	}

	var mergeCommit struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	commitPath := fmt.Sprintf("/repos/%s/%s/git/commits/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(pr.MergeCommitSHA))
	if err := c.Do(ctx, http.MethodGet, commitPath, nil, &mergeCommit); err != nil {
		return nil, false, err
	}
	if len(mergeCommit.Parents) != 2 || mergeCommit.Tree.SHA == "" {
		return nil, false, store.ErrStale
	}
	parents := map[string]int{}
	for _, parent := range mergeCommit.Parents {
		parents[parent.SHA]++
	}
	if parents[w.BaseSHA] != 1 || parents[w.HeadSHA] != 1 || mergeCommit.SHA != "" && mergeCommit.SHA != pr.MergeCommitSHA {
		return nil, false, store.ErrStale
	}

	files, err = e.source(ctx, *w, pr.MergeCommitSHA)
	if err != nil {
		return nil, false, err
	}
	w.TestedSHA = pr.MergeCommitSHA
	w.TestedTreeSHA = mergeCommit.Tree.SHA
	return files, false, nil
}

// reviewSnapshot is the exact tree used by trusted verification. A branch head
// alone omits changes already present on the target branch, so source review,
// candidate diffs and supplemental tests must all use the recorded test merge.
func (e *Engine) reviewSnapshot(ctx context.Context, w domain.Workflow) (map[string]sandbox.File, error) {
	if w.TestedSHA == "" || w.TestedTreeSHA == "" {
		return nil, errors.New("review requires the exact verified commit and tree")
	}
	if w.Repository != "local/pilot" {
		c, owner, repo, err := e.client(ctx, w)
		if err != nil {
			return nil, err
		}
		commit, err := c.GetCommitInfo(ctx, owner, repo, w.TestedSHA)
		if err != nil {
			return nil, err
		}
		parents := map[string]int{}
		for _, parent := range commit.Parents {
			parents[parent]++
		}
		if commit.TreeSHA != w.TestedTreeSHA || len(commit.Parents) != 2 || parents[w.BaseSHA] != 1 || parents[w.HeadSHA] != 1 {
			return nil, store.ErrStale
		}
	}
	files, err := e.source(ctx, w, w.TestedSHA)
	if err != nil {
		return nil, err
	}
	if w.Repository == "local/pilot" && (w.TestedSHA != w.HeadSHA || contentDigest(files) != w.TestedTreeSHA) {
		return nil, store.ErrStale
	}
	return files, nil
}
