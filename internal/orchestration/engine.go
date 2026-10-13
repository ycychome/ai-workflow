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
	Store         *store.Store
	Sandbox       *sandbox.Manager
	mu            sync.RWMutex
	snapshots     sourceSnapshotCache
	builderSlots  roleLimiter
	reviewerSlots roleLimiter
	cfg           config.Config
	app           *gh.Client
	active        map[string]context.CancelFunc
	owner         string
}

func New(s *store.Store, c config.Config) (*Engine, error) {
	c.Runner.WorkspaceBytes = c.EffectiveWorkspaceBytes()
	if !config.ValidWorkspaceBytes(c.Runner.WorkspaceBytes) {
		return nil, errors.New("invalid runner workspace capacity")
	}
	m, e := sandbox.NewManager(sandbox.Options{RunnerImage: c.Runner.Image, WorkspaceBytes: c.Runner.WorkspaceBytes})
	if e != nil {
		return nil, e
	}
	return &Engine{Store: s, Sandbox: m, cfg: c, active: map[string]context.CancelFunc{}, owner: fmt.Sprintf("control-%d-%d", os.Getpid(), time.Now().UnixNano())}, nil
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
		return errors.New("invalid runner workspace capacity")
	}
	m, err := sandbox.NewManager(sandbox.Options{RunnerImage: c.Runner.Image, WorkspaceBytes: c.Runner.WorkspaceBytes})
	if err != nil {
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
					x.LastError = "configuration changed; resume to reconcile and reverify"
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
					x.LastError = "fill in Responses endpoint, Token and Dev/Review model names in Settings"
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
				invalidateWorkflow(x, "configuration changed; resume to reconcile and reverify")
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
		return nil, errors.New("GitHub App is not configured")
	}
	pem, err := os.ReadFile(e.cfg.GitHub.PrivateKeyFile)
	if err != nil {
		return nil, errors.New("GitHub App private key is unavailable")
	}
	c, err := gh.New(gh.Config{AppID: fmt.Sprint(e.cfg.GitHub.AppID), PrivateKeyPEM: pem, BaseURL: e.cfg.GitHub.APIURL})
	if err == nil {
		e.app = c
	}
	return c, err
}
func (e *Engine) client(ctx context.Context, w domain.Workflow) (*gh.InstallationClient, string, string, error) {
	a, err := e.App()
	if err != nil {
		return nil, "", "", err
	}
	parts := strings.Split(w.Repository, "/")
	if len(parts) != 2 {
		return nil, "", "", errors.New("invalid repository name")
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
	started := time.Now()
	defer func() {
		cancel()
		e.Store.Runtime(context.Background(), w.ID, int64(time.Since(started).Seconds()))
		e.Store.Release(context.Background(), w)
		e.mu.Lock()
		delete(e.active, w.ID)
		e.mu.Unlock()
	}()
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if metrics, err := e.Store.Get(ctx, w.ID); err == nil && metrics.RuntimeSeconds+int64(time.Since(started).Seconds()) >= int64(e.Config().Budget.IssueMinutes*60) {
					cancel()
					return
				}
				if e.Store.Renew(ctx, w) != nil {
					cancel()
					return
				}
			}
		}
	}()
	cfg := e.Config()
	remaining := cfg.Budget.IssueMinutes*60 - int(w.RuntimeSeconds)
	if remaining <= 0 {
		e.fail(ctx, &w, errors.New("issue execution time budget exhausted"))
		return
	}
	runCtx, timeout := context.WithTimeout(ctx, time.Duration(remaining)*time.Second)
	defer timeout()
	if cfg.Runner.Mode == "responses" && !cfg.ModelReady() {
		w.Phase = "WAITING_CONFIGURATION"
		w.LastError = "fill in Responses endpoint, Token and Dev/Review model names in Settings"
		_ = e.Store.Save(ctx, &w, statusAction(w))
		return
	}
	var err error
	switch w.Phase {
	case "TRIAGE":
		err = e.triage(runCtx, &w)
	case "PLANNING", "CHANGES_REQUESTED":
		err = e.plan(runCtx, &w)
	case "DEVELOPING":
		err = e.build(runCtx, &w)
	case "VERIFYING":
		err = e.verify(runCtx, &w)
	case "REVIEWING":
		err = e.review(runCtx, &w)
	}
	if err != nil && !errors.Is(err, store.ErrStale) && !errors.Is(err, context.Canceled) {
		e.fail(ctx, &w, err)
	}
}
func (e *Engine) fail(ctx context.Context, w *domain.Workflow, err error) {
	w.LastError = e.redact(err.Error())
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
func (e *Engine) ask(ctx context.Context, w domain.Workflow, role, input string, tools []agent.Tool, schema json.RawMessage, validators ...func(json.RawMessage) error) (json.RawMessage, error) {
	cfg := e.Config()
	token, err := os.ReadFile(cfg.Model.TokenFile)
	if err != nil {
		return nil, errors.New("model Token file is unavailable")
	}
	model := cfg.Model.Dev
	if strings.Contains(role, "Reviewer") || strings.Contains(role, "Review Planner") || strings.Contains(role, "Skeptic") {
		model = cfg.Model.Review
	}
	r, err := agent.NewRunner(agent.Config{BaseURL: cfg.Model.BaseURL, Token: strings.TrimSpace(string(token)), Model: model, Stream: cfg.Model.Stream, MaxOutputTokens: cfg.Model.MaxOutputTokens})
	if err != nil {
		return nil, err
	}
	var validateReport func(json.RawMessage) error
	if len(validators) > 0 {
		validateReport = func(report json.RawMessage) error {
			for _, validate := range validators {
				if validate != nil {
					if err := validate(report); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}
	res, err := r.Run(ctx, agent.Request{System: role, Input: raw(input), Model: model, Tools: tools, ReportSchema: schema, MaxCalls: cfg.Budget.IssueCalls, MaxRequests: cfg.Budget.IssueCalls, ValidateReport: validateReport, BeforeRequest: func(c context.Context) error {
		live := e.Config()
		return e.Store.ReserveRequest(c, w.ID, w.Generation, live.Budget.IssueCalls, live.Budget.DailyCalls)
	}, OnUsage: func(c context.Context, u agent.Usage) error {
		return e.Store.Usage(c, w.ID, u.InputTokens, u.OutputTokens)
	}})
	e.Store.Audit(context.WithoutCancel(ctx), w.ID, "model_run", map[string]any{"role": strings.Split(role, " You are ")[len(strings.Split(role, " You are "))-1][:min(64, len(strings.Split(role, " You are ")[len(strings.Split(role, " You are "))-1]))], "requests": res.Requests, "usage": res.Usage, "error": errString(err)})
	if err == nil && len(res.Report) > 0 {
		e.Store.Audit(context.WithoutCancel(ctx), w.ID, "model_report", map[string]any{"generation": w.Generation, "report": json.RawMessage(e.redact(string(res.Report)))})
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
	profile := domain.Digest([]any{r.Verification, cfg.EffectiveWorkspaceBytes()})
	return domain.VerificationKey(w, domain.Digest(r), profile, e.imageDigest(ctx))
}
func (e *Engine) source(ctx context.Context, w domain.Workflow, sha string) (map[string]sandbox.File, error) {
	if w.Repository == "local/pilot" {
		return loadLocalSnapshot(e.Config().DataDir + "/fixtures/" + domain.Digest(w.ID)[:24])
	}
	if sha == "" {
		return nil, errors.New("source snapshot requires an exact commit SHA")
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
		return domain.Evidence{}, fmt.Errorf("encode evidence: %w", err)
	}
	directory := e.Config().DataDir + "/evidence/" + w.ID
	if err := os.MkdirAll(directory, 0700); err != nil {
		return domain.Evidence{}, err
	}
	file := directory + "/" + domain.Digest([]any{id, data}) + ".json"
	if err := os.WriteFile(file, b, 0600); err != nil {
		return domain.Evidence{}, err
	}
	ev := domain.Evidence{ID: id, Kind: kind, Path: file, Hash: domain.Digest(data), CreatedAt: time.Now().UTC(), BaseSHA: w.BaseSHA, HeadSHA: w.HeadSHA, ProfileDigest: domain.Digest([]any{e.repository(w).Verification, e.Config().EffectiveWorkspaceBytes()})}
	return ev, nil
}
func (e *Engine) Demo(ctx context.Context) (domain.Workflow, error) {
	if e.Config().Runner.Mode != "fake" {
		return domain.Workflow{}, errors.New("select Fake runner in Settings before starting the local fixture")
	}
	issue := int(time.Now().Unix())
	w := domain.Workflow{ID: domain.ID(-1, issue), RepositoryID: -1, Repository: "local/pilot", Issue: issue, InstallationID: 0, IssueTitle: "Fixture: repair Add", IssueBody: "Fix Add so it adds two integers. Preserve tests and exported function signature.", AuthorizedBy: "local-dashboard", Phase: "TRIAGE", Control: "ACTIVE", BaseBranch: "main", BaseSHA: "local-fixture", Branch: fmt.Sprintf("codex/issue-%d", issue)}
	err := e.Store.Create(ctx, w)
	if err != nil {
		return w, err
	}
	return e.Store.Get(ctx, w.ID)
}
