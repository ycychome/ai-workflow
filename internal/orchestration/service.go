// Package orchestration implements a bounded, fake-agent vertical slice.
package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/artifacts"
	c "github.com/ycychome/ai-workflow/internal/contracts"
	"github.com/ycychome/ai-workflow/internal/execution"
	gh "github.com/ycychome/ai-workflow/internal/github"
	p "github.com/ycychome/ai-workflow/internal/persistence"
	"github.com/ycychome/ai-workflow/internal/policies"
)

const BuilderCommand = `test "$(cat greeting.txt)" = hello; printf 'hello M0\n' > greeting.txt`
const VerifyCommand = policies.FileCommand

var Required = []string{"AC-01", "SEC-01"}

type Service struct {
	VerificationProfile                          string
	Store                                        *p.Store
	GitHub                                       *gh.Client
	Runner                                       *execution.Docker
	Repository, BaseBranch, Label, WorkspaceRoot string
	RepositoryID                                 int64
	MaxAttempts                                  int
}

func (s *Service) key(w c.Workflow) c.VerificationKey {
	profile, _ := policies.Get(s.VerificationProfile)
	return c.VerificationKey{RepositoryID: w.RepositoryID, PullRequest: w.PullRequest, SpecDigest: w.SpecDigest, Generation: w.Generation, BaseSHA: w.BaseSHA, HeadSHA: w.HeadSHA, PolicyDigest: c.Digest(profile.Required), ProfileDigest: c.Digest(profile), RunnerDigest: s.Runner.Image}
}
func workflowID(repo int64, issue int) string { return fmt.Sprintf("r%d-i%d", repo, issue) }

type envelope struct {
	Action string `json:"action"`
	Issue  struct {
		Number int             `json:"number"`
		PR     json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"comment"`
	PR gh.PR `json:"pull_request"`
}

func (s *Service) Handle(ctx context.Context, event p.Event) error {
	var ev envelope
	if e := json.Unmarshal(event.Payload, &ev); e != nil {
		return e
	}
	if event.Kind == "issue_comment" {
		cmd := gh.Command(ev.Action, ev.Comment.User.Type, ev.Comment.Body)
		if cmd == "" {
			return nil
		}
		allowed, e := s.GitHub.Permission(ctx, s.Repository, ev.Comment.User.Login)
		if e != nil {
			return e
		}
		if !allowed {
			return nil
		}
		id := workflowID(s.RepositoryID, ev.Issue.Number)
		if cmd == "status" {
			return s.Store.Update(ctx, id, func(w *c.Workflow) ([]c.Action, error) {
				body := fmt.Sprintf("Phase: %s; control: %s; generation: %d; attempts: %d", w.Phase, w.Control, w.Generation, w.Runs)
				return []c.Action{p.Action(*w, "comment", event.ID, body)}, nil
			})
		}
		return s.Store.ControlOnce(ctx, id, cmd, ev.Comment.ID)
	}
	if event.Kind == "issues" {
		if ev.Issue.Number <= 0 || len(ev.Issue.PR) > 0 {
			return nil
		}
		issue, e := s.GitHub.Issue(ctx, s.Repository, ev.Issue.Number)
		if e != nil {
			return e
		}
		id := workflowID(s.RepositoryID, issue.Number)
		if issue.State != "open" {
			if _, e = s.Store.Get(ctx, id); errors.Is(e, pgx.ErrNoRows) {
				return nil
			}
			return s.Store.Control(ctx, id, "cancel")
		}
		eligible := false
		for _, label := range issue.Labels {
			if label.Name == s.Label {
				eligible = true
			}
		}
		if !eligible {
			return nil
		}
		// The acceptance criterion and patch cannot be expanded by Issue text in M0.
		spec := c.Digest(map[string]any{"issue": issue.Number, "repository_id": s.RepositoryID, "criterion": "hello -> hello M0", "authorization_label": s.Label})
		return s.Store.Create(ctx, c.Workflow{ID: id, Repository: s.Repository, RepositoryID: s.RepositoryID, Issue: issue.Number, SpecDigest: spec, Phase: c.Intake, Control: c.Active, Generation: 1, Version: 1})
	}
	// Facts, including bot-generated pushes and PR changes, trigger reconciliation.
	if event.Kind == "pull_request" || event.Kind == "push" {
		return s.Reconcile(ctx)
	}
	return nil
}
func (s *Service) ProcessInbox(ctx context.Context) error {
	ev, e := s.Store.ClaimEvent(ctx)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	err := s.Handle(ctx, ev)
	if e = s.Store.FinishEvent(ctx, ev, err); e != nil {
		return e
	}
	return err
}
func (s *Service) Work(ctx context.Context) error {
	ids, e := s.Store.ListWork(ctx)
	if e != nil {
		return e
	}
	for _, id := range ids {
		w, e := s.Store.ClaimWork(ctx, id, time.Now(), s.MaxAttempts)
		if errors.Is(e, c.ErrStale) {
			continue
		}
		if e != nil {
			return e
		}
		return s.execute(ctx, w)
	}
	return nil
}
func (s *Service) execute(ctx context.Context, w c.Workflow) error {
	base := w.BaseSHA
	var e error
	if w.Phase == c.Developing {
		base, e = s.GitHub.Head(ctx, w.Repository, s.BaseBranch)
		if e != nil {
			return s.fail(ctx, w, e)
		}
	} else {
		base = w.HeadSHA
	}
	profile, e := policies.Get(s.VerificationProfile)
	if e != nil {
		return s.fail(ctx, w, e)
	}
	files, e := s.GitHub.SnapshotFiles(ctx, w.Repository, base, profile.SnapshotFiles)
	if e != nil {
		return s.fail(ctx, w, e)
	}
	workspace, e := artifacts.New(ctx, s.WorkspaceRoot, files)
	if e != nil {
		return s.fail(ctx, w, e)
	}
	defer workspace.Close()
	command := BuilderCommand
	if w.Phase == c.Reviewing {
		command = profile.Command
	}
	run, e := s.Runner.Start(ctx, execution.Request{ID: w.Attempt.ID, Workspace: workspace.Work, Command: command, Timeout: 45 * time.Second})
	if e != nil {
		return s.fail(ctx, w, e)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Runner.Remove(cleanup, run); err != nil {
			slog.Error("cannot confirm container removal", "attempt", w.Attempt.ID, "error", err)
			return
		}
		s.Store.MarkStopped(cleanup, w.ID, w.Attempt.ID)
		s.Store.ConfirmStop(cleanup, w.ID)
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var result execution.Event
	events := s.Runner.Events(run)
	for events != nil {
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				break
			}
			if event.Kind == "exited" {
				result = event
			}
		case <-tick.C:
			current, e := s.Store.Get(ctx, w.ID)
			if e != nil || current.Accept(w.Attempt, time.Now()) != nil {
				cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				stopErr := s.Runner.Cancel(cancelCtx, run)
				cancel()
				if stopErr != nil {
					return stopErr
				}
				for range events {
				}
				s.Store.MarkStopped(context.Background(), w.ID, w.Attempt.ID)
				s.Store.ConfirmStop(context.Background(), w.ID)
				return c.ErrStale
			}
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			s.Runner.Cancel(cancelCtx, run)
			cancel()
			for range events {
			}
			s.Store.MarkStopped(context.Background(), w.ID, w.Attempt.ID)
			return ctx.Err()
		}
	}
	evidenceDigest, evidencePath, e := artifacts.SaveEvidence(s.WorkspaceRoot, artifacts.RunEvidence{AttemptID: w.Attempt.ID, Generation: w.Generation, Epoch: w.Attempt.Epoch, Command: command, ExitCode: result.ExitCode, Log: result.Log, RunnerImage: s.Runner.Image, TestedCommit: base, TestedFilesDigest: c.Digest(files), RecordedAt: time.Now().UTC()})
	if e != nil {
		return s.fail(ctx, w, e)
	}
	if e = s.Store.RecordEvidence(ctx, w, evidenceDigest, evidencePath); e != nil {
		return e
	}
	if result.ExitCode != 0 {
		return s.fail(ctx, w, fmt.Errorf("Runner exit %d", result.ExitCode))
	}
	if w.Phase == c.Developing {
		candidate, e := workspace.Collect(base)
		if e != nil {
			return s.fail(ctx, w, e)
		}
		if _, e = workspace.Integrate(ctx, candidate); e != nil {
			return s.fail(ctx, w, e)
		}
		return s.Store.Update(ctx, w.ID, func(current *c.Workflow) ([]c.Action, error) {
			if e := current.Accept(w.Attempt, time.Now()); e != nil {
				return nil, e
			}
			current.BaseSHA = base
			current.Attempt.Status = "OUTPUT_READY"
			return []c.Action{p.Action(*current, "candidate", "fixed-patch", candidate)}, nil
		})
	}
	// A Reviewer cannot replace the trusted verification command or modify the candidate.
	for name, before := range files {
		after, e := artifacts.ReadRegular(workspace.Work, name)
		if e != nil || after != before {
			return s.fail(ctx, w, errors.New("review modified candidate"))
		}
	}
	coverage := make([]c.Obligation, 0, len(profile.Required))
	for _, id := range profile.Required {
		coverage = append(coverage, c.Obligation{ID: id, Result: "satisfied", Evidence: evidenceDigest})
	}
	evidence := c.Evidence{Key: w.Key, TestedCommit: w.HeadSHA, MechanicalPassed: true, ProvenanceValid: true, Coverage: coverage}
	verdict, reason := c.Gate(w.Key, w.Control, evidence, profile.Required)
	return s.Store.Update(ctx, w.ID, func(current *c.Workflow) ([]c.Action, error) {
		if e := current.Accept(w.Attempt, time.Now()); e != nil {
			return nil, e
		}
		if current.Key != w.Key {
			return nil, c.ErrStale
		}
		current.Attempt.Status = "ACCEPTED"
		current.Phase = c.AwaitingMaintainer
		current.Reason = reason
		return []c.Action{p.Action(*current, "gate", "review", p.GatePayload{Verdict: verdict, Key: w.Key, Reason: reason, Evidence: &evidence}), p.Action(*current, "comment", "review", fmt.Sprintf("M0 %s: %s. Draft PR #%d; human final merge.", verdict, reason, w.PullRequest))}, nil
	})
}
func (s *Service) fail(ctx context.Context, w c.Workflow, cause error) error {
	return s.Store.Update(ctx, w.ID, func(current *c.Workflow) ([]c.Action, error) {
		if e := current.Accept(w.Attempt, time.Now()); e != nil {
			return nil, e
		}
		current.Phase = c.Blocked
		current.Reason = cause.Error()
		current.Attempt.Status = "BLOCKED"
		return []c.Action{p.Action(*current, "gate", "failed", p.GatePayload{Verdict: c.Fail, Key: current.Key, Reason: current.Reason})}, nil
	})
}
func (s *Service) Publish(ctx context.Context) error {
	a, e := s.Store.ClaimAction(ctx)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	w, e := s.Store.Get(ctx, a.WorkflowID)
	if e != nil {
		return e
	}
	guard := func() error { return s.Store.Guard(ctx, a) }
	var receipt any
	switch a.Kind {
	case "candidate":
		var candidate c.Candidate
		if e = json.Unmarshal(a.Payload, &candidate); e != nil {
			break
		}
		var pr gh.PR
		pr, e = s.GitHub.Publish(ctx, w, a, candidate, s.BaseBranch, guard, func(head string) error { return s.Store.RecordCandidateHead(ctx, a, head) })
		receipt = pr
		if errors.Is(e, c.ErrStale) {
			s.Store.Update(ctx, w.ID, func(current *c.Workflow) ([]c.Action, error) {
				if current.Generation != a.Generation || current.Control != c.Active {
					return nil, c.ErrStale
				}
				current.Phase = c.Blocked
				current.Reason = "remote state changed; publisher refuses to overwrite"
				current.Attempt.Status = "BLOCKED"
				return nil, nil
			})
		}
		if e == nil {
			e = s.Store.Update(ctx, w.ID, func(current *c.Workflow) ([]c.Action, error) {
				if current.Generation != a.Generation || current.Attempt.Epoch != a.Epoch || current.Control != c.Active {
					return nil, c.ErrStale
				}
				current.PullRequest = pr.Number
				current.PRDraft = pr.Draft
				current.HeadSHA = pr.Head.SHA
				current.BaseSHA = pr.Base.SHA
				current.Phase = c.Reviewing
				current.Attempt.Status = "INTEGRATED"
				current.Key = s.key(*current)
				return []c.Action{p.Action(*current, "gate", "pending", p.GatePayload{Verdict: c.Pending, Key: current.Key, Reason: "independent verification pending"})}, nil
			})
			if errors.Is(e, c.ErrStale) {
				e = nil
			}
		}
	case "comment":
		var body string
		if e = json.Unmarshal(a.Payload, &body); e != nil {
			break
		}
		receipt, e = s.GitHub.Comment(ctx, w.Repository, w.Issue, a.ID, body, guard)
	case "gate":
		var payload p.GatePayload
		if e = json.Unmarshal(a.Payload, &payload); e != nil {
			break
		}
		if payload.Verdict == c.Pass {
			if payload.Evidence == nil || payload.Key != w.Key {
				e = c.ErrStale
				break
			}
			profile, err := policies.Get(s.VerificationProfile)
			if err != nil {
				e = err
				break
			}
			if s.key(w) != w.Key {
				e = c.ErrStale
				break
			}
			if v, _ := c.Gate(w.Key, w.Control, *payload.Evidence, profile.Required); v != c.Pass {
				e = c.ErrStale
				break
			}
			var pr gh.PR
			pr, e = s.GitHub.Pull(ctx, w.Repository, w.PullRequest)
			if e != nil {
				break
			}
			var base string
			base, e = s.GitHub.Head(ctx, w.Repository, s.BaseBranch)
			if e != nil {
				break
			}
			if pr.State != "open" || pr.Head.SHA != w.Key.HeadSHA || base != w.Key.BaseSHA {
				e = c.ErrStale
				break
			}
		}
		receipt, e = s.GitHub.Check(ctx, w, payload.Verdict, payload.Key, payload.Reason, guard)
	default:
		e = errors.New("unsupported outbox action")
	}
	finishErr := s.Store.FinishAction(ctx, a, receipt, e)
	if finishErr != nil {
		return finishErr
	}
	return e
}
func (s *Service) Reconcile(ctx context.Context) error {
	// Discover authorized open issues as well as refreshing existing workflows: a
	// lost initial webhook must not permanently hide a task.
	issues, e := s.GitHub.OpenIssues(ctx, s.Repository, s.Label)
	if e != nil {
		return e
	}
	for _, issue := range issues {
		id := workflowID(s.RepositoryID, issue.Number)
		if _, err := s.Store.Get(ctx, id); err == nil {
			continue
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"action": "reconcile", "issue": map[string]any{"number": issue.Number}})
		if err := s.Handle(ctx, p.Event{Kind: "issues", Payload: payload}); err != nil {
			slog.Warn("issue admission deferred", "issue", issue.Number, "error", err)
		}
	}
	all, e := s.Store.All(ctx)
	if e != nil {
		return e
	}
	for _, snapshot := range all {
		if snapshot.Control == c.Pausing {
			// An expired execution lease is not proof that a container has stopped. A running
			// local Worker confirms stop; restart uses Runner.Recover before reconciliation.
			if snapshot.Attempt.Status != "RUNNING" {
				s.Store.ConfirmStop(ctx, snapshot.ID)
			}
			continue
		}
		if snapshot.Control == c.Cancelled || snapshot.Control == c.HumanOwned || snapshot.Phase == c.Merged {
			continue
		}
		var pr gh.PR
		if snapshot.PullRequest > 0 {
			pr, e = s.GitHub.Pull(ctx, snapshot.Repository, snapshot.PullRequest)
			if e != nil {
				return e
			}
		}
		issue, err := s.GitHub.Issue(ctx, snapshot.Repository, snapshot.Issue)
		if err != nil {
			return err
		}
		if issue.State != "open" && !pr.Merged {
			if err = s.Store.Control(ctx, snapshot.ID, "cancel"); err != nil {
				return err
			}
			continue
		}
		base, e := s.GitHub.Head(ctx, snapshot.Repository, s.BaseBranch)
		if e != nil {
			return e
		}
		e = s.Store.Update(ctx, snapshot.ID, func(w *c.Workflow) ([]c.Action, error) {
			if w.Generation != snapshot.Generation || w.Version != snapshot.Version {
				return nil, c.ErrStale
			}
			if w.PullRequest > 0 && pr.Merged {
				w.Phase = c.Merged
				w.Control = c.Cancelled
				w.Generation++
				return nil, nil
			}
			if w.PullRequest > 0 && pr.State != "open" {
				w.Phase = c.Blocked
				w.Control = c.Pausing
				w.Generation++
				w.Reason = "PR closed without merge"
				return []c.Action{p.Action(*w, "gate", "closed", p.GatePayload{Verdict: c.Fail, Key: w.Key, Reason: w.Reason})}, nil
			}
			if w.PullRequest > 0 && pr.Draft && !w.PRDraft {
				w.PRDraft = true
				w.Control = c.Pausing
				w.Generation++
				w.Reason = "PR converted to draft"
				return []c.Action{p.Action(*w, "gate", "draft", p.GatePayload{Verdict: c.Fail, Key: w.Key, Reason: w.Reason})}, nil
			}
			w.PRDraft = pr.Draft
			if w.Control == c.Reconciling {
				w.Control = c.Active
				w.Attempt.Status = "SUPERSEDED"
				if w.PullRequest == 0 {
					w.Phase = c.Intake
				} else {
					w.Phase = c.Reviewing
				}
				w.Reason = "resumed after current GitHub snapshot"
			}
			changed := w.PullRequest > 0 && (w.HeadSHA != pr.Head.SHA || w.BaseSHA != base || w.Key != s.key(*w))
			if changed {
				w.Generation++
				w.BaseSHA = base
				w.HeadSHA = pr.Head.SHA
				w.Attempt.Status = "SUPERSEDED"
				w.Phase = c.Reviewing
				w.Reason = "candidate or base changed"
			}
			if w.PullRequest > 0 && (changed || snapshot.Control == c.Reconciling) {
				w.Key = s.key(*w)
				return []c.Action{p.Action(*w, "gate", "reconcile", p.GatePayload{Verdict: c.Fail, Key: w.Key, Reason: "new snapshot requires verification"})}, nil
			}
			return nil, nil
		})
		if e != nil && !errors.Is(e, c.ErrStale) {
			return e
		}
	}
	return nil
}
func (s *Service) Run(ctx context.Context) func() {
	var wg sync.WaitGroup
	loop := func(name string, delay time.Duration, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(delay)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if e := fn(ctx); e != nil && !errors.Is(e, c.ErrStale) && !errors.Is(e, context.Canceled) {
						slog.Warn("control loop", "component", name, "error", e)
					}
				}
			}
		}()
	}
	loop("inbox", 200*time.Millisecond, s.ProcessInbox)
	loop("worker", 200*time.Millisecond, s.Work)
	loop("publisher", 200*time.Millisecond, s.Publish)
	loop("reconciliation", 5*time.Second, s.Reconcile)
	return wg.Wait
}
func (s *Service) Validate() error {
	if _, e := policies.Get(s.VerificationProfile); e != nil {
		return e
	}
	if !strings.Contains(s.Repository, "/") || s.RepositoryID <= 0 || s.MaxAttempts < 2 || s.Label == "" || s.BaseBranch == "" {
		return errors.New("incomplete M0 configuration")
	}
	return nil
}
