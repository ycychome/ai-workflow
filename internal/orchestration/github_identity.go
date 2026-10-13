package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

// remotePRIdentityReason reports why a live PR does not identify the exact
// candidate stored and reviewed by this workflow.
func remotePRIdentityReason(w domain.Workflow, pr gh.PullRequest) string {
	if pr.Number != w.PR {
		return fmt.Sprintf("PR number changed: expected #%d, found #%d", w.PR, pr.Number)
	}
	if pr.Merged {
		return "pull request is already merged"
	}
	if pr.State != "open" {
		return fmt.Sprintf("pull request is %s, not open", pr.State)
	}
	if pr.Draft {
		return "pull request is draft; a ready PR is required for a successful gate"
	}
	if pr.Head.Repo == nil || pr.Base.Repo == nil || pr.Head.Repo.ID != w.RepositoryID || pr.Base.Repo.ID != w.RepositoryID || !strings.EqualFold(pr.Head.Repo.FullName, w.Repository) || !strings.EqualFold(pr.Base.Repo.FullName, w.Repository) {
		return "pull request head or base repository identity changed"
	}
	if pr.Head.Ref == "" || pr.Head.Ref != w.Branch {
		return fmt.Sprintf("pull request head branch changed: expected %q, found %q", w.Branch, pr.Head.Ref)
	}
	if pr.Base.Ref == "" || pr.Base.Ref != w.BaseBranch {
		return fmt.Sprintf("pull request base branch changed: expected %q, found %q", w.BaseBranch, pr.Base.Ref)
	}
	if pr.Head.SHA == "" || pr.Head.SHA != w.HeadSHA {
		return fmt.Sprintf("pull request head changed: expected %s, found %s", w.HeadSHA, pr.Head.SHA)
	}
	if pr.Base.SHA == "" || pr.Base.SHA != w.BaseSHA {
		return fmt.Sprintf("pull request base changed: expected %s, found %s", w.BaseSHA, pr.Base.SHA)
	}
	return ""
}

func (e *Engine) gateActionMatchesCurrent(ctx context.Context, w domain.Workflow, a store.Action, d gateActionData, manual bool) bool {
	if !d.Pass || w.Generation != a.Generation || w.Control != "ACTIVE" || (w.Phase != "READY" && w.Phase != "AWAITING_MAINTAINER") ||
		!w.PRReady || w.PR <= 0 || w.HeadSHA == "" || d.SHA != w.HeadSHA || d.Key == "" {
		return false
	}
	key := e.key(ctx, w)
	if key != d.Key || w.Review.Key != d.Key {
		return false
	}
	if manual {
		return w.Phase == "AWAITING_MAINTAINER" && manualForceIdentity(w, w.ForceApproval, key, domain.Digest(e.repository(w)))
	}
	passed, _ := domain.Gate(w, key)
	return passed
}

func (e *Engine) invalidateLocalGateIdentity(ctx context.Context, expected domain.Workflow, reason string) error {
	return e.Store.Update(ctx, expected.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Generation != expected.Generation || x.Version != expected.Version {
			return nil, store.ErrStale
		}
		x.Generation++
		x.Review.Verdict = "OBSOLETE"
		x.Review.VerificationPassed = false
		x.Review.SkepticComplete = false
		x.Phase = "VERIFYING"
		x.Control = "PAUSING"
		x.LastError = "gate authorization is stale: " + reason
		return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
	})
}

func (e *Engine) revokeGateSuccess(ctx context.Context, c *gh.InstallationClient, owner, repo string, check gh.CheckRun, prior gh.CheckRunInput, reason string) error {
	if check.ID <= 0 {
		return errors.New("cannot revoke an unclaimed successful check run")
	}
	failure := prior
	failure.Status = "completed"
	failure.Conclusion = "failure"
	failure.Output.Summary = "Success was revoked because current PR identity could not be confirmed: " + reason
	_, err := c.UpdateCheckRun(ctx, owner, repo, check.ID, failure)
	return err
}

func (e *Engine) inFlightPublishMatches(ctx context.Context, w domain.Workflow, pr gh.PullRequest) (bool, error) {
	var action store.Action
	err := e.Store.DB.QueryRow(ctx, `SELECT id,workflow_id,generation,kind,data,attempts
FROM outbox WHERE workflow_id=$1 AND kind='publish' AND state IN ('running','unknown')
ORDER BY created_at DESC,id DESC LIMIT 1`, w.ID).Scan(&action.ID, &action.WorkflowID, &action.Generation, &action.Kind, &action.Data, &action.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if action.Generation != w.Generation || w.Control != "ACTIVE" {
		return false, nil
	}
	return e.publishIntentMatchesPR(ctx, w, pr, action)
}

func (e *Engine) publishIntentMatchesPR(ctx context.Context, w domain.Workflow, pr gh.PullRequest, action store.Action) (bool, error) {
	if action.Kind != "publish" || action.WorkflowID != w.ID || action.Generation != w.Generation || w.Control != "ACTIVE" || pr.Number <= 0 || pr.Merged || pr.State != "open" {
		return false, nil
	}
	var d publishActionData
	if err := json.Unmarshal(action.Data, &d); err != nil {
		return false, err
	}
	expectedParent := w.BaseSHA
	if w.PR > 0 {
		expectedParent = w.HeadSHA
	}
	if d.Branch == "" || d.ExpectedParent == "" || d.Message == "" || d.ExpectedParent != expectedParent || d.BaseSHA != w.BaseSHA || d.BaseBranch != w.BaseBranch || d.Branch != w.Branch || pr.Head.Ref != d.Branch || pr.Base.Ref != d.BaseBranch || pr.Base.SHA != d.BaseSHA || pr.Head.SHA == "" ||
		pr.Head.Repo == nil || pr.Base.Repo == nil || pr.Head.Repo.ID != w.RepositoryID || pr.Base.Repo.ID != w.RepositoryID ||
		!strings.EqualFold(pr.Head.Repo.FullName, w.Repository) || !strings.EqualFold(pr.Base.Repo.FullName, w.Repository) {
		return false, nil
	}
	client, owner, repo, err := e.client(ctx, w)
	if err != nil {
		return false, err
	}
	commit, err := client.GetCommitInfo(ctx, owner, repo, pr.Head.SHA)
	if err != nil {
		return false, err
	}
	wantMessage := d.Message + "\n\nAI Workflow Action: " + action.ID
	if commit.SHA != pr.Head.SHA || commit.Message != wantMessage || len(commit.Parents) != 1 || commit.Parents[0] != d.ExpectedParent || commit.TreeSHA == "" {
		return false, nil
	}
	baseArchive, err := client.GetArchive(ctx, owner, repo, d.ExpectedParent)
	if err != nil {
		return false, err
	}
	headArchive, err := client.GetArchive(ctx, owner, repo, pr.Head.SHA)
	if err != nil {
		return false, err
	}
	baseFiles, err := decodeArchive(baseArchive)
	if err != nil {
		return false, err
	}
	headFiles, err := decodeArchive(headArchive)
	if err != nil {
		return false, err
	}
	return publishChangesExactlyMatch(baseFiles, headFiles, d.Changes), nil
}

func publishChangesExactlyMatch(base, head map[string]sandbox.File, changes []gh.FileChange) bool {
	if len(changes) == 0 {
		return false
	}
	wanted := make(map[string]gh.FileChange, len(changes))
	for _, c := range changes {
		if c.Path == "" || wanted[c.Path].Path != "" {
			return false
		}
		wanted[c.Path] = c
	}
	paths := make(map[string]bool, len(base)+len(head))
	for path := range base {
		paths[path] = true
	}
	for path := range head {
		paths[path] = true
	}
	seen := make(map[string]bool, len(changes))
	for path := range paths {
		before, hadBefore := base[path]
		after, hasAfter := head[path]
		modeUnchanged := !hadBefore
		if hadBefore && hasAfter {
			beforeMode, beforeErr := sandbox.NormalizeGitFileMode(before.Mode)
			afterMode, afterErr := sandbox.NormalizeGitFileMode(after.Mode)
			modeUnchanged = beforeErr == nil && afterErr == nil && beforeMode == afterMode
		}
		if hadBefore == hasAfter && (!hadBefore || (modeUnchanged && bytes.Equal(before.Data, after.Data))) {
			continue
		}
		change, ok := wanted[path]
		if !ok {
			return false
		}
		if change.Delete {
			if !hadBefore || hasAfter {
				return false
			}
		} else {
			mode := uint32(0644)
			if change.Mode == "100755" {
				mode = 0755
			} else if change.Mode != "" && change.Mode != "100644" {
				return false
			}
			afterMode, modeErr := sandbox.NormalizeGitFileMode(after.Mode)
			if !hasAfter || modeErr != nil || uint32(afterMode) != mode || !bytes.Equal(after.Data, change.Content) {
				return false
			}
		}
		seen[path] = true
	}
	return len(seen) == len(wanted)
}

func (e *Engine) recordGateIdentityDrift(ctx context.Context, expected domain.Workflow, pr gh.PullRequest, reason string) error {
	return e.Store.Update(ctx, expected.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Generation != expected.Generation || x.Version != expected.Version || x.HeadSHA != expected.HeadSHA || x.BaseSHA != expected.BaseSHA {
			return nil, store.ErrStale
		}
		x.Generation++
		x.Review.Verdict = "OBSOLETE"
		x.Review.VerificationPassed = false
		x.Review.SkepticComplete = false
		x.LastError = "PR identity changed before gate publication: " + reason
		if pr.Merged {
			x.Phase = "MERGED"
			x.Control = "COMPLETED"
			return []store.Action{statusAction(*x)}, nil
		}
		if pr.State != "open" || pr.Draft || pr.Head.Ref == "" || pr.Head.Ref != x.Branch || pr.Head.Repo == nil || pr.Base.Repo == nil || pr.Head.Repo.ID != x.RepositoryID || pr.Base.Repo.ID != x.RepositoryID || !strings.EqualFold(pr.Head.Repo.FullName, x.Repository) || !strings.EqualFold(pr.Base.Repo.FullName, x.Repository) {
			x.Phase = "BLOCKED"
			x.Control = "PAUSED"
			x.PRReady = false
		} else {
			baseOnly := pr.Head.SHA == x.HeadSHA && pr.Base.Ref == x.BaseBranch && pr.Base.SHA != "" && pr.Base.SHA != x.BaseSHA
			if pr.Head.SHA != "" {
				x.HeadSHA = pr.Head.SHA
			}
			if pr.Base.SHA != "" {
				x.BaseSHA = pr.Base.SHA
			}
			if pr.Base.Ref != "" {
				x.BaseBranch = pr.Base.Ref
			}
			x.Phase = "VERIFYING"
			if !baseOnly && x.Control == "ACTIVE" {
				x.Control = "PAUSING"
			}
			if baseOnly && unfinishedDevelopment(x.Tasks) {
				x.Phase = "DEVELOPING"
			}
		}
		return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
	})
}
