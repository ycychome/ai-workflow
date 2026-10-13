package orchestration

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

type executionContextKey struct{}
type clockContextKey struct{}

// executionClock counts the union of active execution intervals. Waiting for
// queue admission never starts this clock and never consumes an attempt timeout.
type executionClock struct {
	mu      sync.Mutex
	active  int
	started time.Time
	elapsed time.Duration
}

func (c *executionClock) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == 0 {
		c.started = time.Now()
	}
	c.active++
}
func (c *executionClock) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active > 0 {
		c.active--
		if c.active == 0 {
			c.elapsed += time.Since(c.started)
		}
	}
}
func (c *executionClock) seconds() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.elapsed
	if c.active > 0 {
		d += time.Since(c.started)
	}
	return int64(d.Seconds())
}

type agentExecution struct {
	e         *Engine
	run       domain.AgentRun
	task      domain.Task
	w         domain.Workflow
	role      agent.RoleID
	ctx       context.Context
	cancel    context.CancelFunc
	manager   *sandbox.Manager
	clock     *executionClock
	once      sync.Once
	finishErr error
	queued    bool
}

func newControllerOwner() string {
	host, _ := os.Hostname()
	kind := "local"
	if _, err := os.Stat("/.dockerenv"); err == nil {
		kind = "docker"
	}
	return fmt.Sprintf("%s/%d/%d/%s", host, os.Getpid(), time.Now().UnixNano(), kind)
}

func taskRole(t domain.Task, review bool) agent.RoleID {
	if isEvidenceTask(t) && (t.Role == "reviewer" || t.Role == "verifier") {
		return agent.RoleID(t.Role)
	}
	if t.ID == "skeptic" && review {
		return agent.RoleSkeptic
	}
	if review {
		return agent.RoleReviewer
	}
	switch taskKind(t) {
	case "analysis":
		return agent.RoleAnalyst
	case "verification":
		return agent.RoleVerifier
	default:
		return agent.RoleBuilder
	}
}
func frozenReview(role agent.RoleID, task domain.Task) bool {
	return roleIsReview(role) || isEvidenceTask(task)
}
func roleIsReview(role agent.RoleID) bool {
	r, err := agent.LookupRole(role)
	return err == nil && r.ModelClass == agent.ModelClassReview
}
func planRevision(w domain.Workflow, review bool) int {
	if review {
		return max(1, w.Review.PlanRevision)
	}
	return max(1, w.PlanRevision)
}
func executionHash(w domain.Workflow, task domain.Task, role agent.RoleID) string {
	return domain.AgentExecutionHash(w, task, string(role), agent.PromptVersion)
}

func (e *Engine) beginAgent(ctx context.Context, expected domain.Workflow, task domain.Task, role agent.RoleID) (*agentExecution, error) {
	if _, err := agent.LookupRole(role); err != nil {
		return nil, err
	}
	run := domain.AgentRun{ID: "run-" + domain.Digest([]any{expected.ID, expected.Generation, task.ID, task.LeaseEpoch, e.owner, time.Now().UnixNano()})[:32], WorkflowID: expected.ID, Owner: e.owner, TaskID: task.ID, Role: string(role), PromptVersion: agent.PromptVersion, StartReason: "新任务启动前须读取最新项目状态", Generation: expected.Generation, PlanRevision: planRevision(expected, frozenReview(role, task))}
	if err := e.Store.EnqueueAgent(ctx, run); err != nil {
		return nil, err
	}
	queued := false
	for {
		cfg := e.Config()
		claimed, err := e.Store.ClaimAgent(ctx, run.ID, e.owner, cfg.EffectiveAgentConcurrency(), cfg.EffectiveWorkflowAgentConcurrency())
		if err == nil {
			run = claimed
			break
		}
		if !errors.Is(err, store.ErrAgentQueued) {
			_ = e.Store.CancelQueuedAgent(context.WithoutCancel(ctx), run.ID)
			return nil, err
		}
		queued = true
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = e.Store.CancelQueuedAgent(context.WithoutCancel(ctx), run.ID)
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	usedSeconds, budgetErr := e.Store.AgentRuntimeSeconds(ctx, expected.ID)
	remainingTime := time.Duration(int64(e.Config().Budget.IssueMinutes)*60-usedSeconds) * time.Second
	if budgetErr == nil && remainingTime <= 0 {
		budgetErr = errors.New("工作流累计执行时间额度已耗尽")
	}
	runCtx, cancel := context.WithTimeout(ctx, max(time.Nanosecond, min(e.Config().AttemptTimeout(), remainingTime)))
	x := &agentExecution{e: e, run: run, task: task, role: role, cancel: cancel, manager: e.sandboxManager(), queued: queued}
	x.clock, _ = ctx.Value(clockContextKey{}).(*executionClock)
	if x.clock != nil {
		x.clock.start()
	}
	e.mu.Lock()
	if e.agentActive == nil {
		e.agentActive = map[string]context.CancelFunc{}
	}
	e.agentActive[run.ID] = cancel
	e.mu.Unlock()
	x.ctx = runCtx
	failed := true
	defer func() {
		if failed {
			_ = x.finish()
		}
	}()
	if budgetErr != nil {
		return nil, budgetErr
	}
	current, err := e.Store.Get(runCtx, expected.ID)
	if err != nil {
		return nil, err
	}
	if current.Generation != expected.Generation || current.Control != "ACTIVE" || current.LeaseOwner != expected.LeaseOwner || current.Spec.Digest != expected.Spec.Digest || planRevision(current, frozenReview(role, task)) != run.PlanRevision {
		return nil, store.ErrStale
	}
	if frozenReview(role, task) && (current.Review.Key != expected.Review.Key || current.HeadSHA != expected.HeadSHA || current.TestedSHA != expected.TestedSHA || current.TestedTreeSHA != expected.TestedTreeSHA) {
		return nil, store.ErrStale
	}
	if role == agent.RoleBuilder || role == agent.RoleAnalyst || role == agent.RoleVerifier || role == agent.RoleReviewer || role == agent.RoleSkeptic {
		tasks := current.Tasks
		if frozenReview(role, task) {
			tasks = current.Review.Tasks
		}
		found := false
		for _, candidate := range tasks {
			if candidate.ID == task.ID {
				if candidate.Status == "SUPERSEDED" || candidate.LeaseEpoch != task.LeaseEpoch {
					return nil, store.ErrStale
				}
				x.task = candidate
				found = true
			}
		}
		// The controller verification profile is not a planner-created task.
		if !found && task.ID != "trusted-verification" {
			return nil, fmt.Errorf("任务 %s 已不在当前计划中", task.ID)
		}
		for _, dep := range x.task.DependsOn {
			ready := false
			for _, candidate := range tasks {
				if candidate.ID == dep && (candidate.Status == "INTEGRATED" || candidate.Status == "ACCEPTED") {
					ready = true
				}
			}
			if !ready {
				return nil, fmt.Errorf("排队后启动检查失败：依赖 %s 尚未完成", dep)
			}
		}
	}
	// Resolve source identity only after admission; the subsequent workspace is
	// created from this newly read state, never from the enqueue-time snapshot.
	if current.BaseSHA == "" && current.Repository != "local/pilot" {
		c, o, r, err := e.client(runCtx, current)
		if err != nil {
			return nil, err
		}
		current.BaseSHA, err = c.GetBranchSHA(runCtx, o, r, current.BaseBranch)
		if err != nil {
			return nil, err
		}
		if err = e.Store.Save(runCtx, &current); err != nil {
			return nil, err
		}
	}
	x.w = current
	x.run.ContextHash = executionHash(current, x.task, role)
	if err = e.Store.BindAgentContext(runCtx, run.ID, run.Owner, run.LeaseEpoch, x.run.ContextHash); err != nil {
		return nil, err
	}
	x.ctx = store.WithAgentFence(context.WithValue(runCtx, executionContextKey{}, x), x.run)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if e.Store.RenewAgent(runCtx, run.ID, run.Owner, run.LeaseEpoch) != nil {
					cancel()
					return
				}
			}
		}
	}()
	failed = false
	return x, nil
}

func (x *agentExecution) check(ctx context.Context) error {
	if err := x.e.Store.CheckAgentLease(ctx, x.run.ID, x.run.Owner, x.run.LeaseEpoch); err != nil {
		return err
	}
	w, err := x.e.Store.Get(ctx, x.run.WorkflowID)
	if err != nil {
		return err
	}
	tasks := w.Tasks
	if frozenReview(x.role, x.task) {
		tasks = w.Review.Tasks
	}
	for _, task := range tasks {
		if task.ID == x.task.ID && (task.Status == "SUPERSEDED" || task.LeaseEpoch != x.task.LeaseEpoch || domain.Digest([]any{task.Paths, task.DependsOn, task.ObligationIDs}) != domain.Digest([]any{x.task.Paths, x.task.DependsOn, x.task.ObligationIDs})) {
			return store.ErrStale
		}
	}
	if executionHash(w, x.task, x.role) != x.run.ContextHash {
		return store.ErrStale
	}
	return nil
}
func (x *agentExecution) finish() error {
	x.once.Do(func() {
		defer func() {
			if x.clock != nil {
				x.clock.stop()
			}
			x.e.mu.Lock()
			delete(x.e.agentActive, x.run.ID)
			x.e.mu.Unlock()
		}()
		x.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := x.e.Store.MarkAgentStopping(ctx, x.run.ID, x.run.Owner, x.run.LeaseEpoch); err != nil {
			x.finishErr = err
			return
		}
		// Positive resource-stop acknowledgement is mandatory before releasing capacity.
		if err := x.manager.StopRun(ctx, x.run.ID); err != nil {
			x.finishErr = err
			return
		}
		x.finishErr = x.e.Store.FinishAgent(ctx, x.run.ID, x.run.Owner, x.run.LeaseEpoch)
		if x.finishErr == nil {
			x.e.Store.Audit(ctx, x.run.WorkflowID, "agent_stopped", map[string]any{"agent_run_id": x.run.ID, "lease_epoch": x.run.LeaseEpoch})
		}
	})
	return x.finishErr
}

// RecoverAgents revokes expired leases but does not confuse lease expiry with
// process exit. A live local request must acknowledge cancellation itself.
func (e *Engine) RecoverAgents(ctx context.Context) error {
	runs, err := e.Store.OrphanAgentRuns(ctx)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	for _, run := range runs {
		e.mu.RLock()
		cancel, live := e.agentActive[run.ID]
		e.mu.RUnlock()
		if live {
			cancel()
			continue
		}
		parts := strings.Split(run.Owner, "/")
		if len(parts) != 4 && len(parts) != 3 {
			continue
		}
		stopped := false
		if len(parts) == 4 && parts[3] == "docker" {
			stopped = controllerContainerStopped(ctx, parts[0])
		} else if parts[0] == host {
			pid, parseErr := strconv.Atoi(parts[1])
			if parseErr != nil || pid <= 0 {
				continue
			}
			stopped = pid == os.Getpid() || errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		}
		if !stopped {
			continue
		}
		if err := e.sandboxManager().StopRun(ctx, run.ID); err != nil {
			return err
		}
		if err := e.Store.FinishAgent(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
			return err
		}
	}
	return e.recoverCIExecutors(ctx)
}

func (x *agentExecution) promptContext(tools []agent.Tool) agent.PromptContext {
	cfg := x.e.Config()
	remaining := max(0, cfg.Budget.IssueCalls-x.w.ModelCalls)
	capabilities := make([]string, 0, len(tools))
	capabilities = append(capabilities, "submit_report")
	for _, tool := range tools {
		capabilities = append(capabilities, tool.Name)
	}
	others := []agent.TaskStateSummary{}
	for _, task := range x.w.Tasks {
		if task.ID != x.task.ID {
			others = append(others, agent.TaskStateSummary{TaskID: task.ID, Role: taskRole(task, false), Status: task.Status})
		}
	}
	for _, task := range x.w.Review.Tasks {
		if task.ID != x.task.ID {
			others = append(others, agent.TaskStateSummary{TaskID: task.ID, Role: taskRole(task, true), Status: task.Status})
		}
	}
	startReason := "新任务启动；先读取 workflow_state 和项目源码，再提交 submit_preflight"
	var queuedAt *time.Time
	queueReason := ""
	if x.queued || x.run.QueueReason != "" {
		queuedAt = &x.run.EnqueuedAt
		queueReason = "曾等待活跃名额或公平调度"
		startReason = "本 Agent 排队后接替运行；必须重新检查当前项目状态、依赖与代码，不能沿用入队前假设"
	}
	if x.task.Attempts > 1 {
		startReason += "；这是重试，须检查上次失败原因"
	}
	checkpoint := "首次执行；尚无本任务已接受检查点"
	if x.task.AgentRunID != "" || len(x.task.Report) > 0 {
		checkpoint = fmt.Sprintf("前次运行 %s；报告摘要 %s。历史报告仅作不可信参考，必须重查当前项目", x.task.AgentRunID, domain.Digest(x.task.Report))
	}
	if !x.w.BudgetDeferredAt.IsZero() {
		startReason += "；此工作流曾因当日额度暂停，累计调用与时长没有重置"
	}
	deadline, _ := x.ctx.Deadline()
	snapshot := x.w.HeadSHA
	if snapshot == "" {
		snapshot = x.w.BaseSHA
	}
	if frozenReview(x.role, x.task) {
		snapshot = x.w.TestedSHA
	}
	if snapshot == "" {
		snapshot = "尚未绑定代码快照"
	}
	repo := x.e.repository(x.w)
	return agent.PromptContext{Repository: x.w.Repository, RepositoryID: x.w.RepositoryID, TestedCommitSHA: x.w.TestedSHA, TestedTreeSHA: x.w.TestedTreeSHA, EvaluationProfile: evaluationProfile(x.w), Checkpoint: checkpoint, Issue: x.w.Issue, PR: x.w.PR, AgentRunID: x.run.ID, LeaseEpoch: x.run.LeaseEpoch, StartedAt: &x.run.StartedAt, Deadline: &deadline, Workspace: "/workspace", SpecDigest: x.w.Spec.Digest, BaseSHA: x.w.BaseSHA, HeadSHA: x.w.HeadSHA, VerificationKey: x.w.Review.Key, AllowedPaths: append([]string{}, x.task.Paths...), ProtectedPaths: append([]string{}, repo.ProtectedPaths...), Concurrency: agent.PromptConcurrency{Global: cfg.EffectiveAgentConcurrency(), Workflow: cfg.EffectiveWorkflowAgentConcurrency()}, WorkflowID: x.w.ID, TaskID: x.task.ID, Attempt: max(1, x.task.Attempts), Generation: int(x.w.Generation), PlanRevision: x.run.PlanRevision, QueueReason: queueReason, QueuedAt: queuedAt, StartReason: startReason, Versions: agent.PromptVersions{Prompt: agent.PromptVersion, Policy: domain.Digest(repo), Snapshot: snapshot}, Budget: agent.PromptBudget{MaxCalls: remaining, MaxRequests: max(1, remaining), RemainingTokens: int64(remaining) * int64(cfg.Model.MaxOutputTokens)}, Capabilities: capabilities, Dependencies: append([]string{}, x.task.DependsOn...), OtherTasks: others}
}

func chineseReportValidator(role agent.RoleID) func(json.RawMessage) error {
	return func(report json.RawMessage) error {
		paths := []string{"summary"}
		switch role {
		case agent.RoleTriage:
			paths = []string{"title", "description", "criteria.*.description", "non_goals.*", "questions.*"}
		case agent.RoleDevPlanner:
			paths = []string{"tasks.*.description", "reason"}
		case agent.RoleReviewPlanner:
			paths = []string{"tasks.*.description", "obligations.*.description", "evidence_requests.*.question"}
		case agent.RoleReviewer, agent.RoleSkeptic:
			paths = append(paths, "findings.*.description", "findings.*.expected", "findings.*.actual", "findings.*.preconditions", "evidence_requests.*.question")
		case agent.RoleBuilder:
			paths = append(paths, "rebuttals.*.argument")
		}
		return agent.ValidateChineseProse(report, paths...)
	}
}

// A different container hostname is not proof of death. Query the same Docker
// daemon managing workspaces, and retain capacity if its answer is uncertain.
func controllerContainerStopped(ctx context.Context, id string) bool {
	if len(id) != 12 && len(id) != 64 {
		return false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return false
	}
	output, err := exec.CommandContext(ctx, "docker", "container", "ls", "-aq", "--no-trunc", "--filter", "id="+id).Output()
	if err != nil {
		return false
	}
	ids := strings.Fields(string(output))
	if len(ids) == 0 {
		return true
	}
	if len(ids) != 1 || !strings.HasPrefix(ids[0], id) {
		return false
	}
	state, err := exec.CommandContext(ctx, "docker", "container", "inspect", "--format", "{{.State.Running}}", ids[0]).Output()
	return err == nil && strings.TrimSpace(string(state)) == "false"
}
