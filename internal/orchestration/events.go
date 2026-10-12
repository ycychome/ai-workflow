package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/store"
)

func (e *Engine) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
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
		if issue.State == "closed" && x.PR == 0 && x.Control == "ACTIVE" {
			x.Generation++
			x.Control = "PAUSING"
			x.LastError = "source issue was closed; maintainer must confirm whether work should continue"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if x.PR == 0 {
			return nil, store.ErrNoChange
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
			x.Control = "PAUSING"
			x.LastError = "pull request head changed outside the current workflow generation"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if pr.Base.SHA != "" && x.BaseSHA != "" && pr.Base.SHA != x.BaseSHA {
			x.Generation++
			x.BaseSHA = pr.Base.SHA
			x.Control = "ACTIVE"
			x.Phase = "VERIFYING"
			x.Review.Verdict = "OBSOLETE"
			x.LastError = "base branch changed; candidate must be reverified"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
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
