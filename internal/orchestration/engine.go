package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Engine struct {
	Store       *store.Store
	Sandbox     *sandbox.Manager
	mu          sync.RWMutex
	snapshots   sourceSnapshotCache
	cfg         config.Config
	app         *gh.Client
	agentActive map[string]context.CancelFunc
	active      map[string]context.CancelFunc
	owner       string
}

func New(s *store.Store, c config.Config) (*Engine, error) {
	c.Runner.WorkspaceBytes = c.EffectiveWorkspaceBytes()
	if !config.ValidWorkspaceBytes(c.Runner.WorkspaceBytes) {
		return nil, errors.New("运行器工作区容量无效")
	}
	m, e := sandbox.NewManager(sandbox.Options{RunnerImage: c.Runner.Image, WorkspaceBytes: c.Runner.WorkspaceBytes})
	if e != nil {
		return nil, e
	}
	// Configuration validation can be constructed without a store. Workflow
	// execution and remote access still require the persistent store.
	if s != nil {
		if err := s.InitializeAgentLimits(context.Background(), c.EffectiveAgentConcurrency(), c.EffectiveWorkflowAgentConcurrency()); err != nil {
			return nil, err
		}
	}
	return &Engine{Store: s, Sandbox: m, cfg: c, active: map[string]context.CancelFunc{}, owner: newControllerOwner()}, nil
}
func (e *Engine) Config() config.Config { e.mu.RLock(); defer e.mu.RUnlock(); return e.cfg }

// Capture once for each workspace lifetime. Configuration changes may install
// another manager, but existing handles remain owned by their original manager.
func (e *Engine) sandboxManager() *sandbox.Manager {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Sandbox
}
func (e *Engine) Invalidate(ctx context.Context, reason string) error {
	ws, err := e.Store.List(ctx)
	if err != nil {
		return err
	}
	for _, w := range ws {
		if w.Phase == "MERGED" || w.Phase == "INTAKE" || w.Phase == "WAITING_CONFIGURATION" {
			continue
		}
		e.stop(w.ID)
		if err = e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Phase == "MERGED" || x.Phase == "INTAKE" || x.Phase == "WAITING_CONFIGURATION" {
				return nil, store.ErrNoChange
			}
			invalidateWorkflow(x, reason)
			return []store.Action{gateAction(*x, false, reason), statusAction(*x)}, nil
		}); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) SetConfig(ctx context.Context, c config.Config) error {
	c.Runner.WorkspaceBytes = c.EffectiveWorkspaceBytes()
	if !config.ValidWorkspaceBytes(c.Runner.WorkspaceBytes) {
		return errors.New("运行器工作区容量无效")
	}
	m, err := sandbox.NewManager(sandbox.Options{RunnerImage: c.Runner.Image, WorkspaceBytes: c.Runner.WorkspaceBytes})
	if err != nil {
		return err
	}
	if err := e.Store.UpdateAgentLimits(ctx, c.EffectiveAgentConcurrency(), c.EffectiveWorkflowAgentConcurrency()); err != nil {
		return err
	}
	e.mu.Lock()
	old := e.cfg
	e.cfg = c
	e.app = nil
	if old.Runner.Image != c.Runner.Image || old.EffectiveWorkspaceBytes() != c.EffectiveWorkspaceBytes() {
		e.Sandbox = m
	}
	e.mu.Unlock()
	ws, err := e.Store.List(ctx)
	if err != nil {
		return err
	}
	quotaChanged := old.EffectiveWorkspaceBytes() != c.EffectiveWorkspaceBytes()
	changed := quotaChanged || domain.Digest(old.Repositories) != domain.Digest(c.Repositories) || old.Runner.Image != c.Runner.Image || old.Runner.Mode != c.Runner.Mode || domain.Digest(old.Model) != domain.Digest(c.Model)
	configurationReady := c.Runner.Mode != "responses" || c.ModelReady()
	for _, w := range ws {
		if w.Phase == "WAITING_CONFIGURATION" {
			_ = e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
				if x.Phase != "WAITING_CONFIGURATION" {
					return nil, store.ErrNoChange
				}
				var actions []store.Action
				if changed {
					x.Generation++
					x.Review.Verdict = "OBSOLETE"
					x.LastError = "配置已变化；恢复后重新核对状态并验证"
					actions = append(actions, gateAction(*x, false, x.LastError))
				}
				// A configuration save may release a workflow waiting for setup,
				// but it must not undo a maintainer stop or takeover.
				if x.Control != "ACTIVE" {
					if !changed {
						return nil, store.ErrNoChange
					}
					return append(actions, statusAction(*x)), nil
				}
				if !configurationReady {
					if !changed {
						return nil, store.ErrNoChange
					}
					x.LastError = "请在设置中填写 Responses 端点、Token 和开发／审查模型名称"
					return append(actions, statusAction(*x)), nil
				}
				if x.Spec.Approved {
					x.Phase = "PLANNING"
				} else {
					x.Phase = "TRIAGE"
				}
				x.LastError = ""
				return append(actions, statusAction(*x)), nil
			})
		} else if changed && w.Phase != "MERGED" && w.Phase != "INTAKE" {
			e.stop(w.ID)
			_ = e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
				if x.Phase == "MERGED" || x.Phase == "INTAKE" || x.Phase == "WAITING_CONFIGURATION" {
					return nil, store.ErrNoChange
				}
				invalidateWorkflow(x, "配置已变化；恢复后重新核对状态并验证")
				return []store.Action{gateAction(*x, false, x.LastError), statusAction(*x)}, nil
			})
		}
	}
	if old.Runner.RequireAcceptForDevelopment && !c.Runner.RequireAcceptForDevelopment {
		if err := e.releaseAcceptedSpecs(ctx); err != nil {
			return err
		}
	}
	return nil
}

// invalidateWorkflow advances the verification generation while preserving
// any control state already chosen by a maintainer. Only active work enters
// the asynchronous pause transition.
func invalidateWorkflow(w *domain.Workflow, reason string) {
	w.Generation++
	if w.Control == "ACTIVE" {
		w.Control = "PAUSING"
	}
	w.Review.Verdict = "OBSOLETE"
	w.LastError = reason
}

func (e *Engine) App() (*gh.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.app != nil {
		return e.app, nil
	}
	if e.cfg.GitHub.AppID == 0 {
		return nil, errors.New("尚未配置 GitHub App")
	}
	pem, err := os.ReadFile(e.cfg.GitHub.PrivateKeyFile)
	if err != nil {
		return nil, errors.New("GitHub App 私钥不可用")
	}
	c, err := gh.New(gh.Config{AppID: fmt.Sprint(e.cfg.GitHub.AppID), PrivateKeyPEM: pem, BaseURL: e.cfg.GitHub.APIURL})
	if err == nil {
		e.app = c
	}
	return c, err
}
func (e *Engine) client(ctx context.Context, w domain.Workflow) (*gh.InstallationClient, string, string, error) {
	if e.Store == nil {
		return nil, "", "", errors.New("外部访问需要持久化工作流存储")
	}
	retired, err := e.Store.IsRetiredWorkflow(ctx, w.ID)
	if err != nil {
		return nil, "", "", err
	}
	if retired {
		return nil, "", "", errors.New("此工作流已随仓库试点重置退役，禁止恢复外部写入")
	}
	a, err := e.App()
	if err != nil {
		return nil, "", "", err
	}
	parts := strings.Split(w.Repository, "/")
	if len(parts) != 2 {
		return nil, "", "", errors.New("仓库名称无效")
	}
	return a.ForInstallation(ctx, w.InstallationID), parts[0], parts[1], nil
}
func (e *Engine) Run(ctx context.Context) {
	if err := e.releaseAcceptedSpecs(ctx); err != nil {
		slog.Warn("release AI-approved requirements", "error", err)
	}
	go e.inboxLoop(ctx)
	go e.outboxLoop(ctx)
	go e.labelLoop(ctx)
	go e.reconcileLoop(ctx)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.resumeDailyBudgetWorkflows(ctx)
				if err := e.RecoverAgents(ctx); err != nil {
					slog.Warn("回收已停止的 Agent", "error", err)
				}
				if err := e.recoverWaitingCI(ctx); err != nil {
					slog.Warn("恢复可信 CI 等待", "error", err)
				}
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.mu.Lock()
			for _, cancel := range e.active {
				cancel()
			}
			e.mu.Unlock()
			return
		case <-ticker.C:
			e.settleControls(ctx)
			workflowLimit := e.Config().Runner.WorkflowConcurrency
			if workflowLimit == 0 {
				workflowLimit = 2
			}
			w, err := e.Store.ClaimWithLimit(ctx, e.owner, workflowLimit)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				slog.Error("claim workflow", "error", err)
				continue
			}
			runCtx, cancel := context.WithCancel(ctx)
			e.mu.Lock()
			if _, exists := e.active[w.ID]; exists {
				e.mu.Unlock()
				cancel()
				e.Store.Release(ctx, w)
				continue
			}
			e.active[w.ID] = cancel
			e.mu.Unlock()
			go e.execute(runCtx, w, cancel)
		}
	}
}
func (e *Engine) execute(ctx context.Context, w domain.Workflow, cancel context.CancelFunc) {
	// Renewal must never concurrently copy the Workflow mutated by the pipeline.
	lease := domain.Workflow{ID: w.ID, Generation: w.Generation, LeaseOwner: w.LeaseOwner}
	defer func() {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 60*time.Second)
		cleanupErr := e.waitWorkflowAgentCleanup(cleanupCtx, w.ID)
		cleanupCancel()
		persistCtx, persistCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer persistCancel()
		if _, err := e.Store.AgentRuntimeSeconds(persistCtx, w.ID); err != nil {
			slog.Error("persist agent runtime", "workflow_id", w.ID, "error", err)
		}
		if cleanupErr == nil {
			e.Store.Release(persistCtx, lease)
		} else {
			slog.Error("agent cleanup pending; workflow lease not released", "workflow_id", w.ID, "error", cleanupErr)
		}
		e.mu.Lock()
		delete(e.active, w.ID)
		e.mu.Unlock()
	}()
	if err := e.checkEvaluationConfig(ctx, w); err != nil {
		e.fail(ctx, &w, err)
		return
	}
	runtime, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil {
		e.fail(ctx, &w, fmt.Errorf("读取持久化执行预算失败：%w", err))
		return
	}
	w.RuntimeSeconds = runtime
	cfg := e.Config()
	if runtime >= int64(cfg.Budget.IssueMinutes)*60 {
		e.fail(ctx, &w, errors.New("工作流累计执行时间额度已耗尽"))
		return
	}
	monitorErrors := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				used, err := e.Store.AgentRuntimeSeconds(ctx, lease.ID)
				if err == nil && used >= int64(e.Config().Budget.IssueMinutes)*60 {
					err = errors.New("工作流累计执行时间额度已耗尽")
				}
				if err != nil {
					monitorErrors <- err
					cancel()
					return
				}
				if e.Store.Renew(ctx, lease) != nil {
					cancel()
					return
				}
			}
		}
	}()
	if cfg.Runner.Mode == "responses" && !cfg.ModelReady() {
		w.Phase = "WAITING_CONFIGURATION"
		w.LastError = "请在设置中填写 Responses 端点、Token 和开发／审查模型名称"
		_ = e.Store.Save(ctx, &w, statusAction(w))
		return
	}
	switch w.Phase {
	case "TRIAGE":
		err = e.triage(ctx, &w)
	case "PLANNING", "CHANGES_REQUESTED":
		err = e.plan(ctx, &w)
	case "DEVELOPING":
		err = e.build(ctx, &w)
	case "VERIFYING":
		err = e.verify(ctx, &w)
	case "REVIEWING":
		err = e.review(ctx, &w)
	}
	select {
	case monitorErr := <-monitorErrors:
		err = monitorErr
	default:
	}
	if err != nil && !errors.Is(err, store.ErrStale) && !errors.Is(err, context.Canceled) {
		e.fail(ctx, &w, err)
	}
}
func (e *Engine) fail(ctx context.Context, w *domain.Workflow, err error) {
	w.LastError = e.redact(err.Error())
	if errors.Is(err, store.ErrDailyBudget) {
		w.ResumePhase = w.Phase
		w.Phase = "WAITING_DAILY_BUDGET"
		w.BudgetDeferredAt = time.Now().UTC()
		w.LastError = "当日模型调用额度已用完；保留任务与累计预算，次日继续"
		_ = e.Store.Save(context.WithoutCancel(ctx), w, statusAction(*w))
		e.Store.Audit(context.Background(), w.ID, "daily_budget_checkpoint", map[string]any{"resume_phase": w.ResumePhase, "model_calls": w.ModelCalls})
		return
	}
	w.Phase = "BLOCKED"
	w.Review.Verdict = "BLOCKED"
	_ = e.Store.Save(context.WithoutCancel(ctx), w, gateAction(*w, false, w.LastError), statusAction(*w))
	e.Store.Audit(context.Background(), w.ID, "blocked", map[string]string{"reason": w.LastError})
}
func (e *Engine) redact(s string) string {
	cfg := e.Config()
	for _, p := range []string{cfg.Model.TokenFile, cfg.GitHub.WebhookSecretFile} {
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 4 {
			s = strings.ReplaceAll(s, strings.TrimSpace(string(b)), "[redacted]")
		}
	}
	if len(s) > 24000 {
		s = s[:24000] + "\n[truncated]"
	}
	return s
}
func (e *Engine) stop(id string) {
	e.mu.RLock()
	cancel := e.active[id]
	e.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}
func (e *Engine) settleControls(ctx context.Context) {
	ws, err := e.Store.List(ctx)
	if err != nil {
		return
	}
	for _, w := range ws {
		if w.Control != "PAUSING" && w.Control != "CANCELLING" {
			continue
		}
		e.mu.RLock()
		_, running := e.active[w.ID]
		e.mu.RUnlock()
		runs, queueErr := e.Store.ListAgentRuns(ctx, w.ID)
		if queueErr != nil {
			continue
		}
		for _, run := range runs {
			if run.Status == domain.AgentActive || run.Status == domain.AgentStopping {
				running = true
			}
		}
		n, err := e.Store.PendingWrites(ctx, w.ID)
		if running || err != nil || n > 0 {
			continue
		}
		_ = e.Store.Update(ctx, w.ID, func(x *domain.Workflow) ([]store.Action, error) {
			if x.Control == "PAUSING" {
				x.Control = "PAUSED"
			} else if x.Control == "CANCELLING" {
				x.Control = "CANCELLED"
			}
			return []store.Action{statusAction(*x)}, nil
		})
	}
}
func (e *Engine) ask(ctx context.Context, w domain.Workflow, role agent.RoleID, input string, tools []agent.Tool, schema json.RawMessage, validators ...func(json.RawMessage) error) (json.RawMessage, error) {
	cfg := e.Config()
	execution, ok := ctx.Value(executionContextKey{}).(*agentExecution)
	if !ok || execution.role != role {
		return nil, errors.New("模型调用缺少匹配角色的有效 Agent 租约")
	}
	if err := execution.check(ctx); err != nil {
		return nil, err
	}
	definition, err := agent.LookupRole(role)
	if err != nil {
		return nil, err
	}
	liveConfig, err := e.ExecutionConfig(ctx)
	if err != nil {
		return nil, err
	}
	promptContext := execution.promptContext(tools)
	promptContext.Concurrency = agent.PromptConcurrency{Global: liveConfig.EffectiveAgentConcurrency(), Workflow: liveConfig.EffectiveWorkflowAgentConcurrency()}
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	remaining := max(0, cfg.Budget.IssueCalls-current.ModelCalls)
	if remaining == 0 {
		return nil, store.ErrIssueBudget
	}
	daily, err := e.Store.DailyRequestRemaining(ctx, cfg.Budget.DailyCalls)
	if err != nil {
		return nil, err
	}
	if daily == 0 {
		return nil, store.ErrDailyBudget
	}
	runtime, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	if runtime >= int64(cfg.Budget.IssueMinutes)*60 {
		return nil, errors.New("工作流累计执行时间额度已耗尽")
	}
	promptContext.Budget = agent.PromptBudget{MaxCalls: remaining, MaxRequests: remaining, RemainingTokens: int64(remaining) * int64(cfg.Model.MaxOutputTokens), TokenBudgetKind: "剩余调用的最大输出 token 估计上界；实际调用、时长和当日额度由控制器另行限制", DailyRemainingRequests: daily, IssueRemainingSeconds: max(0, int64(cfg.Budget.IssueMinutes*60)-runtime), RemainingTasks: max(0, cfg.EffectiveMaxTasksPerWorkflow()-current.TasksCreated), RemainingEvidenceRequests: max(0, cfg.EffectiveMaxEvidenceRequests()-len(current.EvidenceRequests))}
	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		if run.Generation != current.Generation {
			continue
		}
		if run.Status != domain.AgentActive && run.Status != domain.AgentStopping {
			continue
		}
		for i := range promptContext.OtherTasks {
			if promptContext.OtherTasks[i].TaskID == run.TaskID {
				promptContext.OtherTasks[i].Status = run.Status
			}
		}
	}
	system, err := agent.RenderPrompt(role, promptContext)
	if err != nil {
		return nil, err
	}
	token, err := os.ReadFile(cfg.Model.TokenFile)
	if err != nil {
		return nil, errors.New("模型 Token 文件不可用")
	}
	model := cfg.Model.Dev
	if definition.ModelClass == agent.ModelClassReview {
		model = cfg.Model.Review
	}
	runner, err := agent.NewRunner(agent.Config{BaseURL: cfg.Model.BaseURL, Token: strings.TrimSpace(string(token)), Model: model, Stream: cfg.Model.Stream, MaxOutputTokens: cfg.Model.MaxOutputTokens})
	if err != nil {
		return nil, err
	}
	validators = append([]func(json.RawMessage) error{chineseReportValidator(role)}, validators...)
	validate := func(report json.RawMessage) error {
		if err := execution.check(ctx); err != nil {
			return err
		}
		for _, validator := range validators {
			if validator != nil {
				if err := validator(report); err != nil {
					return err
				}
			}
		}
		return nil
	}
	res, err := runner.Run(ctx, agent.Request{Role: role, PromptVersion: agent.PromptVersion, System: system, Input: raw(input), Model: model, Tools: tools, ReportSchema: schema, MaxCalls: remaining, MaxRequests: remaining, ValidateReport: validate, BeforeRequest: func(c context.Context) error {
		if err := execution.check(c); err != nil {
			return err
		}
		if err := e.checkEvaluationConfig(c, w); err != nil {
			return err
		}
		live := e.Config()
		used, err := e.Store.AgentRuntimeSeconds(c, w.ID)
		if err != nil {
			return err
		}
		if used >= int64(live.Budget.IssueMinutes)*60 {
			return errors.New("工作流累计执行时间额度已耗尽")
		}
		return e.Store.ReserveRequest(c, w.ID, w.Generation, live.Budget.IssueCalls, live.Budget.DailyCalls)
	}, OnUsage: func(c context.Context, u agent.Usage) error {
		usageCtx, cancel := context.WithTimeout(context.WithoutCancel(c), 15*time.Second)
		defer cancel()
		return e.Store.Usage(usageCtx, w.ID, u.InputTokens, u.OutputTokens)
	}})
	e.Store.Audit(context.WithoutCancel(ctx), w.ID, "model_run", map[string]any{"role": role, "model_class": definition.ModelClass, "prompt_version": agent.PromptVersion, "agent_run_id": execution.run.ID, "context_hash": execution.run.ContextHash, "requests": res.Requests, "usage": res.Usage, "error": errString(err)})
	if err == nil && len(res.Report) > 0 {
		e.Store.Audit(context.WithoutCancel(ctx), w.ID, "model_report", map[string]any{"generation": w.Generation, "role": role, "agent_run_id": execution.run.ID, "report": json.RawMessage(e.redact(string(res.Report)))})
	}
	return res.Report, err
}
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func (e *Engine) imageDigest(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", e.Config().Runner.Image).Output()
	if err != nil {
		return "image-unavailable"
	}
	return strings.TrimSpace(string(out))
}
func (e *Engine) key(ctx context.Context, w domain.Workflow) string {
	cfg := e.Config()
	r, _ := cfg.Repository(w.Repository)
	// Resource limits are part of the verification profile: a pass obtained
	// under another workspace capacity cannot authorize this configuration.
	profile := domain.Digest([]any{r.Verification, r.VerificationServices, cfg.EffectiveWorkspaceBytes(), agent.PromptVersion})
	return domain.VerificationKey(w, domain.Digest(r), profile, e.imageDigest(ctx))
}
func (e *Engine) source(ctx context.Context, w domain.Workflow, sha string) (map[string]sandbox.File, error) {
	if w.Repository == "local/pilot" {
		directory := e.Config().DataDir + "/fixtures/" + domain.Digest(w.ID)[:24]
		if _, err := os.Stat(directory); errors.Is(err, os.ErrNotExist) && (w.HeadSHA == "" || w.HeadSHA == "local-fixture") {
			return localFiles(), nil
		}
		return loadLocalSnapshot(directory)
	}
	if sha == "" {
		return nil, errors.New("源码快照必须绑定精确提交 SHA")
	}
	key := w.Repository + "\x00" + sha
	return e.snapshots.get(ctx, key, func() (map[string]sandbox.File, error) {
		c, owner, repo, err := e.client(ctx, w)
		if err != nil {
			return nil, err
		}
		var archive []byte
		for attempt := 0; ; attempt++ {
			archive, err = c.GetArchive(ctx, owner, repo, sha)
			if err == nil || attempt >= 2 || !retryableArchiveRead(ctx, err) {
				break
			}
			if err := waitArchiveRetry(ctx, time.Duration(attempt+1)*time.Second); err != nil {
				return nil, err
			}
		}
		if err != nil {
			return nil, err
		}
		return decodeArchive(archive)
	})
}
func localFiles() map[string]sandbox.File {
	return map[string]sandbox.File{"go.mod": {Data: []byte("module pilot\n\ngo 1.24.0\n"), Mode: 0644}, "calculator.go": {Data: []byte("package pilot\n\nfunc Add(a, b int) int { return a - b }\n"), Mode: 0644}, "calculator_test.go": {Data: []byte("package pilot\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2,3)!=5 { t.Fatal(\"Add must add\") } }\n"), Mode: 0644}}
}
func (e *Engine) baseSnapshot(ctx context.Context, w domain.Workflow) (map[string]sandbox.File, error) {
	if w.Repository == "local/pilot" {
		return localFiles(), nil
	}
	return e.source(ctx, w, w.BaseSHA)
}
func (e *Engine) repository(w domain.Workflow) config.Repository {
	if w.Repository == "local/pilot" {
		return config.Repository{Name: w.Repository, AllowedPaths: []string{"**"}, ProtectedPaths: []string{".git/**", ".github/**"}, Verification: [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}, PolicyVersion: "fixture-v1"}
	}
	r, _ := e.Config().Repository(w.Repository)
	return r
}
func (e *Engine) artifact(ctx context.Context, w domain.Workflow, id, kind string, data any) (domain.Evidence, error) {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return domain.Evidence{}, fmt.Errorf("编码证据失败：%w", err)
	}
	directory := e.Config().DataDir + "/evidence/" + w.ID
	if err := os.MkdirAll(directory, 0700); err != nil {
		return domain.Evidence{}, err
	}
	file := directory + "/" + domain.Digest([]any{id, data}) + ".json"
	if err := os.WriteFile(file, b, 0600); err != nil {
		return domain.Evidence{}, err
	}
	createdAt := time.Now().UTC()
	if e.Store != nil {
		// Agent admission uses the database clock. Evidence must use the same
		// clock: a controller slightly behind PostgreSQL otherwise produces
		// apparently pre-admission evidence even after the tool has completed.
		if err := e.Store.DB.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
			return domain.Evidence{}, fmt.Errorf("读取证据记录时间失败：%w", err)
		}
	}
	ev := domain.Evidence{ID: id, Kind: kind, Path: file, Hash: domain.Digest(data), CreatedAt: createdAt, BaseSHA: w.BaseSHA, HeadSHA: w.HeadSHA, ProfileDigest: domain.Digest([]any{e.repository(w).Verification, e.repository(w).VerificationServices, e.Config().EffectiveWorkspaceBytes(), agent.PromptVersion})}
	return ev, nil
}
func (e *Engine) Demo(ctx context.Context) (domain.Workflow, error) {
	if e.Config().Runner.Mode != "fake" {
		return domain.Workflow{}, errors.New("启动本地夹具前，请在设置中选择 Fake 运行器")
	}
	issue := int(time.Now().Unix())
	w := domain.Workflow{ID: domain.ID(-1, issue), RepositoryID: -1, Repository: "local/pilot", Issue: issue, InstallationID: 0, IssueTitle: "Fixture: repair Add", IssueBody: "Fix Add so it adds two integers. Preserve tests and exported function signature.", AuthorizedBy: "local-dashboard", Phase: "TRIAGE", Control: "ACTIVE", BaseBranch: "main", BaseSHA: "local-fixture", Branch: fmt.Sprintf("codex/issue-%d", issue)}
	err := e.Store.Create(ctx, w)
	if err != nil {
		return w, err
	}
	return e.Store.Get(ctx, w.ID)
}
