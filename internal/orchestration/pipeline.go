package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
	"os"
	"sort"
	"strings"
	"sync"
)

func validateDevPlanReport(output json.RawMessage) error {
	var report struct {
		Tasks []domain.Task `json:"tasks"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		return fmt.Errorf("decode development plan: %w", err)
	}
	return validatePlan(report.Tasks)
}

func validateReviewPlanReport(output json.RawMessage) error {
	var report struct {
		Tasks []domain.Task `json:"tasks"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		return fmt.Errorf("decode review plan: %w", err)
	}
	if err := validatePlan(report.Tasks); err != nil {
		return err
	}
	for _, task := range report.Tasks {
		if task.Kind != "verification" {
			return fmt.Errorf("review task %s must have kind verification", task.ID)
		}
	}
	return nil
}

func (e *Engine) triage(ctx context.Context, w *domain.Workflow) error {
	var report struct {
		Decision, Title, Description, Risk string
		Criteria                           []domain.Criterion `json:"criteria"`
		NonGoals                           []string           `json:"non_goals"`
		Questions                          []string           `json:"questions"`
	}
	if e.Config().Runner.Mode == "fake" {
		if !strings.HasPrefix(w.IssueTitle, "Fixture:") {
			return errors.New("Fake runner only accepts the documented Fixture: repair Add task")
		}
		report.Decision = "accepted"
		report.Title = w.IssueTitle
		report.Description = w.IssueBody
		report.Risk = "low"
		report.Criteria = []domain.Criterion{{ID: "AC-01", Description: "Add returns the sum of two integers and existing tests pass"}}
	} else {
		session, cleanup, err := e.plannerSession(ctx, w, "triage")
		if err != nil {
			return err
		}
		defer cleanup()
		out, err := e.ask(ctx, *w, triagePrompt, string(raw(map[string]any{"title": w.IssueTitle, "body": w.IssueBody, "discussion": w.Discussion, "previous_spec": w.Spec})), session.readTools(), triageSchema())
		w.Evidence = append(w.Evidence, session.listEvidence()...)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(out, &report); err != nil {
			return err
		}
	}
	proposed := domain.Spec{Revision: w.Spec.Revision + 1, Title: report.Title, Description: report.Description, Criteria: report.Criteria, NonGoals: report.NonGoals, Risk: report.Risk, SourceHash: normalizedIssueHash(w.IssueBody)}
	if len(report.Criteria) == 0 && report.Decision == "accepted" {
		return errors.New("triage omitted acceptance criteria")
	}
	seen := map[string]bool{}
	for _, c := range report.Criteria {
		if c.ID == "" || c.Description == "" || seen[c.ID] {
			return errors.New("triage acceptance criteria invalid")
		}
		seen[c.ID] = true
	}
	proposed.Digest = domain.SpecDigest(proposed)
	w.ProposalSpec = &proposed
	switch report.Decision {
	case "accepted":
		w.Phase = "SPEC_READY"
		w.LastError = "Review the proposed TaskSpec and post /ai accept to approve it"
		if e.Config().Runner.Mode == "fake" {
			w.Spec = proposed
			w.ProposalSpec = nil
			w.Spec.Approved = true
			w.Spec.ApprovedBy = w.AuthorizedBy
			w.Spec.Digest = domain.SpecDigest(w.Spec)
			w.Phase = "PLANNING"
			w.LastError = ""
		}
	case "needs_info":
		w.Phase = "NEEDS_INFO"
		w.LastError = strings.Join(report.Questions, "\n")
	case "rejected":
		w.Phase = "REJECT_PROPOSED"
	case "deferred":
		w.Phase = "DEFERRED"
	default:
		return errors.New("unsupported triage decision")
	}
	return e.Store.Save(ctx, w, statusAction(*w))
}
func (e *Engine) plan(ctx context.Context, w *domain.Workflow) error {
	if !w.Spec.Approved {
		return errors.New("TaskSpec approval is required")
	}
	var report struct {
		Tasks []domain.Task `json:"tasks"`
	}
	if e.Config().Runner.Mode == "fake" {
		report.Tasks = []domain.Task{{ID: "repair-add", Kind: "code", Description: "Fix Add to return a + b", Paths: []string{"calculator.go"}}}
	} else {
		session, cleanup, err := e.plannerSession(ctx, w, "dev-plan")
		if err != nil {
			return err
		}
		defer cleanup()
		out, err := e.ask(ctx, *w, devPlanPrompt, string(raw(map[string]any{"spec": w.Spec, "findings": w.Review.Findings, "base_sha": w.BaseSHA, "head_sha": w.HeadSHA})), session.readTools(), planSchema(), validateDevPlanReport)
		w.Evidence = append(w.Evidence, session.listEvidence()...)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(out, &report); err != nil {
			return err
		}
	}
	if err := validatePlan(report.Tasks); err != nil {
		return err
	}
	for i := range report.Tasks {
		report.Tasks[i].Status = "QUEUED"
	}
	w.Tasks = report.Tasks
	w.Phase = "DEVELOPING"
	w.LastError = ""
	if w.BaseSHA == "" && w.Repository != "local/pilot" {
		c, o, r, err := e.client(ctx, *w)
		if err != nil {
			return err
		}
		sha, err := c.GetBranchSHA(ctx, o, r, w.BaseBranch)
		if err != nil {
			return err
		}
		w.BaseSHA = sha
	}
	if w.HeadSHA == "" {
		w.HeadSHA = w.BaseSHA
	}
	return e.Store.Save(ctx, w, statusAction(*w))
}

type buildResult struct {
	index    int
	changes  []sandbox.Change
	report   json.RawMessage
	evidence []domain.Evidence
	err      error
}

func (e *Engine) build(ctx context.Context, w *domain.Workflow) error {
	cfg := e.Config()
	ready := readyTasks(w.Tasks, cfg.Runner.Builders, false)
	if len(ready) == 0 {
		all := true
		for _, t := range w.Tasks {
			if t.Status != "INTEGRATED" && t.Status != "SUPERSEDED" {
				all = false
			}
		}
		if !all {
			return errors.New("development DAG has no runnable tasks")
		}
		w.Phase = "VERIFYING"
		return e.Store.Save(ctx, w, statusAction(*w))
	}
	files, err := e.source(ctx, *w, w.HeadSHA)
	if err != nil {
		return err
	}
	for _, idx := range ready {
		t := &w.Tasks[idx]
		if t.Attempts >= cfg.Budget.MaxAttempts {
			return fmt.Errorf("task %s attempts exhausted", t.ID)
		}
		t.Attempts++
		t.LeaseEpoch++
		t.Status = "RUNNING"
	}
	if err = e.Store.Save(ctx, w, statusAction(*w)); err != nil {
		return err
	}
	results := make(chan buildResult, len(ready))
	snapshot := *w
	for _, idx := range ready {
		go func(i int) { results <- e.buildTask(ctx, snapshot, i, files) }(idx)
	}
	var changes []gh.FileChange
	paths := map[string]bool{}
	ids := []string{}
	failed := false
	var reason string
	for range ready {
		res := <-results
		t := &w.Tasks[res.index]
		w.Evidence = append(w.Evidence, res.evidence...)
		t.Report = res.report
		if res.err != nil {
			t.Status = "RETRY_WAIT"
			reason = res.err.Error()
			failed = true
			continue
		}
		if taskKind(*t) != "code" {
			if len(res.changes) != 0 {
				t.Status = "RETRY_WAIT"
				failed = true
				reason = "read-only task modified the source snapshot"
				continue
			}
			t.Status = "INTEGRATED"
			t.IntegratedSHA = w.HeadSHA
			continue
		}
		if len(res.changes) == 0 {
			var report struct {
				Rebuttals []struct {
					FindingID string   `json:"finding_id"`
					Argument  string   `json:"argument"`
					Evidence  []string `json:"evidence"`
				} `json:"rebuttals"`
			}
			_ = json.Unmarshal(res.report, &report)
			if len(report.Rebuttals) > 0 {
				valid := true
				knownEvidence := map[string]bool{}
				for _, ev := range res.evidence {
					knownEvidence[ev.ID] = true
				}
				for _, rb := range report.Rebuttals {
					found := false
					for _, f := range w.Review.Findings {
						if f.ID == rb.FindingID {
							found = true
						}
					}
					if !found || rb.Argument == "" || len(rb.Evidence) == 0 {
						valid = false
					}
					for _, id := range rb.Evidence {
						if !knownEvidence[id] {
							valid = false
						}
					}
				}
				if !valid {
					t.Status = "RETRY_WAIT"
					failed = true
					reason = "rebuttal lacks a known finding or independently recorded evidence"
					continue
				}
				for _, rb := range report.Rebuttals {
					for i := range w.Review.Findings {
						f := &w.Review.Findings[i]
						if f.ID == rb.FindingID {
							f.History = append(f.History, "Dev rebuttal: "+rb.Argument)
							f.State = "DISPUTED"
						}
					}
				}
				t.Status = "INTEGRATED"
				t.IntegratedSHA = w.HeadSHA
				continue
			}
			t.Status = "RETRY_WAIT"
			failed = true
			reason = "builder returned no actual source artifact"
			continue
		}
		for _, change := range res.changes {
			if paths[change.Path] {
				return fmt.Errorf("actual diff conflict at %s; replan required", change.Path)
			}
			paths[change.Path] = true
			mode := "100644"
			if change.Mode&0111 != 0 {
				mode = "100755"
			}
			changes = append(changes, gh.FileChange{Path: change.Path, Content: change.Data, Delete: change.Deleted, Mode: mode})
		}
		t.Status = "VALIDATED"
		ids = append(ids, t.ID)
	}
	if failed {
		w.LastError = reason
		for _, t := range w.Tasks {
			if t.Status == "RETRY_WAIT" && t.Attempts >= cfg.Budget.MaxAttempts {
				return fmt.Errorf("task attempts exhausted: %s", reason)
			}
		}
	}
	if len(changes) == 0 {
		return e.Store.Save(ctx, w, statusAction(*w))
	}
	artifact, err := e.artifact(ctx, *w, fmt.Sprintf("integration:%d:%d", w.Generation, w.Version), "VALIDATED_ARTIFACT", changes)
	if err != nil {
		return err
	}
	w.Evidence = append(w.Evidence, artifact)
	payload := map[string]any{"changes": changes, "task_ids": ids, "message": fmt.Sprintf("Implement issue #%d tasks %s\n\nai-workflow action %s/%d", w.Issue, strings.Join(ids, ","), w.ID, w.Generation), "expected_parent": w.HeadSHA, "base_branch": w.BaseBranch, "base_sha": w.BaseSHA, "branch": w.Branch}
	return e.Store.Save(ctx, w, store.NewAction(*w, "publish", payload), statusAction(*w))
}
func (e *Engine) buildTask(ctx context.Context, w domain.Workflow, idx int, files map[string]sandbox.File) buildResult {
	task := w.Tasks[idx]
	kind := taskKind(task)
	res := buildResult{index: idx}
	id := fmt.Sprintf("%s-g%d-%s-e%d", w.ID, w.Generation, task.ID, task.LeaseEpoch)
	ctx, cancel := context.WithTimeout(ctx, e.Config().AttemptTimeout())
	defer cancel()
	_ = e.Store.AttemptOwned(ctx, id, w.ID, task.ID, w.Generation, task.LeaseEpoch, w.LeaseOwner, "RUNNING", nil, nil)
	workspace, err := e.Sandbox.Create(ctx, id, files)
	if err != nil {
		res.err = err
		return res
	}
	defer e.Sandbox.Destroy(context.Background(), workspace)
	session := &toolSession{e: e, w: w, workspace: workspace, scope: task.Paths}
	if kind != "code" {
		session.baseFiles, res.err = e.source(ctx, w, w.BaseSHA)
		if res.err != nil {
			return res
		}
	}
	if e.Config().Runner.Mode == "fake" {
		original := files["calculator.go"]
		original.Data = []byte(strings.Replace(string(original.Data), "return a - b", "return a + b", 1))
		res.err = e.Sandbox.WriteFile(ctx, workspace, "calculator.go", original)
		res.report = raw(map[string]any{"summary": "Fixed Add in the deterministic fixture", "rebuttals": []any{}})
	} else {
		tools := session.tools()
		schema := builderSchema()
		if kind != "code" {
			if kind == "verification" {
				tools = session.verificationTools()
			} else {
				tools = session.readTools()
			}
			schema = nonCodeBuilderSchema()
		}
		res.report, res.err = e.ask(ctx, w, builderPrompt, string(raw(map[string]any{"spec": w.Spec, "task": task, "integrated_sha": w.HeadSHA, "findings": w.Review.Findings, "dependencies": dependencyReports(w, task)})), tools, schema)
	}
	res.evidence = session.listEvidence()
	if res.err == nil {
		res.changes, res.err = e.Sandbox.CollectChanges(ctx, workspace)
	}
	if res.err == nil && kind != "code" {
		if len(res.changes) != 0 {
			res.err = errors.New("read-only task modified its workspace")
		} else {
			var report struct {
				Summary  string   `json:"summary"`
				Result   string   `json:"result"`
				Evidence []string `json:"evidence"`
			}
			if err := json.Unmarshal(res.report, &report); err != nil || strings.TrimSpace(report.Summary) == "" {
				res.err = errors.New("read-only task report is invalid")
			} else if report.Result != "satisfied" && report.Result != "failed" && report.Result != "unverified" {
				res.err = errors.New("read-only task result must be satisfied, failed or unverified")
			} else {
				known := map[string]domain.Evidence{}
				for _, ev := range session.listEvidence() {
					known[ev.ID] = ev
				}
				if len(report.Evidence) == 0 {
					res.err = errors.New("read-only task report must cite evidence from this run")
				}
				hasSourceEvidence, hasSuccessfulCheck := false, false
				for _, id := range report.Evidence {
					ev, ok := known[id]
					if !ok {
						res.err = fmt.Errorf("read-only task cites evidence not produced by this run: %s", id)
						break
					}
					switch ev.Kind {
					case "SOURCE_ANALYSIS":
						hasSourceEvidence = true
					case "TOOL_VERIFIED":
						if ev.ExitCode != 0 {
							res.err = fmt.Errorf("verification report cites unsuccessful tool evidence: %s", id)
							break
						}
						hasSuccessfulCheck = true
					default:
						res.err = fmt.Errorf("read-only task cites unsupported evidence kind %s", ev.Kind)
					}
					if res.err != nil {
						break
					}
				}
				if res.err == nil && kind == "analysis" && !hasSourceEvidence {
					res.err = errors.New("analysis task must cite actual source-analysis evidence")
				}
				if res.err == nil && report.Result != "satisfied" {
					res.err = errors.New("non-code task did not report satisfied")
				}
				if res.err == nil && kind == "verification" && !hasSuccessfulCheck {
					res.err = errors.New("verification task must cite a successful tool check from this run")
				}
				if res.err == nil {
					ev, err := e.artifact(context.WithoutCancel(ctx), w, id+":non-code", "NON_CODE_ARTIFACT", map[string]any{"task_id": task.ID, "kind": kind, "head_sha": w.HeadSHA, "report": report, "evidence_ids": report.Evidence})
					if err != nil {
						res.err = err
					} else {
						res.evidence = append(res.evidence, ev)
					}
				}
			}
		}
	}
	if res.err == nil {
		for _, c := range res.changes {
			if !allowedPath(c.Path, e.repository(w), task.Paths) {
				res.err = fmt.Errorf("actual artifact violates scope: %s", c.Path)
				break
			}
		}
	}
	if res.err == nil && len(res.changes) > 0 {
		ev, err := e.artifact(context.WithoutCancel(ctx), w, id+":source", "SOURCE_ARTIFACT", map[string]any{"generation": w.Generation, "lease_epoch": task.LeaseEpoch, "base_sha": w.HeadSHA, "changes": res.changes})
		if err != nil {
			res.err = err
		} else {
			res.evidence = append(res.evidence, ev)
		}
	}
	state := "OUTPUT_READY"
	if res.err != nil {
		state = "BLOCKED"
	}
	_ = e.Store.AttemptOwned(context.WithoutCancel(ctx), id, w.ID, task.ID, w.Generation, task.LeaseEpoch, w.LeaseOwner, state, res.report, res.evidence)
	return res
}
func (e *Engine) verify(ctx context.Context, w *domain.Workflow) error {
	files, pending, err := e.verificationSnapshot(ctx, w)
	if err != nil {
		return err
	}
	if pending {
		w.LastError = "GitHub is computing the current test merge commit"
		return e.Store.Save(ctx, w, statusAction(*w))
	}
	workspace, err := e.Sandbox.Create(ctx, fmt.Sprintf("%s-g%d-verify", w.ID, w.Generation), files)
	if err != nil {
		return err
	}
	defer e.Sandbox.Destroy(context.Background(), workspace)
	r := e.repository(*w)
	previousFindings := append([]domain.Finding(nil), w.Review.Findings...)
	for n, command := range r.Verification {
		result, runErr := e.Sandbox.Run(ctx, workspace, command, e.Config().AttemptTimeout())
		result.Stdout = e.redact(result.Stdout)
		result.Stderr = e.redact(result.Stderr)
		code := result.ExitCode
		if runErr != nil {
			code = -1
		}
		ev, err := e.artifact(context.WithoutCancel(ctx), *w, fmt.Sprintf("verify:g%d:%d:%s", w.Generation, n, w.HeadSHA), "TRUSTED_CHECK", result)
		if err != nil {
			return err
		}
		ev.Command = command
		ev.ExitCode = code
		ev.TestedCommitSHA = w.TestedSHA
		ev.TestedTreeSHA = w.TestedTreeSHA
		w.Evidence = append(w.Evidence, ev)
		if code == 0 {
			for i := range previousFindings {
				f := &previousFindings[i]
				if f.ID == "mechanical-"+fmt.Sprint(n) {
					f.State = "RESOLVED"
					f.Evidence = []string{ev.ID}
					f.VerificationKey = e.key(ctx, *w)
					f.History = append(f.History, "Trusted check passed on "+w.TestedSHA)
				}
			}
		}
		if code != 0 {
			if runErr != nil || result.TimedOut || strings.Contains(result.Stdout+result.Stderr, "module lookup disabled by GOPROXY=off") {
				return errors.New("verification infrastructure did not complete; repair or warm the runner image before resuming")
			}
			w.Review = domain.Review{Key: e.key(ctx, *w), Verdict: "CHANGES_REQUESTED", Reason: "trusted verification failed"}
			w.LastError = "trusted verification failed: " + strings.Join(command, " ") + "\n" + result.Stdout + result.Stderr
			if w.FixRounds >= e.Config().Budget.MaxFixRounds {
				return errors.New(w.LastError)
			}
			w.FixRounds++
			w.Review.Findings = append(previousFindings, domain.Finding{ID: "mechanical-" + fmt.Sprint(n), Severity: "high", Category: "test", Description: w.LastError, Expected: "Trusted command exits with status 0", Actual: fmt.Sprintf("Command exited with status %d", code), Preconditions: "Current candidate under the trusted verification profile", EvidenceType: "TOOL_VERIFIED", Evidence: []string{ev.ID}, State: "CONFIRMED", VerificationKey: w.Review.Key})
			w.Phase = "CHANGES_REQUESTED"
			return e.Store.Save(ctx, w, gateAction(*w, false, w.LastError), statusAction(*w))
		}
	}
	key := e.key(ctx, *w)
	for i := range previousFindings {
		f := &previousFindings[i]
		if f.VerificationKey != key {
			f.History = append(f.History, "Reverification required after snapshot change: "+f.VerificationKey)
			f.VerificationKey = key
			f.State = "FIX_CANDIDATE"
			f.Evidence = nil
			f.EvidenceType = "MODEL_CLAIM"
		}
	}
	w.Review = domain.Review{ID: fmt.Sprintf("%s-review-%d-%s", w.ID, w.Generation, w.HeadSHA), Key: key, Findings: previousFindings, VerificationPassed: true, Verdict: "RUNNING"}
	w.Phase = "REVIEWING"
	w.LastError = ""
	return e.Store.Save(ctx, w, store.NewAction(*w, "ready", map[string]any{}), gateAction(*w, false, "independent review is running"), statusAction(*w))
}
func mandatory(w domain.Workflow) []domain.Obligation {
	var out []domain.Obligation
	for _, c := range w.Spec.Criteria {
		out = append(out, domain.Obligation{ID: c.ID, CriterionID: c.ID, Description: c.Description, Result: "unverified", Evidence: []string{}})
	}
	for _, x := range [][2]string{{"SEC-01", "Permission and trust boundaries preserved"}, {"TEST-01", "Trusted tests and test modifications reviewed"}, {"COMP-01", "Affected public interfaces remain compatible"}} {
		out = append(out, domain.Obligation{ID: x[0], Description: x[1], Result: "unverified", Evidence: []string{}})
	}
	return out
}
func (e *Engine) review(ctx context.Context, w *domain.Workflow) error {
	key := e.key(ctx, *w)
	if w.Review.Key != key || !w.Review.VerificationPassed {
		w.Phase = "VERIFYING"
		return e.Store.Save(ctx, w, gateAction(*w, false, "review snapshot changed"))
	}
	files, err := e.source(ctx, *w, w.HeadSHA)
	if err != nil {
		return err
	}
	baseline := mandatory(*w)
	if len(w.Review.Tasks) == 0 {
		if e.Config().Runner.Mode == "fake" {
			w.Review.Obligations = baseline
			for i := 0; i < e.Config().Runner.Reviewers; i++ {
				t := domain.Task{ID: fmt.Sprintf("review-%d", i+1), Kind: "verification", Description: "Independent deterministic fixture review", Paths: []string{"**"}, Status: "QUEUED"}
				for n, o := range baseline {
					if n%e.Config().Runner.Reviewers == i {
						t.ObligationIDs = append(t.ObligationIDs, o.ID)
					}
				}
				w.Review.Tasks = append(w.Review.Tasks, t)
			}
		} else {
			workspace, err := e.Sandbox.Create(ctx, w.ID+"-review-plan", files)
			if err != nil {
				return err
			}
			baseFiles, err := e.baseSnapshot(ctx, *w)
			if err != nil {
				e.Sandbox.Destroy(context.Background(), workspace)
				return err
			}
			session := &toolSession{e: e, w: *w, workspace: workspace, review: true, baseFiles: baseFiles}
			output, askErr := e.ask(ctx, *w, reviewPlanPrompt, string(raw(map[string]any{"spec": w.Spec, "mandatory_obligations": baseline, "head_sha": w.HeadSHA})), session.tools(), reviewPlanSchema(), validateReviewPlanReport)
			e.Sandbox.Destroy(context.Background(), workspace)
			w.Evidence = append(w.Evidence, session.listEvidence()...)
			if askErr != nil {
				return askErr
			}
			var plan struct {
				Tasks       []domain.Task       `json:"tasks"`
				Obligations []domain.Obligation `json:"obligations"`
			}
			if err = json.Unmarshal(output, &plan); err != nil {
				return err
			}
			if err = validatePlan(plan.Tasks); err != nil {
				return err
			}
			for _, task := range plan.Tasks {
				if task.Kind != "verification" {
					return fmt.Errorf("review task %s must have kind verification", task.ID)
				}
			}
			w.Review.Tasks = plan.Tasks
			w.Review.Obligations = baseline
			for i := range w.Review.Tasks {
				if w.Review.Tasks[i].Kind == "" {
					w.Review.Tasks[i].Kind = "verification"
				}
				w.Review.Tasks[i].Status = "QUEUED"
			}
			known := map[string]bool{}
			for _, o := range baseline {
				known[o.ID] = true
			}
			for _, o := range plan.Obligations {
				if !known[o.ID] {
					o.Result = "unverified"
					o.Evidence = nil
					w.Review.Obligations = append(w.Review.Obligations, o)
					known[o.ID] = true
				}
			}
			assigned := map[string]bool{}
			for _, t := range w.Review.Tasks {
				for _, id := range t.ObligationIDs {
					if !known[id] {
						return errors.New("review plan references unknown obligation")
					}
					assigned[id] = true
				}
			}
			for id := range known {
				if !assigned[id] {
					return fmt.Errorf("mandatory review obligation %s unassigned", id)
				}
			}
		}
		if err = e.Store.Save(ctx, w, statusAction(*w)); err != nil {
			return err
		}
	}
	for {
		ready := readyTasks(w.Review.Tasks, e.Config().Runner.Reviewers, true)
		if len(ready) == 0 {
			all := true
			for _, t := range w.Review.Tasks {
				if t.Status != "ACCEPTED" {
					all = false
				}
			}
			if !all {
				return errors.New("review DAG has no runnable task")
			}
			break
		}
		var mu sync.Mutex
		type rr struct {
			idx      int
			report   reviewReport
			evidence []domain.Evidence
			err      error
		}
		ch := make(chan rr, len(ready))
		for _, idx := range ready {
			t := &w.Review.Tasks[idx]
			if t.Attempts >= e.Config().Budget.MaxAttempts {
				return errors.New("review task attempts exhausted")
			}
			t.Attempts++
			t.LeaseEpoch++
			t.Status = "RUNNING"
		}
		if err = e.Store.Save(ctx, w, statusAction(*w)); err != nil {
			return err
		}
		snapshot := *w
		for _, idx := range ready {
			go func(i int) {
				report, ev, err := e.reviewTask(ctx, snapshot, snapshot.Review.Tasks[i], files, false)
				mu.Lock()
				ch <- rr{i, report, ev, err}
				mu.Unlock()
			}(idx)
		}
		for range ready {
			r := <-ch
			t := &w.Review.Tasks[r.idx]
			w.Evidence = append(w.Evidence, r.evidence...)
			if r.err != nil {
				t.Status = "BLOCKED"
				return r.err
			}
			t.Report = raw(r.report)
			if err = mergeReport(w, r.report, t.ObligationIDs, r.evidence, false); err != nil {
				return err
			}
			t.Status = "ACCEPTED"
		}
		if err = e.Store.Save(ctx, w, statusAction(*w)); err != nil {
			return err
		}
	}
	if !w.Review.SkepticComplete {
		task := domain.Task{ID: "skeptic", Kind: "verification", Description: "Challenge findings, counterevidence and omitted coverage", Paths: []string{"**"}}
		for _, o := range w.Review.Obligations {
			task.ObligationIDs = append(task.ObligationIDs, o.ID)
		}
		report, ev, err := e.reviewTask(ctx, *w, task, files, true)
		w.Evidence = append(w.Evidence, ev...)
		if err != nil {
			return err
		}
		if err = mergeReport(w, report, task.ObligationIDs, ev, true); err != nil {
			return err
		}
		w.Review.SkepticComplete = true
	}
	pass, reason := domain.Gate(*w, e.key(ctx, *w))
	w.Review.Reason = reason
	if pass {
		w.Review.Verdict = "PASS"
		w.Phase = "AWAITING_MAINTAINER"
		w.LastError = ""
	} else {
		w.Review.Verdict = "CHANGES_REQUESTED"
		w.LastError = reason
		blocking := false
		for _, f := range w.Review.Findings {
			if f.State != "RESOLVED" && f.State != "REFUTED" && (f.Severity == "critical" || f.Severity == "high" || f.Severity == "medium") {
				blocking = true
			}
		}
		if !blocking || w.FixRounds >= e.Config().Budget.MaxFixRounds {
			w.Phase = "BLOCKED"
		} else {
			w.FixRounds++
			w.Phase = "CHANGES_REQUESTED"
		}
	}
	return e.Store.Save(ctx, w, gateAction(*w, pass, reason), statusAction(*w))
}

type reviewReport struct {
	Obligations []domain.Obligation `json:"obligations"`
	Findings    []domain.Finding    `json:"findings"`
	Summary     string              `json:"summary"`
}

func (e *Engine) reviewTask(ctx context.Context, w domain.Workflow, task domain.Task, files map[string]sandbox.File, skeptic bool) (reviewReport, []domain.Evidence, error) {
	id := fmt.Sprintf("%s-g%d-%s-e%d", w.ID, w.Generation, task.ID, task.LeaseEpoch)
	ctx, cancel := context.WithTimeout(ctx, e.Config().AttemptTimeout())
	defer cancel()
	_ = e.Store.AttemptOwned(ctx, id, w.ID, task.ID, w.Generation, task.LeaseEpoch, w.LeaseOwner, "RUNNING", nil, nil)
	workspace, err := e.Sandbox.Create(ctx, id, files)
	if err != nil {
		return reviewReport{}, nil, err
	}
	defer e.Sandbox.Destroy(context.Background(), workspace)
	session := &toolSession{e: e, w: w, workspace: workspace, review: true}
	baseFiles, err := e.baseSnapshot(ctx, w)
	if err != nil {
		return reviewReport{}, nil, err
	}
	session.baseFiles = baseFiles
	var obligations []domain.Obligation
	for _, o := range w.Review.Obligations {
		for _, oid := range task.ObligationIDs {
			if o.ID == oid {
				obligations = append(obligations, o)
			}
		}
	}
	report := reviewReport{Findings: []domain.Finding{}}
	if e.Config().Runner.Mode == "fake" {
		output, err := session.handler(ctx, "read_file", raw(map[string]string{"path": "calculator.go"}))
		if err != nil {
			return report, nil, err
		}
		if !strings.Contains(output, "a + b") {
			return report, nil, errors.New("fixture reviewer found Add is incorrect")
		}
		ev := session.listEvidence()
		for _, o := range obligations {
			o.Result = "satisfied"
			o.Evidence = []string{ev[0].ID}
			report.Obligations = append(report.Obligations, o)
		}
		report.Summary = "Deterministic fixture source review completed"
	} else {
		prompt := reviewerPrompt
		input := map[string]any{"spec": w.Spec, "task": task, "obligations": obligations, "verification_key": w.Review.Key, "prior_findings": w.Review.Findings}
		if skeptic {
			prompt = skepticPrompt
			input["peer_reports"] = w.Review.Tasks
			input["findings"] = w.Review.Findings
			input["evidence"] = w.Evidence
		}
		out, err := e.ask(ctx, w, prompt, string(raw(input)), session.tools(), reviewSchema())
		if err != nil {
			return report, session.listEvidence(), err
		}
		if err = json.Unmarshal(out, &report); err != nil {
			return report, session.listEvidence(), err
		}
	}
	changes, err := e.Sandbox.CollectChanges(ctx, workspace)
	if err != nil {
		return report, session.listEvidence(), err
	}
	for _, change := range changes {
		if !strings.HasPrefix(change.Path, "review_tests/") {
			return report, session.listEvidence(), fmt.Errorf("review modified candidate source %s", change.Path)
		}
	}
	ev := session.listEvidence()
	_ = e.Store.AttemptOwned(context.WithoutCancel(ctx), id, w.ID, task.ID, w.Generation, task.LeaseEpoch, w.LeaseOwner, "ACCEPTED", report, ev)
	return report, ev, nil
}
func mergeReport(w *domain.Workflow, report reviewReport, assigned []string, evidence []domain.Evidence, skeptic bool) error {
	valid := map[string]bool{}
	for _, ev := range evidence {
		valid[ev.ID] = true
	}
	if skeptic {
		for _, ev := range w.Evidence {
			if ev.HeadSHA == w.HeadSHA {
				valid[ev.ID] = true
			}
		}
	}
	allowed := map[string]bool{}
	for _, id := range assigned {
		allowed[id] = true
	}
	seen := map[string]bool{}
	for _, o := range report.Obligations {
		if !allowed[o.ID] || seen[o.ID] {
			return errors.New("review reported unassigned or duplicate obligation")
		}
		seen[o.ID] = true
		if o.Result != "satisfied" && o.Result != "failed" && o.Result != "unverified" {
			return errors.New("invalid review coverage status")
		}
		if o.Result == "satisfied" && len(o.Evidence) == 0 {
			return errors.New("satisfied obligation has no evidence")
		}
		for _, id := range o.Evidence {
			if !valid[id] {
				return fmt.Errorf("review cited nonexistent evidence %s", id)
			}
		}
		for i := range w.Review.Obligations {
			if w.Review.Obligations[i].ID == o.ID {
				w.Review.Obligations[i].Result = o.Result
				w.Review.Obligations[i].Evidence = o.Evidence
			}
		}
	}
	for id := range allowed {
		if !seen[id] {
			return fmt.Errorf("review omitted obligation %s", id)
		}
	}
	for _, f := range report.Findings {
		if !skeptic || f.ID == "" {
			f.ID = "F-" + domain.Digest([]any{f.Path, f.Line, f.Description})[:10]
		}
		if f.Description == "" || f.Expected == "" || f.Actual == "" || f.Preconditions == "" {
			return errors.New("finding lacks hypothesis, preconditions or expected/actual behavior")
		}
		for _, id := range f.Evidence {
			if !valid[id] {
				return errors.New("finding evidence not recorded")
			}
		}
		if f.EvidenceType != "MODEL_CLAIM" && len(f.Evidence) == 0 {
			return errors.New("finding claims unrecorded source/tool evidence")
		}
		if f.State == "REFUTED" || f.State == "RESOLVED" {
			if !skeptic || len(f.Evidence) == 0 {
				return errors.New("finding can only be refuted by independent evidence")
			}
		} else if f.State == "" {
			f.State = "PROPOSED"
		}
		f.VerificationKey = w.Review.Key
		found := false
		for i := range w.Review.Findings {
			if w.Review.Findings[i].ID == f.ID {
				f.History = append(w.Review.Findings[i].History, "Independent review: "+report.Summary)
				w.Review.Findings[i] = f
				found = true
			}
		}
		if !found {
			w.Review.Findings = append(w.Review.Findings, f)
		}
	}
	return nil
}

// Source snapshots used by local demo publishing are persisted like real artifacts.
func loadLocalSnapshot(directory string) (map[string]sandbox.File, error) {
	b, err := os.ReadFile(directory + "/snapshot.json")
	if os.IsNotExist(err) {
		return localFiles(), nil
	}
	if err != nil {
		return nil, err
	}
	var files map[string]sandbox.File
	err = json.Unmarshal(b, &files)
	return files, err
}
func sortedChanges(changes []gh.FileChange) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
}
