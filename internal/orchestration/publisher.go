package orchestration

import (
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
	Pass           bool   `json:"pass"`
	Reason         string `json:"reason"`
	Key            string `json:"key,omitempty"`
	SHA            string `json:"sha,omitempty"`
	ManualOverride bool   `json:"manual_override,omitempty"`
}
type readyActionData struct {
	GateAfterReady *gateActionData `json:"gate_after_ready,omitempty"`
}

type publishActionData struct {
	IntegrationID  string          `json:"integration_id"`
	ExpectedTree   string          `json:"expected_tree"`
	Changes        []gh.FileChange `json:"changes"`
	TaskIDs        []string        `json:"task_ids"`
	Message        string          `json:"message"`
	ExpectedParent string          `json:"expected_parent"`
	BaseBranch     string          `json:"base_branch"`
	BaseSHA        string          `json:"base_sha"`
	Branch         string          `json:"branch"`
}

type expectedGenerationContextKey struct{}
type expectedHeadContextKey struct{}

func WithExpectedGeneration(ctx context.Context, generation int64) context.Context {
	return context.WithValue(ctx, expectedGenerationContextKey{}, generation)
}

func WithExpectedHead(ctx context.Context, head string) context.Context {
	return context.WithValue(ctx, expectedHeadContextKey{}, head)
}

// statusAction coalesces status updates into one bot-owned issue comment.
func statusAction(w domain.Workflow) store.Action {
	body := statusBody(w)
	return store.NewAction(w, "comment", map[string]any{"body": body, "marker": "status"})
}

func statusBody(w domain.Workflow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**AI 工作流状态：** `%s` · 控制状态 `%s` · 代际 `%d`", w.Phase, w.Control, w.Generation)
	if w.Phase == "NEEDS_INFO" && w.Control == "ACTIVE" {
		b.WriteString("\n\n**下一步：** 维护者直接在本 Issue 回复补充信息即可，无需 @ bot。收到回复后会重新分析；AI 同意后按开发接受策略自动进入开发或等待 `/ai accept`。")
	} else if w.Phase == "INTAKE" {
		b.WriteString("\n\n**下一步：** 检查 Issue 请求并评论 `/ai analyze`（或在控制台开始分析）。AI 同意后默认自动批准规格并进入开发；开启开发前手动接受时才需再评论 `/ai accept`。")
	} else if w.Phase == "SPEC_READY" {
		b.WriteString("\n\n**下一步：** AI 已同意执行。当前启用了开发前手动接受：检查下方规格和验收条件后，用 `/ai accept`（或控制台接受）进入开发。")
	}
	if w.LastError != "" {
		fmt.Fprintf(&b, "\n\n%s", w.LastError)
	}
	if w.PR > 0 {
		fmt.Fprintf(&b, "\n\n拉取请求： [#%d](https://github.com/%s/pull/%d)", w.PR, w.Repository, w.PR)
	}
	if w.ForceApproval != nil {
		fmt.Fprintf(&b, "\n\n**人工越过：** `%s`，操作者 %s，时间 %s（代际 %d）。", w.ForceApproval.Kind, w.ForceApproval.Actor, w.ForceApproval.ApprovedAt.UTC().Format(time.RFC3339), w.ForceApproval.Generation)
		if len(w.ForceApproval.Bypassed) > 0 {
			fmt.Fprintf(&b, " 已越过：%s。", strings.Join(w.ForceApproval.Bypassed, "; "))
		}
	}
	if w.Spec.Approved && w.Spec.ApprovedBy == "ai:dev-triage" {
		b.WriteString("\n\n**需求审批：** AI 已批准当前规格，自动进入开发。最终合并仍由维护者决定。")
	}
	if w.Spec.Title != "" && w.Spec.Approved {
		fmt.Fprintf(&b, "\n\n**已批准规格：** %s\n\n%s", w.Spec.Title, w.Spec.Description)
	}
	if w.ProposalSpec != nil {
		fmt.Fprintf(&b, "\n\n**待批准规格：** %s\n\n%s", w.ProposalSpec.Title, w.ProposalSpec.Description)
	}
	criteria := w.Spec.Criteria
	criteriaTitle := "验收条件"
	if w.ProposalSpec != nil {
		criteria = w.ProposalSpec.Criteria
		criteriaTitle = "提案验收条件"
	}
	if len(criteria) > 0 {
		fmt.Fprintf(&b, "\n\n**%s**\n", criteriaTitle)
		for _, c := range criteria {
			fmt.Fprintf(&b, "- %s: %s\n", c.ID, c.Description)
		}
	}
	if len(w.Tasks) > 0 {
		b.WriteString("\n**任务**\n")
		for _, t := range w.Tasks {
			fmt.Fprintf(&b, "- `%s` · %s · %s\n", t.ID, t.Status, t.Description)
		}
	}
	if len(w.Review.Findings) > 0 {
		b.WriteString("\n**审查发现**\n")
		for _, f := range w.Review.Findings {
			fmt.Fprintf(&b, "- `%s` %s (%s): %s", f.ID, f.Severity, f.State, f.Description)
			if len(f.Evidence) > 0 {
				fmt.Fprintf(&b, " — 证据： %s", strings.Join(f.Evidence, ", "))
			}
			b.WriteByte('\n')
		}
	}
	if len(w.Evidence) > 0 {
		fmt.Fprintf(&b, "\n<details>\n<summary>证据 · %d 条记录</summary>\n\n", len(w.Evidence))
		for _, ev := range w.Evidence {
			fmt.Fprintf(&b, "- `%s` %s · %s · %s\n", ev.ID, ev.Kind, ev.Hash, ev.Path)
		}
		b.WriteString("\n</details>\n")
	}
	return b.String()
}

func gateAction(w domain.Workflow, pass bool, reason string) store.Action {
	return store.NewAction(w, "gate", gateActionData{Pass: pass, Reason: reason, Key: w.Review.Key, SHA: w.HeadSHA})
}

func prOwnedSummary(w domain.Workflow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "工作流状态： %s\n", w.Phase)
	if w.Spec.Approved {
		fmt.Fprintf(&b, "\n## 已批准规格\n%s\n\n%s\n", w.Spec.Title, w.Spec.Description)
	}
	if len(w.Spec.Criteria) > 0 {
		b.WriteString("\n## 验收条件\n")
		for _, c := range w.Spec.Criteria {
			fmt.Fprintf(&b, "- **%s** %s\n", c.ID, c.Description)
		}
	}
	if len(w.Tasks) > 0 {
		b.WriteString("\n## 任务\n")
		for _, t := range w.Tasks {
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", t.ID, t.Status, t.Description)
		}
	}
	fmt.Fprintf(&b, "\n## 审查\n结论： %s\n%s\n", w.Review.Verdict, w.Review.Reason)
	if len(w.Review.Findings) > 0 {
		b.WriteString("\n### 审查发现\n")
		for _, f := range w.Review.Findings {
			fmt.Fprintf(&b, "- **%s · %s · %s** %s", f.ID, f.Severity, f.State, f.Description)
			if len(f.Evidence) > 0 {
				fmt.Fprintf(&b, " （证据：%s）", strings.Join(f.Evidence, ", "))
			}
			b.WriteByte('\n')
		}
	}
	if len(w.Review.Obligations) > 0 {
		b.WriteString("\n### 审查义务\n")
		for _, o := range w.Review.Obligations {
			fmt.Fprintf(&b, "- `%s` %s: %s", o.ID, o.Result, o.Description)
			if len(o.Evidence) > 0 {
				fmt.Fprintf(&b, " （证据：%s）", strings.Join(o.Evidence, ", "))
			}
			b.WriteByte('\n')
		}
	}
	if len(w.Evidence) > 0 {
		b.WriteString("\n### 证据元数据\n")
		for _, ev := range w.Evidence {
			fmt.Fprintf(&b, "- `%s` %s · sha256 `%s` · base `%s` · head `%s`\n", ev.ID, ev.Kind, ev.Hash, ev.BaseSHA, ev.HeadSHA)
		}
	}
	return b.String()
}

func prBodyWithOwnedSummary(w domain.Workflow) string {
	return fmt.Sprintf("实现 #%d 的需求。\n\n%s\n%s\n%s", w.Issue, gh.OwnedPRBodyStartMarker, prOwnedSummary(w), gh.OwnedPRBodyEndMarker)
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
	if event.Issue != nil && event.Issue.PullRequest != nil && x.Event == "issue_comment" && event.Action == "created" && event.Comment != nil {
		w, err := e.workflowForPR(ctx, repo.ID, event.Issue.Number)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		retired, retiredErr := e.Store.IsRetiredWorkflow(ctx, w.ID)
		if retiredErr != nil {
			return retiredErr
		}
		if retired {
			return nil
		}
		return e.issueComment(ctx, w, event.Comment.ID, event.Comment.User.Login, event.Comment.User.Type, event.Comment.Body)
	}
	if event.Issue != nil && event.Issue.PullRequest == nil {
		retired, err := e.Store.IsRetiredWorkflow(ctx, domain.ID(repo.ID, event.Issue.Number))
		if err != nil {
			return err
		}
		if retired {
			return nil
		}
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
		retired, retiredErr := e.Store.IsRetiredWorkflow(ctx, w.ID)
		if retiredErr != nil {
			return retiredErr
		}
		if retired {
			return nil
		}
		return e.pullRequestEvent(ctx, w, event)
	}
	return nil
}

// createIntake converges webhook delivery and discovery with an atomic initial
// status action, including repair of legacy interrupted intake transactions.
func (e *Engine) createIntake(ctx context.Context, repo config.Repository, installationID int64, issue gh.Issue, source string) error {
	// Evaluation profiles are granted only by the durable trial creation API.
	if strings.HasPrefix(strings.TrimSpace(issue.Body), "<!-- ai-workflow-evaluation-trial:") {
		return nil
	}
	if repo.ID <= 0 || installationID <= 0 || issue.Number <= 0 || issue.PullRequest != nil || issue.State == "closed" {
		return nil
	}
	w := issueIntake(repo, installationID, issue, e.Config().Runner.RequireAcceptForAnalysis)
	created, err := e.Store.CreateIntake(ctx, w, statusAction)
	if err != nil || !created {
		return err
	}
	if source == "reconcile_discovery" {
		e.Store.Audit(context.WithoutCancel(ctx), w.ID, "issue_intake_recovered", map[string]any{
			"source": source, "issue_url": issue.HTMLURL, "issue_number": issue.Number,
		})
	}
	return nil
}

func normalizedIssueHash(s string) string { return domain.Digest(strings.Join(strings.Fields(s), " ")) }

// v3 preserves semantic whitespace (code indentation, literal strings, Markdown).
// Only CRLF is normalized because GitHub/editor transports may change line endings.
func normalizedIssueSourceHash(title, body string) string {
	return "issue-v3:" + domain.Digest([]string{strings.ReplaceAll(title, "\r\n", "\n"), strings.ReplaceAll(body, "\r\n", "\n")})
}
func sourceMatchesIssue(w domain.Workflow, hash string) bool {
	return hash == normalizedIssueSourceHash(w.IssueTitle, w.IssueBody)
}

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
	if normalizedIssueSourceHash(w.IssueTitle, w.IssueBody) == normalizedIssueSourceHash(issue.Title, issue.Body) {
		return nil
	}
	e.mu.RLock()
	cancelWorker := e.active[w.ID]
	e.mu.RUnlock()
	err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Version != w.Version || x.Generation != w.Generation {
			return nil, store.ErrStale
		}
		if normalizedIssueSourceHash(x.IssueTitle, x.IssueBody) == normalizedIssueSourceHash(issue.Title, issue.Body) {
			return nil, store.ErrNoChange
		}
		if _, err := domain.ApplyIssueSourceEdit(x, issue.Title, issue.Body); err != nil {
			return nil, err
		}
		x.Discussion = append(x.Discussion, "Issue 编辑提案必须由维护者批准，才能改变已批准规格。")
		x.LastError = "Issue 已变化；停止操作完成后使用 /ai analyze 复核编辑。明确批准提案之前，已批准规格保持不变。"
		return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
	})
	if err == nil && cancelWorker != nil {
		cancelWorker()
	}
	return err
}

func (e *Engine) issueComment(ctx context.Context, w domain.Workflow, commentID int64, actor, actorType, body string) error {
	trimmed := strings.TrimSpace(body)
	if strings.Contains(trimmed, "<!-- ai-workflow-action-id:") {
		return nil
	}
	if commentID > 0 {
		ctx = store.WithCommand(ctx, fmt.Sprintf("github:%d:%d", w.RepositoryID, commentID))
	}
	if !strings.HasPrefix(strings.ToLower(trimmed), "/ai ") && strings.ToLower(trimmed) != "/ai" {
		if trimmed == "" {
			return nil
		}
		maintainerReply := strings.EqualFold(actorType, "User") && w.Phase == "NEEDS_INFO" && e.authorized(ctx, w, actor, false)
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			x.Discussion = append(x.Discussion, fmt.Sprintf("%s: %s", actor, trimmed))
			if resumeAfterClarification(*x, maintainerReply) {
				x.Generation++
				x.Phase = "TRIAGE"
				x.ProposalSpec = nil
				x.LastError = ""
				return []store.Action{statusAction(*x)}, nil
			}
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

func resumeAfterClarification(w domain.Workflow, maintainerReply bool) bool {
	return maintainerReply && w.AuthorizedBy != "" && w.Phase == "NEEDS_INFO" && w.Control == "ACTIVE"
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
		return errors.New("审查检查点已不匹配当前候选")
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
			return nil, errors.New("恢复期间审查检查点已变化，请重试 /ai resume")
		}
		x.Generation++
		x.Control = "ACTIVE"
		x.Phase = "REVIEWING"
		x.Review.ID = fmt.Sprintf("%s-review-%d-%s", x.ID, x.Generation, x.HeadSHA)
		x.Review.Key = newKey
		x.Review.Verdict = "PENDING"
		x.Review.Reason = "正在恢复质疑复核，并复用已接受的审查报告"
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

// integrationCheckpointReady allows an infrastructure-failed integration to
// resume against its exact accepted source snapshot. It deliberately does not
// recover or publish anything here: the normal leased pipeline repeats the
// complete integration validation and publication path after the command.
func (e *Engine) integrationCheckpointReady(ctx context.Context, w domain.Workflow) (domain.IntegrationRecord, bool, error) {
	if w.Phase != "BLOCKED" || w.Control != "ACTIVE" || !w.Spec.Approved || w.ProposalSpec != nil ||
		w.Generation < 1 || w.BaseSHA == "" || w.HeadSHA == "" || w.Spec.Digest == "" {
		return domain.IntegrationRecord{}, false, nil
	}
	var record domain.IntegrationRecord
	count := 0
	for _, candidate := range w.IntegrationRecords {
		if candidate.Status == "PREPARING" || candidate.Status == "CLEANUP_PENDING" {
			return domain.IntegrationRecord{}, false, nil
		}
		if candidate.Status == "FAILED" && candidate.Generation == w.Generation && candidate.ParentSHA == w.HeadSHA && candidate.SpecDigest == w.Spec.Digest {
			record = candidate
			count++
		}
	}
	if count != 1 || record.ID == "" || len(record.ArtifactIDs) == 0 {
		return domain.IntegrationRecord{}, false, nil
	}
	// A pending or uncertain publish may already have changed the remote PR.
	// Keep the ordinary generation-changing resume path in that case.
	clear, err := integrationCheckpointWritesClear(ctx, e.Store, w)
	if err != nil {
		return domain.IntegrationRecord{}, false, err
	}
	if !clear {
		return domain.IntegrationRecord{}, false, nil
	}

	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil {
		return domain.IntegrationRecord{}, false, err
	}
	byRun := make(map[string]domain.AgentRun, len(runs))
	for _, run := range runs {
		byRun[run.ID] = run
		if run.Generation == w.Generation && run.StartedAt.IsZero() == false &&
			(run.StoppedAt.IsZero() || run.Status == domain.AgentActive || run.Status == domain.AgentStopping) {
			return domain.IntegrationRecord{}, false, nil
		}
	}

	// LoadIntegrationInputs verifies the persisted record identity, source
	// artifacts, evidence hashes and deterministic integration ID. The
	// validator then checks the accepted attempt receipt and stopped builder
	// run for every artifact.
	record.Status = "PREPARING"
	inputs, err := e.LoadIntegrationInputs(ctx, w, record)
	if err != nil {
		return domain.IntegrationRecord{}, false, nil
	}
	validatedCodeTasks := 0
	for _, task := range w.Tasks {
		if taskKind(task) == "code" && task.Status == "VALIDATED" {
			validatedCodeTasks++
		}
	}
	if validatedCodeTasks != len(inputs) {
		return domain.IntegrationRecord{}, false, nil
	}
	seenTasks := map[string]bool{}
	for _, input := range inputs {
		if seenTasks[input.Task.ID] || e.validateIntegrationInput(ctx, w, input, byRun) != nil {
			return domain.IntegrationRecord{}, false, nil
		}
		seenTasks[input.Task.ID] = true
	}
	return record, true, nil
}

func integrationCheckpointWritesClear(ctx context.Context, s *store.Store, w domain.Workflow) (bool, error) {
	var pendingPublish, unknownWrite bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE workflow_id=$1 AND kind='publish' AND state IN('pending','running','unknown','retry_wait'))`, w.ID).Scan(&pendingPublish); err != nil {
		return false, err
	}
	if pendingPublish {
		return false, nil
	}
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE workflow_id=$1 AND state='unknown')`, w.ID).Scan(&unknownWrite); err != nil {
		return false, err
	}
	if unknownWrite {
		return false, nil
	}
	pending, err := s.PendingWrites(ctx, w.ID)
	if err != nil {
		return false, err
	}
	return pending == 0, nil
}

func sameIntegrationCheckpoint(current, expected domain.Workflow, recordID string) bool {
	if current.Version != expected.Version || current.ID != expected.ID || current.Generation != expected.Generation ||
		current.BaseSHA != expected.BaseSHA || current.HeadSHA != expected.HeadSHA || current.Spec.Digest != expected.Spec.Digest ||
		current.Phase != "BLOCKED" || current.Control != "ACTIVE" || !current.Spec.Approved || current.ProposalSpec != nil {
		return false
	}
	for _, record := range current.IntegrationRecords {
		if record.ID == recordID {
			return record.Status == "FAILED" && record.Generation == current.Generation && record.ParentSHA == current.HeadSHA && record.SpecDigest == current.Spec.Digest
		}
	}
	return false
}

func (e *Engine) resumeIntegrationCheckpoint(ctx context.Context, w domain.Workflow, record domain.IntegrationRecord) error {
	err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if !sameIntegrationCheckpoint(*x, w, record.ID) {
			return nil, errors.New("恢复期间集成检查点已变化，请检查当前状态后重试 /ai resume")
		}
		clear, err := integrationCheckpointWritesClear(ctx, e.Store, *x)
		if err != nil {
			return nil, err
		}
		if !clear {
			return nil, errors.New("恢复期间出现待确认的外部写入，集成检查点未恢复")
		}
		x.Control = "ACTIVE"
		x.Phase = "DEVELOPING"
		x.LastError = ""
		return []store.Action{statusAction(*x)}, nil
	})
	if err != nil {
		return err
	}
	e.Store.Audit(context.WithoutCancel(ctx), w.ID, "integration_checkpoint_resumed", map[string]any{
		"integration_id": record.ID, "generation": w.Generation, "artifact_ids": record.ArtifactIDs,
	})
	return nil
}

// Command applies a maintainer command. Pass dashboard=true only after the local
// dashboard has authenticated the caller; local fixture commands are dashboard-only.
func (e *Engine) Command(ctx context.Context, workflowID, actor, command string, dashboard ...bool) error {
	retired, retiredErr := e.Store.IsRetiredWorkflow(ctx, workflowID)
	if retiredErr != nil {
		return retiredErr
	}
	if retired {
		return errors.New("退役的工作流不能通过命令重新激活")
	}
	w, err := e.Store.Get(ctx, workflowID)
	if err != nil {
		return err
	}
	if expected, ok := ctx.Value(expectedGenerationContextKey{}).(int64); ok && w.Generation != expected {
		return errors.New("工作流代际已变化，请检查当前状态后重新确认")
	}
	if expected, ok := ctx.Value(expectedHeadContextKey{}).(string); ok && w.HeadSHA != expected {
		return errors.New("拉取请求头提交已变化，请检查当前状态后重新确认")
	}
	if !e.authorized(ctx, w, actor, len(dashboard) > 0 && dashboard[0]) {
		return errors.New("操作者不是已授权的仓库维护者")
	}
	if handled, err := e.HandleRiskAuthorizationCommand(ctx, workflowID, actor, command, dashboard...); handled {
		if err != nil {
			return err
		}
		return e.Store.Update(store.WithoutCommand(ctx), workflowID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Phase != "WAITING_RISK" {
				return nil, store.ErrNoChange
			}
			if x.Control != "ACTIVE" {
				return nil, errors.New("高风险授权已记录；工作流仍受维护者停止控制")
			}
			x.Phase = "VERIFYING"
			x.LastError = ""
			return []store.Action{statusAction(*x)}, nil
		})
	}
	if handled, err := e.HandleWaiverCommand(ctx, workflowID, actor, command, dashboard...); handled {
		return err
	}
	cmd := strings.ToLower(strings.TrimSpace(command))
	if strings.HasPrefix(cmd, "/ai ") {
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "/ai "))
	}
	switch cmd {
	case "accept":
		if w.Phase == "INTAKE" {
			return e.analyze(ctx, w, actor)
		}
		return e.accept(ctx, w, actor)
	case "analyze":
		return e.analyze(ctx, w, actor)
	case "forceaccept":
		return e.forceAccept(ctx, w, actor)
	case "forceaccept dev":
		if !forceIssueStage(w) {
			return fmt.Errorf("阶段 %s、控制状态 %s 不允许 forceaccept dev", w.Phase, w.Control)
		}
		return e.forceAcceptIssue(ctx, w, actor)
	case "forceaccept merge":
		if w.PR <= 0 {
			return errors.New("forceaccept merge 需要已有拉取请求")
		}
		return e.forceAcceptPR(ctx, w, actor)
	case "status":
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			return []store.Action{store.NewAction(*x, "comment", map[string]any{"body": statusBody(*x), "marker": "status"})}, nil
		})
	case "reject":
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Phase != "SPEC_READY" {
				return nil, fmt.Errorf("规格就绪后才能拒绝；当前阶段为 %s", x.Phase)
			}
			x.Generation++
			x.Phase = "REJECTED"
			x.Control = "PAUSED"
			x.ProposalSpec = nil
			x.LastError = "维护者已拒绝规格"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		})
	case "defer":
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Phase != "SPEC_READY" && x.Phase != "TRIAGE" && x.Phase != "INTAKE" {
				return nil, fmt.Errorf("阶段 %s 不允许暂缓", x.Phase)
			}
			x.Generation++
			x.Phase = "DEFERRED"
			x.Control = "PAUSED"
			x.LastError = "维护者已暂缓处理"
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
			return []store.Action{gateAction(*x, false, "工作流控制状态已变化"), statusAction(*x)}, nil
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
			return errors.New("对账发现外部变化；请审查并批准提案后恢复")
		}
		if refreshed.ProposalSpec != nil {
			return errors.New("待批准规格需要明确执行 `/ai accept`；`/ai analyze` 只用于请求重新分析")
		}
		if refreshed.ProposalSpec == nil && !sourceMatchesIssue(refreshed, refreshed.Spec.SourceHash) {
			return errors.New("Issue 编辑后需先执行 `/ai analyze` 才能恢复")
		}
		currentKey := e.key(ctx, refreshed)
		if skepticCheckpointReady(refreshed, currentKey) {
			return e.resumeSkepticCheckpoint(ctx, refreshed, currentKey)
		}
		if record, ready, err := e.integrationCheckpointReady(ctx, refreshed); err != nil {
			return err
		} else if ready {
			return e.resumeIntegrationCheckpoint(ctx, refreshed, record)
		}
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Version != refreshed.Version || x.Generation != refreshed.Generation {
				return nil, store.ErrStale
			}
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
				return nil, errors.New("工作流没有已批准规格；请使用 `/ai analyze` 请求分析")
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
			x.LastError = "已请求重新规划；替代规格批准前，原已批准规格仍然有效"
			x.Discussion = append(x.Discussion, "维护者请求重新规划；明确批准替代规格之前，保留已批准规格。")
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		})
	case "review deep":
		return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.PR <= 0 {
				return nil, errors.New("深度审查需要已有拉取请求")
			}
			x.Generation++
			x.Control = "ACTIVE"
			x.Phase = "REVIEWING"
			x.Review.Verdict = "PENDING"
			x.LastError = ""
			return []store.Action{gateAction(*x, false, "深度审查已开始"), statusAction(*x)}, nil
		})
	default:
		return fmt.Errorf("未知 /ai 命令 %q", cmd)
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
		if x.Control != "ACTIVE" && x.Control != "PAUSED" {
			return nil, errors.New("当前控制状态不允许批准规格")
		}
		if x.Phase != "SPEC_READY" {
			return nil, fmt.Errorf("规格就绪后才能批准；当前阶段为 %s", x.Phase)
		}
		if err := validateProposalSpec(x, x.ProposalSpec); err != nil {
			return nil, err
		}
		var proposed *domain.SpecChangeProposal
		for i := range x.SpecChanges {
			if x.SpecChanges[i].Spec.Digest == x.ProposalSpec.Digest && len(x.SpecChanges[i].Decisions) == 0 {
				proposed = &x.SpecChanges[i]
				break
			}
		}
		if proposed == nil {
			value, err := domain.ProposeSpecChange(x, *x.ProposalSpec, actor, time.Now().UTC())
			if err != nil {
				return nil, err
			}
			proposed = &value
		}
		if err := domain.DecideSpecChange(x, proposed.ID, proposed.PreviousApprovedDigest, "accepted", actor, "维护者明确批准规格提案", time.Now().UTC()); err != nil {
			return nil, err
		}
		x.Generation++
		x.Control = "ACTIVE"
		x.Phase = "PLANNING"
		x.AuthorizedBy = actor
		x.LastError = ""
		return []store.Action{statusAction(*x)}, nil
	})
}

func validateProposalSpec(w *domain.Workflow, proposal *domain.Spec) error {
	if proposal == nil {
		return errors.New("没有可批准的规格提案")
	}
	if proposal.Title == "" || proposal.Description == "" || len(proposal.Criteria) == 0 {
		return errors.New("不能批准不完整的规格")
	}
	if !sourceMatchesIssue(*w, proposal.SourceHash) {
		return errors.New("提案与当前 Issue 不匹配，请使用 `/ai analyze` 刷新")
	}
	seen := map[string]bool{}
	for _, c := range proposal.Criteria {
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Description) == "" || seen[c.ID] {
			return errors.New("提案验收条件无效")
		}
		seen[c.ID] = true
	}
	if proposal.Digest == "" || proposal.Digest != domain.SpecDigest(*proposal) {
		return errors.New("提案规格摘要无效")
	}
	return nil
}

func analyzeAllowed(w domain.Workflow) bool {
	if w.Control == "CANCELLING" || w.Control == "CANCELLED" || w.Control == "HUMAN_OWNED" || w.Control == "COMPLETED" || w.Control == "PAUSING" {
		return false
	}
	switch w.Phase {
	case "INTAKE", "NEEDS_INFO":
		return w.Control == "ACTIVE" || w.Control == "PAUSED"
	case "TRIAGE", "SPEC_READY", "DEFERRED", "REJECT_PROPOSED", "REJECTED", "BLOCKED", "WAITING_CONFIGURATION":
		return w.Control != "ACTIVE" && (!w.Spec.Approved || !sourceMatchesIssue(w, w.Spec.SourceHash))
	default:
		return w.Spec.Approved && !sourceMatchesIssue(w, w.Spec.SourceHash) && w.Control == "PAUSED"
	}
}

func (e *Engine) analyze(ctx context.Context, w domain.Workflow, actor string) error {
	if !analyzeAllowed(w) {
		return fmt.Errorf("阶段 %s、控制状态 %s 不允许重新分析", w.Phase, w.Control)
	}
	if w.Control == "PAUSED" {
		if pending, err := e.Store.PendingWrites(ctx, w.ID); err != nil {
			return err
		} else if pending > 0 {
			return errors.New("存在结果未确认的外部写入，请先对账再分析")
		}
	}
	e.stop(w.ID)
	return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if !analyzeAllowed(*x) || x.Generation != w.Generation || x.Version != w.Version {
			return nil, errors.New("开始分析前工作流已变化")
		}
		x.Generation++
		x.Control = "ACTIVE"
		x.Phase = "TRIAGE"
		x.ProposalSpec = nil
		x.AuthorizedBy = actor
		x.LastError = "维护者已请求分析；明确批准新提案之前，已批准规格保持不变"
		return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
	})
}

func forceControlAllowed(w domain.Workflow) bool {
	return w.Control == "ACTIVE" || w.Control == "PAUSED" || w.Control == "CANCELLED"
}

func forceIssueStage(w domain.Workflow) bool {
	if !forceControlAllowed(w) {
		return false
	}
	switch w.Phase {
	case "INTAKE", "TRIAGE", "NEEDS_INFO", "SPEC_READY", "DEFERRED", "REJECT_PROPOSED", "REJECTED":
		return true
	case "BLOCKED":
		return w.PR == 0
	default:
		return false
	}
}

func manualForceIdentity(w domain.Workflow, approval *domain.ForceApproval, key, policyDigest string) bool {
	return approval != nil && approval.Kind == "pr_merge_gate" && approval.Generation == w.Generation &&
		approval.SpecDigest == w.Spec.Digest && approval.PolicyDigest == policyDigest &&
		approval.VerificationKey == key && w.Review.Key == key && approval.BaseSHA == w.BaseSHA && approval.HeadSHA == w.HeadSHA &&
		w.PR > 0 && w.Control == "ACTIVE" && w.Phase == "AWAITING_MAINTAINER"
}

// ForceApprovalCurrent reports whether a recorded manual PR override still
// applies to the workflow's exact candidate identity.
func (e *Engine) ForceApprovalCurrent(ctx context.Context, w domain.Workflow) bool {
	if w.ForceApproval == nil || w.ForceApproval.Kind != "pr_merge_gate" {
		return false
	}
	return manualForceIdentity(w, w.ForceApproval, e.key(ctx, w), domain.Digest(e.repository(w)))
}

func (e *Engine) IssueMatchesApprovedSpec(w domain.Workflow) bool {
	return w.Spec.Approved && sourceMatchesIssue(w, w.Spec.SourceHash)
}

func (e *Engine) forceAccept(ctx context.Context, w domain.Workflow, actor string) error {
	if forceIssueStage(w) {
		return e.forceAcceptIssue(ctx, w, actor)
	}
	if w.PR <= 0 {
		return fmt.Errorf("阶段 %s 不允许 forceaccept", w.Phase)
	}
	return e.forceAcceptPR(ctx, w, actor)
}

func (e *Engine) forceAcceptIssue(ctx context.Context, w domain.Workflow, actor string) error {
	if !forceIssueStage(w) {
		return errors.New("当前工作流状态不允许通过 forceaccept 启动开发")
	}
	if pending, err := e.Store.PendingWrites(ctx, w.ID); err != nil {
		return err
	} else if pending > 0 {
		return errors.New("存在结果未确认的外部写入，请先对账再执行 forceaccept")
	}
	if w.Repository != "local/pilot" {
		c, owner, repo, err := e.client(ctx, w)
		if err != nil {
			return err
		}
		issue, err := c.GetIssue(ctx, owner, repo, w.Issue)
		if err != nil {
			return err
		}
		if issue.State != "open" {
			return errors.New("forceaccept 需要原始 Issue 仍为打开状态")
		}
		if issue.Title != w.IssueTitle || normalizedIssueSourceHash(issue.Title, issue.Body) != normalizedIssueSourceHash(w.IssueTitle, w.IssueBody) {
			return errors.New("当前页面加载后 Issue 已变化；请对账并检查最新需求后执行 forceaccept")
		}
	}
	proposal := domain.Spec{}
	if err := validateProposalSpec(&w, w.ProposalSpec); err == nil {
		proposal = *w.ProposalSpec
	} else if w.Spec.Approved && sourceMatchesIssue(w, w.Spec.SourceHash) {
		proposal = w.Spec
	} else {
		criteria := append([]domain.Criterion(nil), w.Spec.Criteria...)
		criteria = append(criteria, domain.Criterion{ID: "AC-FORCE-REQUEST", Description: "实现并验证当前 Issue 标题与正文中的完整需求，保留此前已批准约束；对未解决问题采用合理并有记录的默认值，不能把每个问题都当作硬性要求。"})
		proposal = domain.Spec{
			Revision: w.Spec.Revision + 1, Title: w.IssueTitle,
			Description: manualIssueSpecDescription(w),
			Criteria:    criteria,
			NonGoals:    append([]string(nil), w.Spec.NonGoals...),
			Risk:        "人工强制批准；已越过分析与规格审查。",
			SourceHash:  normalizedIssueSourceHash(w.IssueTitle, w.IssueBody),
		}
	}
	proposal.Approved = true
	proposal.ApprovedBy = actor
	proposal.Digest = domain.SpecDigest(proposal)
	now := time.Now().UTC()
	bypassed := []string{"模型分诊", "规格审查"}
	e.mu.RLock()
	cancelWorker := e.active[w.ID]
	e.mu.RUnlock()
	err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if !forceIssueStage(*x) || x.Generation != w.Generation || x.Version != w.Version || !sourceMatchesIssue(*x, proposal.SourceHash) {
			return nil, errors.New("forceaccept 启动开发前工作流已变化")
		}
		x.Generation++
		proposal.Revision = x.Spec.Revision + 1
		proposal.Digest = domain.SpecDigest(proposal)
		x.Spec = proposal
		x.ProposalSpec = nil
		x.Tasks = nil
		x.Review = domain.Review{}
		x.Control = "ACTIVE"
		x.Phase = "PLANNING"
		x.AuthorizedBy = actor
		x.LastError = "已通过明确的 /ai forceaccept 启动开发；已越过模型分诊和规格审查。"
		key := e.key(ctx, *x)
		policy := e.repository(*x)
		x.ForceApproval = &domain.ForceApproval{Kind: "issue_development", Actor: actor, Command: "forceaccept", ApprovedAt: now, Generation: x.Generation, SpecDigest: x.Spec.Digest, PolicyDigest: domain.Digest(policy), VerificationKey: key, IssueHash: normalizedIssueSourceHash(x.IssueTitle, x.IssueBody), Bypassed: bypassed, PriorConstraints: append([]domain.Criterion(nil), w.Spec.Criteria...), Discussion: append([]string(nil), w.Discussion...)}
		return []store.Action{gateAction(*x, false, "人工 forceaccept 已重新启动开发；已撤销原拉取请求门禁批准"), statusAction(*x)}, nil
	})
	if err == nil && cancelWorker != nil {
		cancelWorker()
	}
	return err
}

func (e *Engine) forceAcceptPR(ctx context.Context, w domain.Workflow, actor string) error {
	if !forceControlAllowed(w) {
		return errors.New("forceaccept 不能覆盖正在停止、人工接管或已完成的控制状态")
	}
	switch w.Phase {
	case "VERIFYING", "REVIEWING", "CHANGES_REQUESTED", "BLOCKED", "AWAITING_MAINTAINER":
	default:
		return fmt.Errorf("阶段 %s 不允许对拉取请求执行 forceaccept", w.Phase)
	}
	if w.Repository != "local/pilot" {
		if err := e.reconcileOne(store.WithoutCommand(ctx), w); err != nil && !errors.Is(err, store.ErrNoChange) {
			return fmt.Errorf("forceaccept 前无法对账当前拉取请求：%w", err)
		}
		refreshed, err := e.Store.Get(ctx, w.ID)
		if err != nil {
			return err
		}
		w = refreshed
		if !forceControlAllowed(w) {
			return errors.New("拉取请求对账期间工作流控制状态已变化")
		}
		switch w.Phase {
		case "VERIFYING", "REVIEWING", "CHANGES_REQUESTED", "BLOCKED", "AWAITING_MAINTAINER":
		default:
			return fmt.Errorf("对账后阶段 %s 不允许对拉取请求执行 forceaccept", w.Phase)
		}
		c, owner, repo, err := e.client(ctx, w)
		if err != nil {
			return err
		}
		pr, err := c.GetPullRequest(ctx, owner, repo, w.PR)
		if err != nil {
			return err
		}
		if pr.Merged || pr.State != "open" {
			return errors.New("forceaccept 需要打开且尚未合并的拉取请求")
		}
		if pr.Head.SHA == "" || pr.Base.SHA == "" || pr.Head.SHA != w.HeadSHA || pr.Base.SHA != w.BaseSHA {
			return errors.New("对账期间拉取请求身份已变化；请验证当前候选后执行 forceaccept")
		}
	}
	if pending, err := e.Store.PendingWrites(ctx, w.ID); err != nil {
		return err
	} else if pending > 0 {
		return errors.New("存在结果未确认的外部写入，请先对账再执行 forceaccept")
	}
	e.mu.RLock()
	cancelWorker := e.active[w.ID]
	e.mu.RUnlock()
	next := w
	next.Generation++
	next.Control = "ACTIVE"
	next.Phase = "AWAITING_MAINTAINER"
	policy := e.repository(w)
	key := e.key(ctx, next)
	now := time.Now().UTC()
	bypassed := []string{"自动验证", "审查分析", "质疑复核", "自动合并门禁条件"}
	err := e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Version != w.Version || x.Generation != w.Generation || x.PR != w.PR || x.HeadSHA != w.HeadSHA || x.BaseSHA != w.BaseSHA || x.Control == "HUMAN_OWNED" || x.Control == "CANCELLING" || x.Control == "COMPLETED" {
			return nil, errors.New("forceaccept 前拉取请求身份或控制状态已变化")
		}
		x.Generation++
		x.Control = "ACTIVE"
		x.Phase = "AWAITING_MAINTAINER"
		x.Review.Key = key
		x.Review.Verdict = "FORCED"
		x.Review.Reason = "人工越过，操作者 " + actor + " 通过 /ai forceaccept 批准；已越过审查和自动验证。拉取请求仍需维护者合并。"
		x.ForceApproval = &domain.ForceApproval{Kind: "pr_merge_gate", Actor: actor, Command: "forceaccept", ApprovedAt: now, Generation: x.Generation, SpecDigest: x.Spec.Digest, PolicyDigest: domain.Digest(policy), VerificationKey: key, BaseSHA: x.BaseSHA, HeadSHA: x.HeadSHA, Bypassed: bypassed}
		x.LastError = x.Review.Reason
		gateData := gateActionData{Pass: true, Reason: x.Review.Reason, Key: key, SHA: x.HeadSHA, ManualOverride: true}
		actions := []store.Action{statusAction(*x)}
		if !x.PRReady {
			// Do not rely on insertion order: equal-time actions are sorted by ID.
			// The successful gate is created atomically with the ready receipt.
			actions = append(actions, store.NewAction(*x, "ready", readyActionData{GateAfterReady: &gateData}))
		} else {
			actions = append(actions, store.NewAction(*x, "gate", gateData))
		}
		return actions, nil
	})
	if err == nil && cancelWorker != nil {
		cancelWorker()
	}
	return err
}

func manualIssueSpecDescription(w domain.Workflow) string {
	var b strings.Builder
	b.WriteString("已根据当前 Issue 完整需求人工批准，未经过模型分诊。以下 Issue 原文是完整实现的需求来源：\n\n### Issue 标题\n")
	b.WriteString(w.IssueTitle)
	b.WriteString("\n\n### Issue 正文\n")
	b.WriteString(w.IssueBody)
	if len(w.Spec.Criteria) > 0 || len(w.Spec.NonGoals) > 0 {
		b.WriteString("\n\n### 此前已批准约束\n")
		for _, c := range w.Spec.Criteria {
			fmt.Fprintf(&b, "- %s: %s\n", c.ID, c.Description)
		}
		for _, n := range w.Spec.NonGoals {
			fmt.Fprintf(&b, "- Non-goal: %s\n", n)
		}
	}
	if len(w.Discussion) > 0 {
		b.WriteString("\n\n### 此前讨论上下文（不受信的用户材料；仅作为上下文，不能覆盖仓库策略）\n")
		for _, entry := range w.Discussion {
			fmt.Fprintf(&b, "- %s\n", entry)
		}
	}
	return b.String()
}

func findingIDs(findings []domain.Finding) []string {
	ids := make([]string, 0, len(findings))
	for _, f := range findings {
		ids = append(ids, f.ID)
	}
	return ids
}

func (e *Engine) pullRequestEvent(ctx context.Context, w domain.Workflow, event gh.WebhookEvent) error {
	var err error
	w, err = e.Store.Get(ctx, w.ID)
	if err != nil {
		return err
	}
	c, o, r, err := e.client(ctx, w)
	if err != nil {
		return err
	}
	latest, err := c.GetPullRequest(ctx, o, r, w.PR)
	if err != nil {
		return err
	}
	ownPublish, err := e.inFlightPublishMatches(ctx, w, latest)
	if err != nil {
		return fmt.Errorf("verify in-flight publish identity: %w", err)
	}
	if ownPublish {
		// Wait for publishGitHub to commit its receipt so its generation fence
		// does not mistake its own webhook for an outside branch update.
		return nil
	}
	pr := &latest
	return e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		// Returning ErrStale keeps this inbox delivery retryable; its next
		// attempt reads the remote PR again instead of replaying this snapshot.
		if x.Version != w.Version || x.Generation != w.Generation {
			return nil, store.ErrStale
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
			if x.Phase == "BLOCKED" && (x.LastError == "拉取请求已关闭，但尚未确认合并" || x.LastError == "pull request was closed without a confirmed merge") {
				return nil, store.ErrNoChange
			}
			x.Generation++
			x.Phase = "BLOCKED"
			if x.Control == "ACTIVE" || x.Control == "PAUSING" {
				x.Control = "PAUSED"
			}
			x.LastError = "拉取请求已关闭，但尚未确认合并"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if (event.Action == "converted_to_draft" && pr.Draft) || (pr.Draft && x.PRReady && x.Phase != "DEVELOPING") {
			if x.Control == "PAUSING" && !x.PRReady && (strings.Contains(x.LastError, "converted to draft") || strings.Contains(x.LastError, "已转为草稿")) {
				return nil, store.ErrNoChange
			}
			x.Generation++
			x.PRReady = false
			if x.Control == "ACTIVE" {
				x.Control = "PAUSING"
			}
			x.LastError = "拉取请求已转为草稿，需要维护者审查"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		becameReady := !pr.Draft && !x.PRReady
		if becameReady {
			x.PRReady = true
		}
		if pr.Head.SHA != "" && x.HeadSHA != "" && pr.Head.SHA != x.HeadSHA {
			x.Generation++
			x.HeadSHA = pr.Head.SHA
			if x.Control == "ACTIVE" {
				x.Control = "PAUSING"
			}
			x.Review.Verdict = "OBSOLETE"
			x.LastError = "工作流之外修改了拉取请求头提交；请先对账再继续"
			return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
		}
		if pr.Base.Ref != "" && pr.Base.Ref != x.BaseBranch {
			x.Generation++
			x.BaseBranch = pr.Base.Ref
			x.BaseSHA = pr.Base.SHA
			if x.Control == "ACTIVE" {
				x.Control = "PAUSING"
			}
			x.Review.Verdict = "OBSOLETE"
			x.LastError = "拉取请求目标分支已变化；恢复后验证当前目标"
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
			return []store.Action{gateAction(*x, false, "基线分支已变化，验证已过期"), statusAction(*x)}, nil
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
		e.processOutboxAction(ctx, a)
	}
}

// Leave enough of the 90-second lease for a durable receipt after cancellation.
// A stalled GitHub request must not monopolize the single publisher forever.
func (e *Engine) processOutboxAction(ctx context.Context, a store.Action) {
	effectCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	err := e.Store.StartAction(effectCtx, a)
	var receipt any
	var unknown bool
	if err == nil {
		receipt, err, unknown = e.performAction(effectCtx, a)
	}
	cancel()
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	if errors.Is(err, store.ErrStale) {
		_ = e.Store.AbandonAction(finishCtx, a, err.Error())
		return
	}
	if finishErr := e.Store.FinishAction(finishCtx, a, receipt, err, unknown); finishErr != nil && ctx.Err() == nil {
		e.Store.Audit(finishCtx, a.WorkflowID, "outbox_finish_error", map[string]any{"action": a.ID, "error": finishErr.Error()})
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
	var data readyActionData
	if err := json.Unmarshal(a.Data, &data); err != nil {
		return nil, err, false
	}

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
			return e.afterReadyGate(ctx, *x, a, data)
		})
		if err != nil {
			return map[string]any{"local": true, "ready": true}, err, false
		}
		return map[string]any{"local": true, "ready": true}, nil, false
	}
	if current.PR <= 0 {
		return nil, errors.New("标记就绪前必须已有拉取请求"), false
	}
	c, owner, repo, err := e.client(ctx, current)
	if err != nil {
		return nil, err, false
	}
	pr, err := c.MarkPullRequestReady(ctx, owner, repo, current.PR)
	if err != nil {
		return nil, err, outcomeUnknown(err)
	}
	if reason := remotePRIdentityReason(current, pr); reason != "" {
		if err := e.recordGateIdentityDrift(ctx, current, pr, reason); err != nil {
			return nil, err, false
		}
		return nil, fmt.Errorf("%w: %s", store.ErrStale, reason), false
	}
	receipt := map[string]any{"pr": pr.Number, "draft": pr.Draft, "head_sha": pr.Head.SHA}
	err = e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
		if x.Generation != a.Generation || x.Control != "ACTIVE" || x.PR != current.PR {
			return nil, store.ErrStale
		}
		if x.HeadSHA != current.HeadSHA || x.BaseSHA != current.BaseSHA || x.BaseBranch != current.BaseBranch || x.Branch != current.Branch {
			return nil, store.ErrStale
		}
		x.PRReady = true
		return e.afterReadyGate(ctx, *x, a, data)
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
	manual := d.Pass && d.ManualOverride
	if d.Pass {
		if current.Generation != a.Generation || current.Control != "ACTIVE" || (current.Phase != "READY" && current.Phase != "AWAITING_MAINTAINER") {
			return nil, store.ErrStale, false
		}
		if !e.gateActionMatchesCurrent(ctx, current, a, d, manual) {
			if err := e.invalidateLocalGateIdentity(ctx, current, "审查身份、证据或人工越过记录与当前候选不匹配"); err != nil {
				return nil, err, false
			}
			return nil, store.ErrStale, false
		}
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
	var confirmedPR gh.PullRequest
	if d.Pass {
		confirmedPR, err = c.GetPullRequest(ctx, owner, repo, current.PR)
		if err != nil {
			return nil, err, outcomeUnknown(err)
		}
		if reason := remotePRIdentityReason(current, confirmedPR); reason != "" {
			if driftErr := e.recordGateIdentityDrift(ctx, current, confirmedPR, reason); driftErr != nil {
				return nil, driftErr, false
			}
			return nil, fmt.Errorf("%w: %s", store.ErrStale, reason), false
		}
		latest, getErr := e.Store.Get(ctx, current.ID)
		if getErr != nil {
			return nil, getErr, false
		}
		if latest.Version != current.Version || !e.gateActionMatchesCurrent(ctx, latest, a, d, manual) {
			return nil, store.ErrStale, false
		}
	}
	keySummary := d.Reason
	if manual && current.ForceApproval != nil {
		keySummary = fmt.Sprintf("人工越过：%s 于 %s 通过 /ai forceaccept 明确批准。这不代表自动验证通过；拉取请求仍需维护者合并。\n\n已越过：%s", current.ForceApproval.Actor, current.ForceApproval.ApprovedAt.UTC().Format(time.RFC3339), strings.Join(current.ForceApproval.Bypassed, "; "))
	}
	if d.Key != "" {
		keySummary += "\n\n验证身份：" + d.Key
	}
	input := gh.CheckRunInput{Name: "workflow/merge-gate", HeadSHA: targetSHA, Status: "completed", Conclusion: "failure", DetailsURL: fmt.Sprintf("https://github.com/%s/pull/%d", current.Repository, current.PR), Output: gh.CheckOutput{Title: "AI 工作流门禁", Summary: keySummary}, Gate: true}
	if d.Pass {
		input.Conclusion = "success"
	} else if strings.Contains(strings.ToLower(d.Reason), "in progress") || strings.Contains(d.Reason, "进行中") {
		input.Status = "in_progress"
		input.Conclusion = ""
	}
	checks, findErr := c.ListCheckRuns(ctx, owner, repo, targetSHA)
	if findErr != nil {
		return nil, findErr, false
	}
	if d.Pass {
		// ListCheckRuns is a network round trip. Re-read the candidate immediately
		// before publishing green so a ref/base change during that interval cannot
		// inherit the earlier PR snapshot.
		latestPR, verifyErr := c.GetPullRequest(ctx, owner, repo, current.PR)
		if verifyErr != nil {
			return nil, verifyErr, outcomeUnknown(verifyErr)
		}
		if reason := remotePRIdentityReason(current, latestPR); reason != "" {
			if driftErr := e.recordGateIdentityDrift(ctx, current, latestPR, reason); driftErr != nil {
				return nil, driftErr, false
			}
			return nil, fmt.Errorf("%w: %s", store.ErrStale, reason), false
		}
		latestWorkflow, verifyErr := e.Store.Get(ctx, current.ID)
		if verifyErr != nil {
			return nil, verifyErr, false
		}
		if latestWorkflow.Version != current.Version || !e.gateActionMatchesCurrent(ctx, latestWorkflow, a, d, manual) {
			return nil, store.ErrStale, false
		}
		if !manual {
			if _, err := e.trustedCIGate(ctx, current); err != nil {
				return nil, err, false
			}
		}
		changes, riskErr := e.CurrentRiskChanges(ctx, current)
		if riskErr != nil {
			return nil, riskErr, false
		}
		if riskErr = e.CheckRiskAuthorization(ctx, current, changes); riskErr != nil {
			return nil, riskErr, false
		}
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
					check = candidate
					err = nil
					break
				}
				if prior != nil && candidate.ID == prior.ID {
					return nil, err, true
				}
			}
		}
		if err != nil {
			return nil, err, true
		}
	}
	if d.Pass {
		if !manual {
			if _, ciErr := e.trustedCIGate(ctx, current); ciErr != nil {
				revokeErr := e.revokeGateSuccess(ctx, c, owner, repo, check, input, "门禁发布后可信 CI 身份变化："+ciErr.Error())
				stateErr := e.recordTrustedCIDrift(ctx, current, ciErr)
				if revokeErr != nil {
					return check, errors.Join(ciErr, revokeErr, stateErr), true
				}
				return check, errors.Join(store.ErrStale, ciErr, stateErr), false
			}
		}
		livePR, liveErr := c.GetPullRequest(ctx, owner, repo, current.PR)
		latest, getErr := e.Store.Get(ctx, current.ID)
		driftReason := ""
		if liveErr != nil {
			driftReason = "门禁发布后无法重新核验拉取请求身份"
		} else {
			driftReason = remotePRIdentityReason(current, livePR)
		}
		if getErr != nil || latest.Version != current.Version || !e.gateActionMatchesCurrent(ctx, latest, a, d, manual) {
			if driftReason == "" {
				driftReason = "门禁发布期间工作流身份已变化"
			}
		}
		if driftReason != "" {
			revokeErr := e.revokeGateSuccess(ctx, c, owner, repo, check, input, driftReason)
			if liveErr == nil {
				_ = e.recordGateIdentityDrift(ctx, current, livePR, driftReason)
			}
			if revokeErr != nil {
				return check, revokeErr, true
			}
			return check, fmt.Errorf("%w: %s", store.ErrStale, driftReason), false
		}
	}
	if targetSHA == current.HeadSHA && current.Generation == a.Generation {
		if d.Pass {
			latest, latestErr := e.Store.Get(ctx, w.ID)
			if latestErr != nil {
				return check, latestErr, false
			}
			valid := e.gateActionMatchesCurrent(ctx, latest, a, d, manual)
			if valid {
				if _, bodyErr := c.UpdateOwnedPRBody(ctx, owner, repo, latest.PR, prOwnedSummary(latest)); bodyErr != nil {
					return check, bodyErr, outcomeUnknown(bodyErr)
				}
			}
		}
		_ = e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			valid := x.Generation == a.Generation && x.Version == current.Version && x.Control == "ACTIVE" &&
				(x.Phase == "READY" || x.Phase == "AWAITING_MAINTAINER") && x.PRReady && x.PR == current.PR &&
				x.HeadSHA == current.HeadSHA && x.BaseSHA == current.BaseSHA && x.BaseBranch == current.BaseBranch && x.Review.Key == d.Key
			if !valid {
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
	if err := validatePublishIntegration(w, d); err != nil {
		return nil, err, false
	}
	if len(d.Changes) == 0 {
		return nil, errors.New("发布操作没有文件变更"), false
	}
	if d.ExpectedParent == "" || d.BaseBranch == "" || d.BaseSHA == "" || d.Branch == "" {
		return nil, errors.New("发布操作缺少分支身份"), false
	}
	policy := e.repository(w)
	for _, f := range d.Changes {
		if !allowedPath(f.Path, policy, []string{"**"}) {
			return nil, fmt.Errorf("不允许发布路径：%s", f.Path), false
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
		return nil, fmt.Errorf("基线分支已移动：预期 %s，实际 %s", d.BaseSHA, baseSHA), false
	}
	branchSHA, err := c.GetBranchSHA(ctx, owner, repo, d.Branch)
	if err != nil {
		var apiErr *gh.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
			return nil, err, false
		}
		if d.ExpectedParent != d.BaseSHA {
			return nil, errors.New("新发布分支必须从配置的基线 SHA 开始"), false
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
		// Claim only the exact single-parent commit and complete requested diff;
		// a marker alone cannot authorize additional external file changes.
		info, infoErr := c.GetCommitInfo(ctx, owner, repo, branchSHA)
		if infoErr != nil {
			return nil, infoErr, false
		}
		if info.SHA != branchSHA || info.Message != d.Message+"\n\n"+marker || len(info.Parents) != 1 || info.Parents[0] != d.ExpectedParent {
			return nil, fmt.Errorf("发布分支已移动：预期 %s，实际 %s", d.ExpectedParent, branchSHA), false
		}
		parentArchive, archiveErr := c.GetArchive(ctx, owner, repo, d.ExpectedParent)
		if archiveErr != nil {
			return nil, archiveErr, false
		}
		archive, archiveErr := c.GetArchive(ctx, owner, repo, branchSHA)
		if archiveErr != nil {
			return nil, archiveErr, false
		}
		parentFiles, decodeErr := decodeArchive(parentArchive)
		if decodeErr != nil {
			return nil, decodeErr, false
		}
		files, decodeErr := decodeArchive(archive)
		if decodeErr != nil {
			return nil, decodeErr, false
		}
		if !publishChangesExactlyMatch(parentFiles, files, d.Changes) {
			return nil, errors.New("已发布的重试提交与请求变更不完全匹配"), false
		}
		result = gh.PublishResult{CommitSHA: branchSHA, TreeSHA: info.TreeSHA, Ref: "refs/heads/" + d.Branch}
	} else {
		message := d.Message + "\n\n" + marker
		result, err = c.PublishBranch(ctx, owner, repo, d.Branch, d.ExpectedParent, message, d.Changes, d.ExpectedTree)
		if err != nil {
			return result, err, outcomeUnknown(err)
		}
	}
	if result.TreeSHA != d.ExpectedTree {
		return nil, errors.New("GitHub 返回的提交树与受信集成结果不一致"), false
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
		if err := e.completeIntegrationReceipt(ctx, x, d, result.CommitSHA, result.TreeSHA); err != nil {
			return nil, err
		}
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
			if t.Status != "INTEGRATED" && t.Status != "ACCEPTED" && t.Status != "SUPERSEDED" {
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
	if err := validatePublishIntegration(w, d); err != nil {
		return nil, err, false
	}
	if len(d.Changes) == 0 {
		return nil, errors.New("夹具发布没有变更"), false
	}
	policy := e.repository(w)
	for _, f := range d.Changes {
		if !allowedPath(f.Path, policy, []string{"**"}) {
			return nil, fmt.Errorf("不允许发布路径：%s", f.Path), false
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
	tree, treeErr := GitTreeSHA(files)
	if treeErr != nil {
		return nil, treeErr, false
	}
	if tree != d.ExpectedTree {
		return nil, errors.New("夹具候选树与集成记录不一致"), false
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
		if err := e.completeIntegrationReceipt(ctx, x, d, sha, tree); err != nil {
			return nil, err
		}
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
			if t.Status != "INTEGRATED" && t.Status != "ACCEPTED" && t.Status != "SUPERSEDED" {
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
