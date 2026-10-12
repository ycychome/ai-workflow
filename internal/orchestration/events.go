package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/store"
)

func (e *Engine) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	historicalPages := make(map[string]int)
	// Run discovery immediately on startup so issues created during a webhook
	// outage do not wait for the first periodic reconciliation interval.
	e.discoverOpenIssues(ctx, historicalPages)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.discoverOpenIssues(ctx, historicalPages)
		workflows, err := e.Store.List(ctx)
		if err != nil {
			continue
		}
		for _, w := range workflows {
			if ctx.Err() != nil {
				return
			}
			if w.Repository == "local/pilot" || w.Phase == "MERGED" || w.Control == "CANCELLED" {
				continue
			}
			_ = e.reconcileOne(ctx, w)
		}
	}
}

// discoverOpenIssues repairs missed issues/opened deliveries using current
// GitHub state. It reads two bounded pages per configured repository and only
// creates INTAKE workflows; it never replays comments or authorizes work.
func (e *Engine) discoverOpenIssues(ctx context.Context, historicalPages map[string]int) {
	cfg := e.Config()
	if len(cfg.Repositories) == 0 || cfg.GitHub.AppID <= 0 || cfg.GitHub.InstallationID <= 0 {
		return
	}
	app, err := e.App()
	if err != nil {
		slog.Warn("issue discovery unavailable", "error", err)
		return
	}
	for _, repoCfg := range cfg.Repositories {
		if ctx.Err() != nil {
			return
		}
		parts := strings.Split(repoCfg.Name, "/")
		if repoCfg.ID <= 0 || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		client := app.ForInstallation(ctx, cfg.GitHub.InstallationID)
		issues, err := client.ListOpenIssuesPage(ctx, parts[0], parts[1], 1)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("discover open GitHub issues", "repository", repoCfg.Name, "error", err)
				e.Store.Audit(context.WithoutCancel(ctx), "", "issue_discovery_error", map[string]any{"repository": repoCfg.Name, "page": 1, "error": err.Error()})
			}
		}
		page := historicalPages[repoCfg.Name]
		if page < 2 {
			page = 2
		}
		history, historyErr := client.ListOpenIssuesPage(ctx, parts[0], parts[1], page)
		if historyErr != nil {
			if ctx.Err() == nil {
				slog.Warn("discover historical GitHub issues", "repository", repoCfg.Name, "page", page, "error", historyErr)
				e.Store.Audit(context.WithoutCancel(ctx), "", "issue_discovery_error", map[string]any{"repository": repoCfg.Name, "page": page, "error": historyErr.Error()})
			}
		} else {
			issues = append(issues, history...)
			if len(history) < 100 {
				historicalPages[repoCfg.Name] = 2
			} else {
				historicalPages[repoCfg.Name] = page + 1
			}
		}
		for _, issue := range issues {
			if ctx.Err() != nil {
				return
			}
			if !discoverableIssue(issue) {
				continue
			}
			if err := e.createIntake(ctx, repoCfg, cfg.GitHub.InstallationID, issue, "reconcile_discovery"); err != nil && ctx.Err() == nil {
				slog.Warn("recover missed GitHub issue", "repository", repoCfg.Name, "issue", issue.Number, "error", err)
				e.Store.Audit(context.WithoutCancel(ctx), domain.ID(repoCfg.ID, issue.Number), "issue_discovery_error", map[string]any{"repository": repoCfg.Name, "issue": issue.Number, "error": err.Error()})
			}
		}
	}
}

func discoverableIssue(issue gh.Issue) bool {
	return issue.Number > 0 && issue.State == "open" && issue.PullRequest == nil
}

// reconcileOne refreshes authoritative issue, PR and branch state. Webhooks are
// hints; this read repairs missed deliveries and never treats a closed PR as merged.
func (e *Engine) reconcileOne(ctx context.Context, w domain.Workflow) error {
	if w.Repository == "local/pilot" {
		return nil
	}
	c, owner, repo, err := e.client(ctx, w)
	if err != nil {
		return err
	}
	issue, err := c.GetIssue(ctx, owner, repo, w.Issue)
	if err != nil {
		return err
	}
	var pr gh.PullRequest
	if w.PR > 0 {
		pr, err = c.GetPullRequest(ctx, owner, repo, w.PR)
		if err != nil {
			return err
		}
	} else if w.Branch != "" {
		found, findErr := c.FindPullRequestByHead(ctx, owner, repo, w.Branch, "")
		if findErr != nil {
			return findErr
		}
		if found != nil {
			pr = *found
		}
	}
	var gate *gh.CheckRun
	if pr.Number > 0 && pr.Head.SHA != "" {
		checks, checkErr := c.ListCheckRuns(ctx, owner, repo, pr.Head.SHA)
		if checkErr != nil {
			return checkErr
		}
		for i := range checks {
			if checks[i].Name == "workflow/merge-gate" && checks[i].App.ID == e.Config().GitHub.AppID {
				gate = &checks[i]
				break
			}
		}
	}
	return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if normalizedIssueHash(issue.Body) != normalizedIssueHash(x.IssueBody) {
			x.IssueTitle = issue.Title
			x.IssueBody = issue.Body
			x.ProposalSpec = nil
			x.Generation++
			x.Control = "PAUSING"
			x.LastError = "Issue changed; maintainer acceptance is required before specification updates."
			x.Discussion = append(x.Discussion, "Issue edit proposal detected during reconciliation.")
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if issue.State == "closed" && !pr.Merged && x.Control == "ACTIVE" {
			x.Generation++
			x.Control = "PAUSING"
			x.LastError = "source issue was closed; maintainer must confirm whether work should continue"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if x.PR == 0 {
			if pr.Number == 0 {
				return nil, store.ErrNoChange
			}
			x.PR = pr.Number
			x.HeadSHA = pr.Head.SHA
			x.BaseSHA = pr.Base.SHA
			x.BaseBranch = pr.Base.Ref
			x.PRReady = !pr.Draft
			x.Generation++
			x.Review.Verdict = "OBSOLETE"
			if x.Spec.Approved && unfinishedDevelopment(x.Tasks) {
				x.Phase = "DEVELOPING"
			} else if x.Spec.Approved {
				x.Phase = "VERIFYING"
			}
			return []store.Action{gateAction(*x, false, "recovered a previously published pull request"), statusAction(*x)}, nil
		}
		if pr.Merged {
			if x.Phase == "MERGED" && x.Control == "COMPLETED" {
				return nil, store.ErrNoChange
			}
			x.Generation++
			x.Phase = "MERGED"
			x.Control = "COMPLETED"
			x.LastError = ""
			return []store.Action{statusAction(*x)}, nil
		}
		if pr.State == "closed" {
			if x.Phase != "BLOCKED" || x.Control != "PAUSED" {
				x.Generation++
				x.Phase = "BLOCKED"
				x.Control = "PAUSED"
				x.LastError = "pull request is closed without a confirmed merge"
				return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
			}
			return nil, store.ErrNoChange
		}
		if pr.Draft && x.PRReady && x.Phase != "DEVELOPING" && x.Control == "ACTIVE" {
			x.Generation++
			x.PRReady = false
			x.Control = "PAUSING"
			x.LastError = "pull request returned to draft; maintainer review required"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if pr.Head.SHA != "" && x.HeadSHA != "" && pr.Head.SHA != x.HeadSHA {
			// A commit from the current outbox may have landed before its receipt was
			// recorded. If there is an unfinished publish action, let it reconcile.
			pending, nErr := e.Store.PendingWrites(ctx, x.ID)
			if nErr == nil && pending > 0 {
				return nil, store.ErrNoChange
			}
			x.Generation++
			x.HeadSHA = pr.Head.SHA
			x.Control = "PAUSING"
			x.LastError = "pull request head changed outside the current workflow generation"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if pr.Base.Ref != "" && pr.Base.Ref != x.BaseBranch {
			x.Generation++
			x.BaseBranch = pr.Base.Ref
			x.BaseSHA = pr.Base.SHA
			x.Control = "PAUSING"
			x.Review.Verdict = "OBSOLETE"
			x.LastError = "PR target branch changed; resume to verify the current target"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if pr.Base.SHA != "" && x.BaseSHA != "" && pr.Base.SHA != x.BaseSHA {
			x.Generation++
			x.BaseSHA = pr.Base.SHA
			x.Phase = "VERIFYING"
			if unfinishedDevelopment(x.Tasks) {
				x.Phase = "DEVELOPING"
			}
			x.Review.Verdict = "OBSOLETE"
			x.LastError = "base branch changed; candidate must be reverified"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if gate != nil && x.HeadSHA == pr.Head.SHA && (x.CheckID != gate.ID || x.CheckSHA != pr.Head.SHA || x.GateStatus != gate.Status || x.GateConclusion != gate.Conclusion) {
			x.CheckID = gate.ID
			x.CheckSHA = pr.Head.SHA
			x.GateStatus = gate.Status
			x.GateConclusion = gate.Conclusion
			x.GateConfirmedAt = time.Now().UTC()
			return nil, nil
		}
		return nil, store.ErrNoChange
	})
}

// EventIngest stores an authenticated webhook delivery in the durable inbox.
// The HTTP layer verifies the original body signature before calling it.
func (e *Engine) EventIngest(ctx context.Context, deliveryID, event string, payload []byte) error {
	if deliveryID == "" || event == "" {
		return errors.New("GitHub delivery ID and event name are required")
	}
	var probe map[string]any
	if err := json.Unmarshal(payload, &probe); err != nil {
		return fmt.Errorf("invalid webhook payload: %w", err)
	}
	return e.Store.Inbox(ctx, deliveryID, event, payload)
}
