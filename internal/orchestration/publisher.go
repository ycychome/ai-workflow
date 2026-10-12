package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

type commentAction struct {
	Body string `json:"body"`
}
type gateActionData struct {
	Pass   bool   `json:"pass"`
	Reason string `json:"reason"`
	Key    string `json:"key,omitempty"`
	SHA    string `json:"sha,omitempty"`
}
type publishActionData struct {
	Changes        []gh.FileChange `json:"changes"`
	TaskIDs        []string        `json:"task_ids"`
	Message        string          `json:"message"`
	ExpectedParent string          `json:"expected_parent"`
	BaseBranch     string          `json:"base_branch"`
	BaseSHA        string          `json:"base_sha"`
	Branch         string          `json:"branch"`
}

// statusAction coalesces status updates into one bot-owned issue comment.
func statusAction(w domain.Workflow) store.Action {
	body := statusBody(w)
	return store.NewAction(w, "comment", map[string]any{"body": body, "marker": "status"})
}

func statusBody(w domain.Workflow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**AI workflow status:** `%s` · control `%s` · generation `%d`", w.Phase, w.Control, w.Generation)
	if w.LastError != "" {
		fmt.Fprintf(&b, "\n\n%s", w.LastError)
	}
	if w.PR > 0 {
		fmt.Fprintf(&b, "\n\nPull request: [#%d](https://github.com/%s/pull/%d)", w.PR, w.Repository, w.PR)
	}
	if w.Spec.Title != "" && w.Spec.Approved {
		fmt.Fprintf(&b, "\n\n**Accepted specification:** %s\n\n%s", w.Spec.Title, w.Spec.Description)
	}
	if w.ProposalSpec != nil {
		fmt.Fprintf(&b, "\n\n**Proposed specification:** %s\n\n%s", w.ProposalSpec.Title, w.ProposalSpec.Description)
	}
	criteria := w.Spec.Criteria
	criteriaTitle := "Acceptance criteria"
	if w.ProposalSpec != nil {
		criteria = w.ProposalSpec.Criteria
		criteriaTitle = "Proposed acceptance criteria"
	}
	if len(criteria) > 0 {
		fmt.Fprintf(&b, "\n\n**%s**\n", criteriaTitle)
		for _, c := range criteria {
			fmt.Fprintf(&b, "- %s: %s\n", c.ID, c.Description)
		}
	}
	if len(w.Tasks) > 0 {
		b.WriteString("\n**Tasks**\n")
		for _, t := range w.Tasks {
			fmt.Fprintf(&b, "- `%s` · %s · %s\n", t.ID, t.Status, t.Description)
		}
	}
	if len(w.Review.Findings) > 0 {
		b.WriteString("\n**Review findings**\n")
		for _, f := range w.Review.Findings {
			fmt.Fprintf(&b, "- `%s` %s (%s): %s", f.ID, f.Severity, f.State, f.Description)
			if len(f.Evidence) > 0 {
				fmt.Fprintf(&b, " — evidence: %s", strings.Join(f.Evidence, ", "))
			}
			b.WriteByte('\n')
		}
	}
	if len(w.Evidence) > 0 {
		b.WriteString("\n**Evidence**\n")
		for _, ev := range w.Evidence {
			fmt.Fprintf(&b, "- `%s` %s · %s · %s\n", ev.ID, ev.Kind, ev.Hash, ev.Path)
		}
	}
	return b.String()
}

func gateAction(w domain.Workflow, pass bool, reason string) store.Action {
	return store.NewAction(w, "gate", gateActionData{Pass: pass, Reason: reason, Key: w.Review.Key, SHA: w.HeadSHA})
}

func prOwnedSummary(w domain.Workflow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow status: %s\n", w.Phase)
	if w.Spec.Approved {
		fmt.Fprintf(&b, "\n## Accepted specification\n%s\n\n%s\n", w.Spec.Title, w.Spec.Description)
	}
	if len(w.Spec.Criteria) > 0 {
		b.WriteString("\n## Acceptance criteria\n")
		for _, c := range w.Spec.Criteria {
			fmt.Fprintf(&b, "- **%s** %s\n", c.ID, c.Description)
		}
	}
	if len(w.Tasks) > 0 {
		b.WriteString("\n## Tasks\n")
		for _, t := range w.Tasks {
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", t.ID, t.Status, t.Description)
		}
	}
	fmt.Fprintf(&b, "\n## Review\nVerdict: %s\n%s\n", w.Review.Verdict, w.Review.Reason)
	if len(w.Review.Findings) > 0 {
		b.WriteString("\n### Findings\n")
		for _, f := range w.Review.Findings {
			fmt.Fprintf(&b, "- **%s · %s · %s** %s", f.ID, f.Severity, f.State, f.Description)
			if len(f.Evidence) > 0 {
				fmt.Fprintf(&b, " (evidence: %s)", strings.Join(f.Evidence, ", "))
			}
			b.WriteByte('\n')
		}
	}
	if len(w.Review.Obligations) > 0 {
		b.WriteString("\n### Obligations\n")
		for _, o := range w.Review.Obligations {
			fmt.Fprintf(&b, "- `%s` %s: %s", o.ID, o.Result, o.Description)
			if len(o.Evidence) > 0 {
				fmt.Fprintf(&b, " (evidence: %s)", strings.Join(o.Evidence, ", "))
			}
			b.WriteByte('\n')
		}
	}
	if len(w.Evidence) > 0 {
		b.WriteString("\n### Evidence metadata\n")
		for _, ev := range w.Evidence {
			fmt.Fprintf(&b, "- `%s` %s · sha256 `%s` · base `%s` · head `%s`\n", ev.ID, ev.Kind, ev.Hash, ev.BaseSHA, ev.HeadSHA)
		}
	}
	return b.String()
}

func prBodyWithOwnedSummary(w domain.Workflow) string {
	return fmt.Sprintf("Implements #%d.\n\n%s\n%s\n%s", w.Issue, gh.OwnedPRBodyStartMarker, prOwnedSummary(w), gh.OwnedPRBodyEndMarker)
}

func outcomeUnknown(err error) bool {
	var unknown *gh.OutcomeUnknownError
	return errors.As(err, &unknown)
}

func (e *Engine) inboxLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		x, err := e.Store.ClaimInbox(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				time.Sleep(250 * time.Millisecond)
			}
			continue
		}
		err = e.handleInbox(ctx, x)
		_ = e.Store.FinishInbox(context.WithoutCancel(ctx), x, err)
	}
}

func (e *Engine) handleInbox(ctx context.Context, x store.Inbox) error {
	var event gh.WebhookEvent
	if err := json.Unmarshal(x.Payload, &event); err != nil {
		return fmt.Errorf("decode GitHub event: %w", err)
	}
	if event.Repository == nil || event.Repository.FullName == "" {
		return nil
	}
	cfg := e.Config()
	repo, ok := cfg.Repository(event.Repository.FullName)
	if !ok || repo.ID == 0 || (event.Repository.ID != 0 && event.Repository.ID != repo.ID) {
		return nil
	}
	if event.Installation == nil || event.Installation.ID <= 0 {
		return nil
	}
	if cfg.GitHub.InstallationID > 0 && cfg.GitHub.InstallationID != event.Installation.ID {
		return nil
	}
	if event.Issue != nil && event.Issue.PullRequest == nil {
		if x.Event == "issues" && event.Action == "opened" {
			return e.createIntake(ctx, repo, event.Installation.ID, *event.Issue, "webhook")
		}
		w, err := e.Store.Get(ctx, domain.ID(repo.ID, event.Issue.Number))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case x.Event == "issues" && event.Action == "edited":
			c, o, r, err := e.client(ctx, w)
			if err != nil {
				return err
			}
			current, err := c.GetIssue(ctx, o, r, w.Issue)
			if err != nil {
				return err
			}
			return e.issueEdited(ctx, w, &current)
		case x.Event == "issues" && (event.Action == "closed" || event.Action == "reopened"):
			return e.reconcileOne(ctx, w)
		case x.Event == "issue_comment" && event.Action == "created" && event.Comment != nil:
			return e.issueComment(ctx, w, event.Comment.ID, event.Comment.User.Login, event.Comment.User.Type, event.Comment.Body)
		default:
			return nil
		}
	}
	if event.PullRequest != nil {
		w, err := e.workflowForPR(ctx, repo.ID, event.PullRequest.Number)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return e.pullRequestEvent(ctx, w, event)
	}
	return nil
}

// createIntake safely converges webhook delivery and periodic discovery. Only
// the caller that inserted the unique repository/issue row emits the initial
// status comment; a racing or redelivered path is a no-op.
func (e *Engine) createIntake(ctx context.Context, repo config.Repository, installationID int64, issue gh.Issue, source string) error {
	if repo.ID <= 0 || installationID <= 0 || issue.Number <= 0 || issue.PullRequest != nil || issue.State == "closed" {
		return nil
	}
	w := domain.Workflow{
		ID: domain.ID(repo.ID, issue.Number), RepositoryID: repo.ID, Repository: repo.Name,
		InstallationID: installationID, Issue: issue.Number, IssueTitle: issue.Title,
		IssueBody: issue.Body, Phase: "INTAKE", Control: "ACTIVE", BaseBranch: repo.BaseBranch,
		Branch: fmt.Sprintf("codex/issue-%d", issue.Number),
	}
	w.Spec.SourceHash = normalizedIssueHash(issue.Body)
	created, err := e.Store.CreateIfAbsent(ctx, w)
	if err != nil || !created {
		return err
	}
	if source == "reconcile_discovery" {
		e.Store.Audit(context.WithoutCancel(ctx), w.ID, "issue_intake_recovered", map[string]any{
			"source": source, "issue_url": issue.HTMLURL, "issue_number": issue.Number,
		})
	}
	return e.Store.Update(ctx, w.ID, func(current *domain.Workflow) ([]store.Action, error) {
		return []store.Action{statusAction(*current)}, nil
	})
}

func normalizedIssueHash(s string) string { return domain.Digest(strings.Join(strings.Fields(s), " ")) }

func (e *Engine) workflowForPR(ctx context.Context, repositoryID int64, number int) (domain.Workflow, error) {
	workflows, err := e.Store.List(ctx)
	if err != nil {
		return domain.Workflow{}, err
	}
	for _, w := range workflows {
		if w.RepositoryID == repositoryID && w.PR == number {
			return w, nil
		}
	}
	return domain.Workflow{}, pgx.ErrNoRows
}

func (e *Engine) issueEdited(ctx context.Context, w domain.Workflow, issue *gh.Issue) error {
	if normalizedIssueHash(w.IssueBody) == normalizedIssueHash(issue.Body) {
		return nil
	}
	e.stop(w.ID)
	return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if normalizedIssueHash(x.IssueBody) == normalizedIssueHash(issue.Body) {
			return nil, store.ErrNoChange
		}
		x.IssueTitle = issue.Title
		x.IssueBody = issue.Body
		x.ProposalSpec = nil
		x.Generation++
		x.Control = "PAUSING"
		x.Discussion = append(x.Discussion, "Issue edit proposal requires maintainer acceptance before the approved specification can change.")
		x.LastError = "Issue changed; review the proposed edit and use /ai accept to start a new triage."
		return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
	})
}

func (e *Engine) issueComment(ctx context.Context, w domain.Workflow, commentID int64, actor, actorType, body string) error {
	trimmed := strings.TrimSpace(body)
	if strings.Contains(trimmed, "<!-- ai-workflow-action-id:") {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(trimmed), "/ai ") && strings.ToLower(trimmed) != "/ai" {
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			x.Discussion = append(x.Discussion, fmt.Sprintf("%s: %s", actor, trimmed))
			return nil, nil
		})
	}
	if strings.EqualFold(actorType, "Bot") {
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			x.Discussion = append(x.Discussion, fmt.Sprintf("%s: %s", actor, trimmed))
			return nil, nil
		})
	}
	ctx = store.WithCommand(ctx, fmt.Sprintf("github:%d:%d", w.RepositoryID, commentID))
	return e.Command(ctx, w.ID, actor, strings.TrimSpace(strings.TrimPrefix(trimmed, "/ai")))
}

func skepticCheckpointReady(w domain.Workflow, currentKey string) bool {
	if w.Phase != "BLOCKED" || w.Review.Verdict == "OBSOLETE" || currentKey == "" || w.Review.Key != currentKey ||
		!w.Review.VerificationPassed || w.Review.SkepticComplete || len(w.Review.Tasks) == 0 || !w.Spec.Approved {
		return false
	}
	if w.Control != "ACTIVE" && w.Control != "PAUSED" {
		return false
	}
	for _, task := range w.Review.Tasks {
		if task.Status != "ACCEPTED" {
			return false
		}
	}
	return true
}

func sameSkepticCheckpoint(current, expected domain.Workflow, currentKey string) bool {
	return current.Version == expected.Version && current.ID == expected.ID &&
		current.RepositoryID == expected.RepositoryID && current.PR == expected.PR &&
		current.Generation == expected.Generation && current.BaseSHA == expected.BaseSHA &&
		current.HeadSHA == expected.HeadSHA && current.Spec.Digest == expected.Spec.Digest &&
		current.Review.ID == expected.Review.ID && current.Review.Key == expected.Review.Key &&
		skepticCheckpointReady(current, currentKey)
}

func (e *Engine) resumeSkepticCheckpoint(ctx context.Context, w domain.Workflow, oldKey string) error {
	if !skepticCheckpointReady(w, oldKey) {
		return errors.New("review checkpoint no longer matches the current candidate")
	}
	e.stop(w.ID)
	next := w
	next.Generation++
	newKey := e.key(ctx, next)
	var reviewerIDs []string
	for _, task := range w.Review.Tasks {
		reviewerIDs = append(reviewerIDs, task.ID)
	}
	err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if !sameSkepticCheckpoint(*x, w, oldKey) {
			return nil, errors.New("review checkpoint changed during resume; retry /ai resume")
		}
		x.Generation++
		x.Control = "ACTIVE"
		x.Phase = "REVIEWING"
		x.Review.ID = fmt.Sprintf("%s-review-%d-%s", x.ID, x.Generation, x.HeadSHA)
		x.Review.Key = newKey
		x.Review.Verdict = "PENDING"
		x.Review.Reason = "resuming skeptic review; accepted reviewer reports are reused"
		x.LastError = ""
		return []store.Action{gateAction(*x, false, x.Review.Reason), statusAction(*x)}, nil
	})
	if err != nil {
		return err
	}
	e.Store.Audit(context.WithoutCancel(ctx), w.ID, "skeptic_checkpoint_resumed", map[string]any{
		"old_key": oldKey, "new_key": newKey, "reused_reviewer_ids": reviewerIDs,
		"generation": next.Generation,
	})
	return nil
}

// Command applies a maintainer command. Pass dashboard=true only after the local
// dashboard has authenticated the caller; local fixture commands are dashboard-only.
func (e *Engine) Command(ctx context.Context, workflowID, actor, command string, dashboard ...bool) error {
	w, err := e.Store.Get(ctx, workflowID)
	if err != nil {
		return err
	}
	if !e.authorized(ctx, w, actor, len(dashboard) > 0 && dashboard[0]) {
		return errors.New("actor is not an authorized repository maintainer")
	}
	cmd := strings.ToLower(strings.TrimSpace(command))
	if strings.HasPrefix(cmd, "/ai ") {
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "/ai "))
	}
	switch cmd {
	case "accept":
		return e.accept(ctx, w, actor)
	case "status":
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			return []store.Action{store.NewAction(*x, "comment", map[string]any{"body": statusBody(*x), "marker": "status"})}, nil
		})
	case "reject":
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Phase != "SPEC_READY" {
				return nil, fmt.Errorf("reject is available when a specification is ready; current phase is %s", x.Phase)
			}
			x.Generation++
			x.Phase = "REJECTED"
			x.Control = "PAUSED"
			x.ProposalSpec = nil
			x.LastError = "specification rejected by maintainer"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		})
	case "defer":
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Phase != "SPEC_READY" && x.Phase != "TRIAGE" && x.Phase != "INTAKE" {
				return nil, fmt.Errorf("defer is unavailable in phase %s", x.Phase)
			}
			x.Generation++
			x.Phase = "DEFERRED"
			x.Control = "PAUSED"
			x.LastError = "deferred by maintainer"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		})
	case "pause", "cancel", "takeover":
		e.stop(w.ID)
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			x.Generation++
			switch cmd {
			case "pause":
				x.Control = "PAUSING"
			case "cancel":
				x.Control = "CANCELLING"
			case "takeover":
				x.Control = "HUMAN_OWNED"
			}
			x.LastError = ""
			return []store.Action{gateAction(*x, false, "workflow control changed"), statusAction(*x)}, nil
		})
	case "resume":
		if err := e.reconcileOne(store.WithoutCommand(ctx), w); err != nil {
			return err
		}
		refreshed, err := e.Store.Get(ctx, w.ID)
		if err != nil {
			return err
		}
		if refreshed.Control == "PAUSING" {
			return errors.New("reconciliation found an external change; review the proposal and accept it before resuming")
		}
		if refreshed.ProposalSpec != nil {
			return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
				x.Generation++
				x.Control = "ACTIVE"
				if x.Phase == "SPEC_READY" || x.Phase == "DEFERRED" {
					x.Phase = "SPEC_READY"
				} else {
					x.Phase = "TRIAGE"
				}
				return []store.Action{statusAction(*x)}, nil
			})
		}
		if refreshed.ProposalSpec == nil && normalizedIssueHash(refreshed.IssueBody) != refreshed.Spec.SourceHash {
			return errors.New("the edited issue requires /ai accept before it can be resumed")
		}
		currentKey := e.key(ctx, refreshed)
		if skepticCheckpointReady(refreshed, currentKey) {
			return e.resumeSkepticCheckpoint(ctx, refreshed, currentKey)
		}
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			x.Generation++
			x.Control = "ACTIVE"
			x.LastError = ""
			if unfinishedDevelopment(x.Tasks) && x.Spec.Approved {
				x.Phase = "DEVELOPING"
			} else if x.PR > 0 && x.Spec.Approved {
				x.Phase = "VERIFYING"
			} else if x.Spec.Approved {
				x.Phase = "PLANNING"
			} else {
				x.Phase = "TRIAGE"
			}
			return []store.Action{statusAction(*x)}, nil
		})
	case "replan":
		e.stop(w.ID)
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			x.Generation++
			x.Control = "ACTIVE"
			x.Phase = "TRIAGE"
			x.ProposalSpec = nil
			x.LastError = "replan requested; previous accepted specification remains active until a replacement is accepted"
			x.Discussion = append(x.Discussion, "Maintainer requested a replan; preserve the accepted specification until a replacement is explicitly accepted.")
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		})
	case "review deep":
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.PR <= 0 {
				return nil, errors.New("deep review requires an existing pull request")
			}
			x.Generation++
			x.Control = "ACTIVE"
			x.Phase = "REVIEWING"
			x.Review.Verdict = "PENDING"
			x.LastError = ""
			return []store.Action{gateAction(*x, false, "deep review started"), statusAction(*x)}, nil
		})
	default:
		return fmt.Errorf("unknown /ai command %q", cmd)
	}
}

func (e *Engine) authorized(ctx context.Context, w domain.Workflow, actor string, dashboard bool) bool {
	if w.Repository == "local/pilot" {
		return dashboard || actor == "local-dashboard"
	}
	if dashboard && actor != "" {
		return true
	}
	r, ok := e.Config().Repository(w.Repository)
	if !ok {
		return false
	}
	listed := false
	for _, m := range r.Maintainers {
		if strings.EqualFold(m, actor) {
			listed = true
			break
		}
	}
	if !listed {
		return false
	}
	c, owner, repo, err := e.client(ctx, w)
	if err != nil {
		return false
	}
	permission, err := c.GetCollaboratorPermission(ctx, owner, repo, actor)
	if err != nil {
		return false
	}
	return permission == "admin" || permission == "maintain"
}

func (e *Engine) accept(ctx context.Context, w domain.Workflow, actor string) error {
	return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Control == "CANCELLING" || x.Control == "CANCELLED" {
			return nil, errors.New("cancelled workflow cannot be accepted")
		}
		if x.ProposalSpec == nil && normalizedIssueHash(x.IssueBody) != x.Spec.SourceHash {
			x.Generation++
			x.Control = "ACTIVE"
			x.Phase = "TRIAGE"
			x.ProposalSpec = nil
			x.LastError = "issue edit accepted for triage; the current approved specification remains active until a proposal is accepted"
			return []store.Action{statusAction(*x)}, nil
		}
		if x.Phase == "INTAKE" {
			x.Control = "ACTIVE"
			x.Phase = "TRIAGE"
			x.AuthorizedBy = actor
			x.LastError = ""
			return []store.Action{statusAction(*x)}, nil
		}
		if x.Phase != "SPEC_READY" {
			return nil, fmt.Errorf("accept is available when a specification is ready; current phase is %s", x.Phase)
		}
		if x.ProposalSpec == nil {
			return nil, errors.New("there is no proposed specification to accept")
		}
		proposal := *x.ProposalSpec
		if proposal.Title == "" || proposal.Description == "" || len(proposal.Criteria) == 0 {
			return nil, errors.New("cannot accept an incomplete specification")
		}
		proposal.Approved = true
		proposal.ApprovedBy = actor
		proposal.SourceHash = normalizedIssueHash(x.IssueBody)
		proposal.Digest = domain.SpecDigest(proposal)
		x.Spec = proposal
		x.ProposalSpec = nil
		x.Generation++
		x.Control = "ACTIVE"
		x.Phase = "PLANNING"
		x.AuthorizedBy = actor
		x.LastError = ""
		return []store.Action{statusAction(*x)}, nil
	})
}

func (e *Engine) pullRequestEvent(ctx context.Context, w domain.Workflow, event gh.WebhookEvent) error {
	c, o, r, err := e.client(ctx, w)
	if err != nil {
		return err
	}
	latest, err := c.GetPullRequest(ctx, o, r, w.PR)
	if err != nil {
		return err
	}
	pr := &latest
	return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
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
			if x.Phase == "BLOCKED" && x.Control == "PAUSED" {
				return nil, store.ErrNoChange
			}
			x.Generation++
			x.Phase = "BLOCKED"
			x.Control = "PAUSED"
			x.LastError = "pull request was closed without a confirmed merge"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if (event.Action == "converted_to_draft" && pr.Draft) || (pr.Draft && x.PRReady && x.Phase != "DEVELOPING") {
			if x.Control == "PAUSING" && !x.PRReady && strings.Contains(x.LastError, "converted to draft") {
				return nil, store.ErrNoChange
			}
			x.Generation++
			x.PRReady = false
			x.Control = "PAUSING"
			x.LastError = "pull request was converted to draft; maintainer review required"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		becameReady := !pr.Draft && !x.PRReady
		if becameReady {
			x.PRReady = true
		}
		if pr.Head.SHA != "" && x.HeadSHA != "" && pr.Head.SHA != x.HeadSHA {
			x.Generation++
			x.HeadSHA = pr.Head.SHA
			x.Control = "PAUSING"
			x.LastError = "pull request head changed outside this workflow; reconcile before continuing"
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
			return []store.Action{gateAction(*x, false, "base branch changed; verification is stale"), statusAction(*x)}, nil
		}
		if becameReady {
			return []store.Action{statusAction(*x)}, nil
		}
		return nil, store.ErrNoChange
	})
}

func (e *Engine) outboxLoop(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a, err := e.Store.ClaimAction(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				time.Sleep(250 * time.Millisecond)
			}
			continue
		}
		if err = e.Store.StartAction(ctx, a); err != nil {
			_ = e.Store.AbandonAction(context.WithoutCancel(ctx), a, err.Error())
			continue
		}
		receipt, effectErr, unknown := e.performAction(ctx, a)
		if finishErr := e.Store.FinishAction(context.WithoutCancel(ctx), a, receipt, effectErr, unknown); finishErr != nil && ctx.Err() == nil {
			e.Store.Audit(context.WithoutCancel(ctx), a.WorkflowID, "outbox_finish_error", map[string]any{"action": a.ID, "error": finishErr.Error()})
		}
	}
}

func (e *Engine) performAction(ctx context.Context, a store.Action) (any, error, bool) {
	w, err := e.Store.Get(ctx, a.WorkflowID)
	if err != nil {
		return nil, err, false
	}
	switch a.Kind {
	case "comment":
		var d commentAction
		if err = json.Unmarshal(a.Data, &d); err != nil {
			return nil, err, false
		}
		if w.Repository == "local/pilot" {
			return map[string]string{"local": "comment recorded"}, nil, false
		}
		c, owner, repo, err := e.client(ctx, w)
		if err != nil {
			return nil, err, false
		}
		marker := domain.Digest([]string{"status", w.ID})[:24]
		r, err := c.UpsertActionComment(ctx, owner, repo, w.Issue, marker, d.Body)
		return r, err, outcomeUnknown(err)
	case "gate":
		var d gateActionData
		if err = json.Unmarshal(a.Data, &d); err != nil {
			return nil, err, false
		}
		if w.Repository == "local/pilot" {
			e.Store.Audit(context.WithoutCancel(ctx), w.ID, "fixture_gate", d)
			return map[string]any{"internal": true, "gate": d}, nil, false
		}
		return e.publishGate(ctx, w, a, d)
	case "publish":
		var d publishActionData
		if err = json.Unmarshal(a.Data, &d); err != nil {
			return nil, err, false
		}
		if w.Repository == "local/pilot" {
			return e.publishFixture(ctx, w, a, d)
		}
		return e.publishGitHub(ctx, w, a, d)
	case "ready":
		return e.markPRReady(ctx, w, a)
	default:
		return nil, fmt.Errorf("unsupported outbox action %q", a.Kind), false
	}
}

func (e *Engine) markPRReady(ctx context.Context, w domain.Workflow, a store.Action) (any, error, bool) {
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return nil, err, false
	}
	if current.Generation != a.Generation || current.Control != "ACTIVE" {
		return nil, store.ErrStale, false
	}
	if current.Repository == "local/pilot" {
		err = e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Generation != a.Generation || x.Control != "ACTIVE" || x.PR != current.PR {
				return nil, store.ErrStale
			}
			x.PRReady = true
			return nil, nil
		})
		if err != nil {
			return map[string]any{"local": true, "ready": true}, err, false
		}
		return map[string]any{"local": true, "ready": true}, nil, false
	}
	if current.PR <= 0 {
		return nil, errors.New("pull request is required before marking ready"), false
	}
	c, owner, repo, err := e.client(ctx, current)
	if err != nil {
		return nil, err, false
	}
	pr, err := c.MarkPullRequestReady(ctx, owner, repo, current.PR)
	if err != nil {
		return nil, err, outcomeUnknown(err)
	}
	receipt := map[string]any{"pr": pr.Number, "draft": pr.Draft, "head_sha": pr.Head.SHA}
	err = e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Generation != a.Generation || x.Control != "ACTIVE" || x.PR != current.PR {
			return nil, store.ErrStale
		}
		x.PRReady = true
		return nil, nil
	})
	if err != nil {
		return receipt, err, false
	}
	return receipt, nil, false
}

func (e *Engine) publishGate(ctx context.Context, w domain.Workflow, a store.Action, d gateActionData) (any, error, bool) {
	// A stale failure may revoke an older green check. A stale success can never
	// touch the current gate because StartAction and this identity check both fence it.
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return nil, err, false
	}
	if d.Pass && (current.Generation != a.Generation || current.Review.Key != d.Key || current.HeadSHA != d.SHA || current.Control != "ACTIVE" || (current.Phase != "READY" && current.Phase != "AWAITING_MAINTAINER")) {
		return nil, store.ErrStale, false
	}
	if current.PR <= 0 || current.HeadSHA == "" {
		return map[string]string{"internal": "no pull request head to gate"}, nil, false
	}
	targetSHA := d.SHA
	if targetSHA == "" {
		targetSHA = current.HeadSHA
	}
	c, owner, repo, err := e.client(ctx, current)
	if err != nil {
		return nil, err, false
	}
	keySummary := d.Reason
	if d.Key != "" {
		keySummary += "\n\nVerification key: " + d.Key
	}
	input := gh.CheckRunInput{Name: "workflow/merge-gate", HeadSHA: targetSHA, Status: "completed", Conclusion: "failure", DetailsURL: fmt.Sprintf("https://github.com/%s/pull/%d", current.Repository, current.PR), Output: gh.CheckOutput{Title: "AI workflow gate", Summary: keySummary}, Gate: true}
	if d.Pass {
		input.Conclusion = "success"
	} else if strings.Contains(strings.ToLower(d.Reason), "in progress") {
		input.Status = "in_progress"
		input.Conclusion = ""
	}
	checks, findErr := c.ListCheckRuns(ctx, owner, repo, targetSHA)
	if findErr != nil {
		return nil, findErr, false
	}
	var prior *gh.CheckRun
	for i := range checks {
		if checks[i].Name == input.Name && checks[i].App.ID == e.Config().GitHub.AppID {
			prior = &checks[i]
			break
		}
	}
	var check gh.CheckRun
	if prior != nil {
		check, err = c.UpdateCheckRun(ctx, owner, repo, prior.ID, input)
	} else {
		check, err = c.CreateCheckRun(ctx, owner, repo, input)
	}
	if err != nil {
		if !outcomeUnknown(err) {
			return nil, err, false
		}
		latest, listErr := c.ListCheckRuns(ctx, owner, repo, targetSHA)
		if listErr != nil {
			return nil, err, true
		}
		for _, candidate := range latest {
			if candidate.Name == input.Name && candidate.App.ID == e.Config().GitHub.AppID {
				if candidate.Status == input.Status && candidate.Conclusion == input.Conclusion && candidate.Output.Summary == input.Output.Summary {
					return candidate, nil, false
				}
				if prior != nil && candidate.ID == prior.ID {
					return nil, err, true
				}
			}
		}
		return nil, err, true
	}
	if targetSHA == current.HeadSHA && current.Generation == a.Generation {
		if d.Pass {
			latest, latestErr := e.Store.Get(ctx, w.ID)
			if latestErr != nil {
				return check, latestErr, false
			}
			if latest.Generation == a.Generation && latest.Control == "ACTIVE" && latest.Review.Key == d.Key && latest.HeadSHA == d.SHA {
				if _, bodyErr := c.UpdateOwnedPRBody(ctx, owner, repo, latest.PR, prOwnedSummary(latest)); bodyErr != nil {
					return check, bodyErr, outcomeUnknown(bodyErr)
				}
			}
		}
		_ = e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Generation != a.Generation || x.Review.Key != d.Key || x.HeadSHA != d.SHA {
				if !d.Pass && x.HeadSHA == targetSHA {
					x.CheckID = check.ID
					x.CheckSHA = targetSHA
					x.GateStatus = check.Status
					x.GateConclusion = check.Conclusion
					x.GateConfirmedAt = time.Now().UTC()
					return nil, nil
				}
				return nil, store.ErrStale
			}
			x.CheckID = check.ID
			x.CheckSHA = targetSHA
			x.GateStatus = check.Status
			x.GateConclusion = check.Conclusion
			x.GateConfirmedAt = time.Now().UTC()
			return nil, nil
		})
	}
	return check, nil, false
}

func (e *Engine) publishGitHub(ctx context.Context, w domain.Workflow, a store.Action, d publishActionData) (any, error, bool) {
	if len(d.Changes) == 0 {
		return nil, errors.New("publish action has no file changes"), false
	}
	if d.ExpectedParent == "" || d.BaseBranch == "" || d.BaseSHA == "" || d.Branch == "" {
		return nil, errors.New("publish action is missing branch identity"), false
	}
	policy := e.repository(w)
	for _, f := range d.Changes {
		if !allowedPath(f.Path, policy, []string{"**"}) {
			return nil, fmt.Errorf("publish path is not allowed: %s", f.Path), false
		}
	}
	c, owner, repo, err := e.client(ctx, w)
	if err != nil {
		return nil, err, false
	}
	baseSHA, err := c.GetBranchSHA(ctx, owner, repo, d.BaseBranch)
	if err != nil {
		return nil, err, false
	}
	if baseSHA != d.BaseSHA {
		return nil, fmt.Errorf("base branch moved: expected %s, found %s", d.BaseSHA, baseSHA), false
	}
	branchSHA, err := c.GetBranchSHA(ctx, owner, repo, d.Branch)
	if err != nil {
		var apiErr *gh.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
			return nil, err, false
		}
		if d.ExpectedParent != d.BaseSHA {
			return nil, errors.New("new publish branch must start at the configured base SHA"), false
		}
		branchSHA, err = c.CreateBranch(ctx, owner, repo, d.Branch, d.BaseSHA)
		if err != nil {
			return nil, err, outcomeUnknown(err)
		}
	}
	marker := "AI Workflow Action: " + a.ID
	var result gh.PublishResult
	if branchSHA != d.ExpectedParent {
		// A previous request may have advanced the branch before its process died.
		// Claim only the exact single-parent commit bearing this action marker and
		// whose resulting tree contains every requested change.
		info, infoErr := c.GetCommitInfo(ctx, owner, repo, branchSHA)
		if infoErr != nil {
			return nil, infoErr, false
		}
		if info.SHA != branchSHA || !strings.Contains(info.Message, marker) || len(info.Parents) != 1 || info.Parents[0] != d.ExpectedParent {
			return nil, fmt.Errorf("publish branch moved: expected %s, found %s", d.ExpectedParent, branchSHA), false
		}
		archive, archiveErr := c.GetArchive(ctx, owner, repo, branchSHA)
		if archiveErr != nil {
			return nil, archiveErr, false
		}
		files, decodeErr := decodeArchive(archive)
		if decodeErr != nil {
			return nil, decodeErr, false
		}
		for _, change := range d.Changes {
			file, exists := files[change.Path]
			if change.Delete {
				if exists {
					return nil, errors.New("published retry commit does not match the requested deletion"), false
				}
				continue
			}
			expectedMode := fs.FileMode(0644)
			if change.Mode == "100755" {
				expectedMode = 0755
			}
			if !exists || !bytes.Equal(file.Data, change.Content) || file.Mode.Perm() != expectedMode.Perm() {
				return nil, fmt.Errorf("published retry commit does not match file %q", change.Path), false
			}
		}
		result = gh.PublishResult{CommitSHA: branchSHA, TreeSHA: info.TreeSHA, Ref: "refs/heads/" + d.Branch}
	} else {
		message := d.Message + "\n\n" + marker
		result, err = c.PublishBranch(ctx, owner, repo, d.Branch, d.ExpectedParent, message, d.Changes)
		if err != nil {
			return result, err, outcomeUnknown(err)
		}
	}
	pr, err := c.ClaimPullRequest(ctx, owner, repo, gh.PullRequestInput{Title: w.IssueTitle, Head: d.Branch, Base: d.BaseBranch, Body: prBodyWithOwnedSummary(w), Draft: true})
	if err != nil {
		return result, err, outcomeUnknown(err)
	}
	receipt := map[string]any{"commit_sha": result.CommitSHA, "pr": pr.Number, "head_sha": pr.Head.SHA, "base_sha": pr.Base.SHA}
	updateErr := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Generation != a.Generation || x.Control != "ACTIVE" {
			return nil, store.ErrStale
		}
		x.HeadSHA = result.CommitSHA
		x.PR = pr.Number
		x.Branch = d.Branch
		for i := range x.Tasks {
			for _, id := range d.TaskIDs {
				if x.Tasks[i].ID == id {
					x.Tasks[i].Status = "INTEGRATED"
					x.Tasks[i].IntegratedSHA = result.CommitSHA
				}
			}
		}
		all := len(x.Tasks) > 0
		for _, t := range x.Tasks {
			if t.Status != "INTEGRATED" && t.Status != "ACCEPTED" {
				all = false
			}
		}
		if all {
			x.Phase = "VERIFYING"
		} else {
			x.Phase = "DEVELOPING"
		}
		return []store.Action{statusAction(*x)}, nil
	})
	if updateErr != nil {
		return receipt, updateErr, false
	}
	return receipt, nil, false
}

func (e *Engine) publishFixture(ctx context.Context, w domain.Workflow, a store.Action, d publishActionData) (any, error, bool) {
	if len(d.Changes) == 0 {
		return nil, errors.New("fixture publish has no changes"), false
	}
	policy := e.repository(w)
	for _, f := range d.Changes {
		if !allowedPath(f.Path, policy, []string{"**"}) {
			return nil, fmt.Errorf("publish path is not allowed: %s", f.Path), false
		}
	}
	root := filepath.Join(e.Config().DataDir, "fixtures", domain.Digest(w.ID)[:24])
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err, false
	}
	snapshot := filepath.Join(root, "snapshot.json")
	files := localFiles()
	var previous []byte
	hadPrevious := false
	if b, err := os.ReadFile(snapshot); err == nil {
		previous = append([]byte(nil), b...)
		hadPrevious = true
		var loaded map[string]sandbox.File
		if json.Unmarshal(b, &loaded) == nil && len(loaded) > 0 {
			files = loaded
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err, false
	}
	for _, f := range d.Changes {
		if f.Delete {
			delete(files, f.Path)
		} else {
			mode := fs.FileMode(0644)
			if f.Mode == "100755" {
				mode = 0755
			}
			files[f.Path] = sandbox.File{Data: append([]byte(nil), f.Content...), Mode: mode}
		}
	}
	sha := contentDigest(files)
	data, err := json.Marshal(files)
	if err != nil {
		return nil, err, false
	}
	tmp := snapshot + ".tmp"
	if err = os.WriteFile(tmp, data, 0600); err != nil {
		return nil, err, false
	}
	if err = os.Rename(tmp, snapshot); err != nil {
		return nil, err, false
	}
	receipt := map[string]any{"head_sha": sha, "snapshot": snapshot, "local": true}
	updateErr := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Generation != a.Generation || x.Control != "ACTIVE" {
			return nil, store.ErrStale
		}
		x.HeadSHA = sha
		x.PR = 0
		x.Branch = "local/fixture"
		for i := range x.Tasks {
			for _, id := range d.TaskIDs {
				if x.Tasks[i].ID == id {
					x.Tasks[i].Status = "INTEGRATED"
					x.Tasks[i].IntegratedSHA = sha
				}
			}
		}
		all := len(x.Tasks) > 0
		for _, t := range x.Tasks {
			if t.Status != "INTEGRATED" && t.Status != "ACCEPTED" {
				all = false
			}
		}
		if all {
			x.Phase = "VERIFYING"
		} else {
			x.Phase = "DEVELOPING"
		}
		return []store.Action{statusAction(*x)}, nil
	})
	if updateErr != nil {
		// The action may finish after a pause or generation change. Keep its
		// receipt, but roll back this local snapshot so the new generation cannot
		// accidentally consume an unadopted result.
		if hadPrevious {
			rollbackTmp := snapshot + ".rollback"
			if writeErr := os.WriteFile(rollbackTmp, previous, 0600); writeErr == nil {
				if renameErr := os.Rename(rollbackTmp, snapshot); renameErr != nil {
					return receipt, fmt.Errorf("%w (snapshot rollback failed: %v)", updateErr, renameErr), false
				}
			} else {
				return receipt, fmt.Errorf("%w (snapshot rollback failed: %v)", updateErr, writeErr), false
			}
		} else if removeErr := os.Remove(snapshot); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return receipt, fmt.Errorf("%w (snapshot rollback failed: %v)", updateErr, removeErr), false
		}
		return receipt, updateErr, false
	}
	return receipt, nil, false
}

func unfinishedDevelopment(tasks []domain.Task) bool {
	for _, task := range tasks {
		if task.Status != "INTEGRATED" && task.Status != "SUPERSEDED" {
			return true
		}
	}
	return false
}
