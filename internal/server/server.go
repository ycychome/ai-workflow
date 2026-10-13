package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/orchestration"
	"github.com/ycychome/ai-workflow/internal/store"
	"gopkg.in/yaml.v3"
)

//go:embed web/index.html web/app.js web/style.css
var webFiles embed.FS

const (
	sessionCookie = "aiwf_session"
	sessionTTL    = 12 * time.Hour
	maxWebhook    = 2 << 20
)

type pendingState struct{ expires time.Time }

type workflowStateView struct {
	domain.Workflow
	Scheduling               store.Scheduling `json:"scheduling"`
	ForceApprovalCurrent     bool             `json:"force_approval_current"`
	IssueMatchesApprovedSpec bool             `json:"issue_matches_approved_spec"`
}

// Handlers owns the two HTTP surfaces used by the control process.
type Handlers struct {
	engine     *orchestration.Engine
	configPath string
	tokenPath  string
	key        [32]byte
	mu         sync.Mutex
	sessions   map[string]time.Time
	states     map[string]pendingState
	installURL string
	client     *http.Client
}

// New constructs independent public and dashboard handlers. tokenPath may be
// empty, in which case /secrets/dashboard-token is used.
func New(engine *orchestration.Engine, configPath, tokenPath string) (*Handlers, error) {
	if engine == nil || engine.Store == nil {
		return nil, errors.New("server: engine and workflow store are required")
	}
	if strings.TrimSpace(configPath) == "" {
		return nil, errors.New("server: config path is required")
	}
	if tokenPath == "" {
		tokenPath = "/secrets/dashboard-token"
	}
	h := &Handlers{engine: engine, configPath: configPath, tokenPath: tokenPath, sessions: make(map[string]time.Time), states: make(map[string]pendingState), client: &http.Client{Timeout: 20 * time.Second}}
	if _, err := rand.Read(h.key[:]); err != nil {
		return nil, fmt.Errorf("server: create session key: %w", err)
	}
	return h, nil
}

// PublicHandler exposes only readiness, liveness, and GitHub webhook endpoints.
func (h *Handlers) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/readyz", h.readyz)
	mux.HandleFunc("/webhooks/github", h.githubWebhook)
	return securityHeaders(mux)
}

// DashboardHandler serves the embedded dashboard and its authenticated API.
func (h *Handlers) DashboardHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", h.login)
	mux.HandleFunc("/api/", h.dashboardAPI)
	mux.HandleFunc("/setup/callback", h.manifestCallback)
	mux.HandleFunc("/setup", h.setup)
	mux.HandleFunc("/setup/manifest", h.manifestStart)
	mux.HandleFunc("/setup/installation", h.selectInstallation)
	mux.HandleFunc("/", h.static)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'self'; form-action 'self' https://github.com")
		next.ServeHTTP(w, r)
	})
}

func (h *Handlers) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) readyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.engine.Store.DB.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "database": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "database": "ok"})
}

func (h *Handlers) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := h.checkOrigin(r); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	expected, err := os.ReadFile(h.tokenPath)
	if err != nil || len(strings.TrimSpace(string(expected))) == 0 {
		writeError(w, http.StatusServiceUnavailable, "dashboard token is not configured")
		return
	}
	gotHash := sha256.Sum256([]byte(strings.TrimSpace(body.Token)))
	wantHash := sha256.Sum256([]byte(strings.TrimSpace(string(expected))))
	if subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid dashboard token")
		return
	}
	value, sessionID, err := h.newSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	h.mu.Lock()
	h.sessions[sessionID] = time.Now().Add(sessionTTL)
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) newSession() (cookieValue, id string, err error) {
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", "", err
	}
	id = base64.RawURLEncoding.EncodeToString(nonce[:])
	mac := hmac.New(sha256.New, h.key[:])
	_, _ = mac.Write(nonce[:])
	h.mu.Lock()
	for old, expiry := range h.sessions {
		if time.Now().After(expiry) {
			delete(h.sessions, old)
		}
	}
	h.mu.Unlock()
	return id + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), id, nil
}

func (h *Handlers) sessionID(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 2 {
		return "", false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, h.key[:])
	_, _ = mac.Write(nonce)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	id := parts[0]
	h.mu.Lock()
	defer h.mu.Unlock()
	expires, ok := h.sessions[id]
	if !ok || time.Now().After(expires) {
		delete(h.sessions, id)
		return "", false
	}
	return id, true
}

func (h *Handlers) checkOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil // Local command-line clients may use the authenticated cookie directly.
	}
	u, err := url.Parse(origin)
	expectedScheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		expectedScheme = "https"
	}
	if err != nil || u.Scheme != expectedScheme || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
		return errors.New("cross-origin request rejected")
	}
	return nil
}

func (h *Handlers) dashboardAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/login" {
		h.login(w, r)
		return
	}
	if _, ok := h.sessionID(r); !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if err := h.checkOrigin(r); err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
	}
	switch r.URL.Path {
	case "/api/state":
		h.state(w, r)
	case "/api/settings":
		h.settings(w, r)
	case "/api/audit":
		h.audit(w, r)
	case "/api/evidence":
		h.evidence(w, r)
	case "/api/doctor":
		h.doctor(w, r)
	case "/api/demo":
		h.demo(w, r)
	case "/api/github/discover":
		h.discover(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/api/workflows/") && strings.HasSuffix(r.URL.Path, "/commands") {
			h.command(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "unknown API endpoint")
	}
}

func (h *Handlers) evidence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	wf, err := h.engine.Store.Get(r.Context(), r.URL.Query().Get("workflow_id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow was not found")
		return
	}
	for _, ev := range wf.Evidence {
		if ev.ID != r.URL.Query().Get("evidence_id") {
			continue
		}
		file, err := filepath.Abs(ev.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid evidence path")
			return
		}
		base, err := filepath.Abs(filepath.Join(h.engine.Config().DataDir, "evidence", wf.ID))
		if err != nil || !strings.HasPrefix(file, base+string(filepath.Separator)) {
			writeError(w, http.StatusForbidden, "evidence is outside the workflow store")
			return
		}
		f, err := os.Open(file)
		if err != nil {
			writeError(w, http.StatusNotFound, "evidence file is unavailable")
			return
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 256<<10))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read evidence")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"evidence": ev, "content": string(b)})
		return
	}
	writeError(w, http.StatusNotFound, "evidence ID was not recorded for this workflow")
}

func (h *Handlers) state(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	workflows, err := h.engine.Store.List(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load workflows")
		return
	}
	cfg := h.engine.Config()
	workflowLimit := cfg.Runner.WorkflowConcurrency
	if workflowLimit == 0 {
		workflowLimit = 2
	}
	scheduling, err := h.engine.Store.SchedulingFor(ctx, workflows, workflowLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load workflow scheduling metadata")
		return
	}
	maxGeneration := int64(0)
	views := make([]workflowStateView, 0, len(workflows))
	for _, wf := range workflows {
		if wf.Generation > maxGeneration {
			maxGeneration = wf.Generation
		}
		views = append(views, workflowStateView{Workflow: wf, Scheduling: scheduling[wf.ID], ForceApprovalCurrent: h.engine.ForceApprovalCurrent(ctx, wf), IssueMatchesApprovedSpec: h.engine.IssueMatchesApprovedSpec(wf)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": views, "health": h.basicHealth(ctx, cfg), "config": publicConfig(cfg), "generation": maxGeneration})
}

func (h *Handlers) basicHealth(ctx context.Context, cfg config.Config) map[string]any {
	status := "ok"
	if err := h.engine.Store.DB.Ping(ctx); err != nil {
		status = "error"
	}
	model := "not_configured"
	if cfg.Model.BaseURL != "" && cfg.Model.Dev != "" && cfg.Model.Review != "" && cfg.Model.TokenFile != "" {
		if _, err := os.Stat(cfg.Model.TokenFile); err == nil {
			model = "configured"
		}
	}
	github := "not_configured"
	if cfg.GitHub.AppID != 0 {
		if _, err := os.Stat(cfg.GitHub.PrivateKeyFile); err == nil {
			github = "configured"
		}
	}
	sandbox := "available"
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := exec.CommandContext(probeCtx, "docker", "image", "inspect", cfg.Runner.Image).Run(); err != nil {
		sandbox = "unavailable"
	} else if cfg.Runner.Mode == "fake" {
		sandbox = "fake_mode"
	}
	return map[string]any{"database": status, "github": github, "model": model, "sandbox": sandbox}
}

func publicConfig(cfg config.Config) map[string]any {
	repos := make([]map[string]any, 0, len(cfg.Repositories))
	for _, repo := range cfg.Repositories {
		repos = append(repos, map[string]any{"name": repo.Name, "id": repo.ID, "base_branch": repo.BaseBranch, "maintainers": repo.Maintainers})
	}
	return map[string]any{
		"github":       map[string]any{"app_id": cfg.GitHub.AppID, "installation_id": cfg.GitHub.InstallationID, "public_url": cfg.GitHub.PublicURL, "configured": cfg.GitHub.AppID > 0},
		"model":        map[string]any{"base_url": cfg.Model.BaseURL, "dev": cfg.Model.Dev, "review": cfg.Model.Review, "stream": cfg.Model.Stream, "max_output_tokens": cfg.Model.MaxOutputTokens},
		"runner":       map[string]any{"mode": cfg.Runner.Mode, "image": cfg.Runner.Image, "builders": cfg.Runner.Builders, "reviewers": cfg.Runner.Reviewers, "workflow_concurrency": cfg.Runner.WorkflowConcurrency, "require_accept_for_analysis": cfg.Runner.RequireAcceptForAnalysis, "attempt_minutes": cfg.Runner.AttemptMinutes},
		"budget":       map[string]any{"issue_calls": cfg.Budget.IssueCalls, "daily_calls": cfg.Budget.DailyCalls, "issue_minutes": cfg.Budget.IssueMinutes, "max_attempts": cfg.Budget.MaxAttempts, "max_fix_rounds": cfg.Budget.MaxFixRounds, "input_price_per_million": cfg.Budget.InputPrice, "output_price_per_million": cfg.Budget.OutputPrice},
		"repositories": repos,
	}
}

type settingsUpdate struct {
	GitHub struct {
		PublicURL *string `json:"public_url"`
	} `json:"github"`
	Model struct {
		BaseURL         *string `json:"base_url"`
		Dev             *string `json:"dev"`
		Review          *string `json:"review"`
		Stream          *bool   `json:"stream"`
		MaxOutputTokens *int    `json:"max_output_tokens"`
		Token           string  `json:"token"`
	} `json:"model"`
	Runner struct {
		Mode                     *string `json:"mode"`
		Builders                 *int    `json:"builders"`
		Reviewers                *int    `json:"reviewers"`
		WorkflowConcurrency      *int    `json:"workflow_concurrency"`
		RequireAcceptForAnalysis *bool   `json:"require_accept_for_analysis"`
		AttemptMinutes           *int    `json:"attempt_minutes"`
	} `json:"runner"`
	Budget struct {
		IssueCalls   *int     `json:"issue_calls"`
		DailyCalls   *int     `json:"daily_calls"`
		IssueMinutes *int     `json:"issue_minutes"`
		MaxAttempts  *int     `json:"max_attempts"`
		MaxFixRounds *int     `json:"max_fix_rounds"`
		InputPrice   *float64 `json:"input_price_per_million"`
		OutputPrice  *float64 `json:"output_price_per_million"`
	} `json:"budget"`
}

func (h *Handlers) settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := h.engine.Config()
		writeJSON(w, http.StatusOK, map[string]any{
			"github": map[string]any{"app_id": cfg.GitHub.AppID, "installation_id": cfg.GitHub.InstallationID, "public_url": cfg.GitHub.PublicURL},
			"model":  map[string]any{"base_url": cfg.Model.BaseURL, "dev": cfg.Model.Dev, "review": cfg.Model.Review, "stream": cfg.Model.Stream, "max_output_tokens": cfg.Model.MaxOutputTokens},
			"runner": map[string]any{"mode": cfg.Runner.Mode, "builders": cfg.Runner.Builders, "reviewers": cfg.Runner.Reviewers, "workflow_concurrency": cfg.Runner.WorkflowConcurrency, "require_accept_for_analysis": cfg.Runner.RequireAcceptForAnalysis, "attempt_minutes": cfg.Runner.AttemptMinutes},
			"budget": map[string]any{"issue_calls": cfg.Budget.IssueCalls, "daily_calls": cfg.Budget.DailyCalls, "issue_minutes": cfg.Budget.IssueMinutes, "max_attempts": cfg.Budget.MaxAttempts, "max_fix_rounds": cfg.Budget.MaxFixRounds, "input_price_per_million": cfg.Budget.InputPrice, "output_price_per_million": cfg.Budget.OutputPrice},
		})
	case http.MethodPost:
		h.updateSettings(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (h *Handlers) updateSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var update settingsUpdate
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings JSON: "+err.Error())
		return
	}
	cfg := h.engine.Config()
	if update.GitHub.PublicURL != nil {
		if strings.TrimSpace(*update.GitHub.PublicURL) == "" {
			cfg.GitHub.PublicURL = ""
		} else {
			publicURL, err := normalizePublicURL(*update.GitHub.PublicURL)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			cfg.GitHub.PublicURL = publicURL
		}
	}
	if update.Model.BaseURL != nil {
		cfg.Model.BaseURL = strings.TrimSpace(*update.Model.BaseURL)
	}
	if update.Model.Dev != nil {
		cfg.Model.Dev = strings.TrimSpace(*update.Model.Dev)
	}
	if update.Model.Review != nil {
		cfg.Model.Review = strings.TrimSpace(*update.Model.Review)
	}
	if update.Model.Stream != nil {
		cfg.Model.Stream = *update.Model.Stream
	}
	if update.Model.MaxOutputTokens != nil {
		cfg.Model.MaxOutputTokens = *update.Model.MaxOutputTokens
	}
	if update.Runner.Mode != nil {
		cfg.Runner.Mode = strings.TrimSpace(*update.Runner.Mode)
	}
	if update.Runner.Builders != nil {
		cfg.Runner.Builders = *update.Runner.Builders
	}
	if update.Runner.Reviewers != nil {
		cfg.Runner.Reviewers = *update.Runner.Reviewers
	}
	if update.Runner.WorkflowConcurrency != nil {
		cfg.Runner.WorkflowConcurrency = *update.Runner.WorkflowConcurrency
	}
	if update.Runner.RequireAcceptForAnalysis != nil {
		cfg.Runner.RequireAcceptForAnalysis = *update.Runner.RequireAcceptForAnalysis
	}
	if update.Runner.AttemptMinutes != nil {
		cfg.Runner.AttemptMinutes = *update.Runner.AttemptMinutes
	}
	if update.Budget.IssueCalls != nil {
		cfg.Budget.IssueCalls = *update.Budget.IssueCalls
	}
	if update.Budget.DailyCalls != nil {
		cfg.Budget.DailyCalls = *update.Budget.DailyCalls
	}
	if update.Budget.IssueMinutes != nil {
		cfg.Budget.IssueMinutes = *update.Budget.IssueMinutes
	}
	if update.Budget.MaxAttempts != nil {
		cfg.Budget.MaxAttempts = *update.Budget.MaxAttempts
	}
	if update.Budget.MaxFixRounds != nil {
		cfg.Budget.MaxFixRounds = *update.Budget.MaxFixRounds
	}
	if update.Budget.InputPrice != nil {
		cfg.Budget.InputPrice = *update.Budget.InputPrice
	}
	if update.Budget.OutputPrice != nil {
		cfg.Budget.OutputPrice = *update.Budget.OutputPrice
	}
	if !validSettings(cfg) {
		writeError(w, http.StatusBadRequest, "invalid settings: mode must be fake or responses; builders/reviewers 1–2; workflow concurrency 1–4; budget and output limits must be valid")
		return
	}
	if strings.TrimSpace(update.Model.Token) != "" {
		if err := h.engine.Invalidate(r.Context(), "model credential changed; resume to reconcile and reverify"); err != nil {
			writeError(w, http.StatusInternalServerError, "could not revoke current runs before credential update")
			return
		}
		if cfg.Model.TokenFile == "" {
			cfg.Model.TokenFile = filepath.Join(filepath.Dir(h.configPath), "model-token")
		}
		if err := writeSecret(resolveConfigPath(h.configPath, cfg.Model.TokenFile), strings.TrimSpace(update.Model.Token)); err != nil {
			writeError(w, http.StatusInternalServerError, "could not save model token")
			return
		}
	}
	if err := h.persistConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save settings: "+err.Error())
		return
	}
	if err := h.engine.SetConfig(r.Context(), cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "settings saved but engine reload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": publicConfig(cfg)})
}

func validSettings(cfg config.Config) bool {
	if !((cfg.Runner.Mode == "fake" || cfg.Runner.Mode == "responses") && cfg.Runner.Builders >= 1 && cfg.Runner.Builders <= 2 && cfg.Runner.Reviewers >= 1 && cfg.Runner.Reviewers <= 2 && config.ValidWorkflowConcurrency(cfg.Runner.WorkflowConcurrency) && cfg.Runner.AttemptMinutes >= 1 && cfg.Budget.IssueCalls >= 1 && cfg.Budget.DailyCalls >= 1 && cfg.Budget.IssueMinutes >= 1 && cfg.Budget.MaxAttempts >= 1 && cfg.Budget.MaxFixRounds >= 0 && cfg.Model.MaxOutputTokens >= 1 && cfg.Budget.InputPrice >= 0 && cfg.Budget.OutputPrice >= 0 && !math.IsNaN(cfg.Budget.InputPrice) && !math.IsNaN(cfg.Budget.OutputPrice) && !math.IsInf(cfg.Budget.InputPrice, 0) && !math.IsInf(cfg.Budget.OutputPrice, 0)) {
		return false
	}
	if cfg.Model.BaseURL == "" {
		return true
	}
	u, err := url.Parse(cfg.Model.BaseURL)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") && u.RawQuery == "" && u.Fragment == ""
}

func (h *Handlers) persistConfig(cfg config.Config) error {
	// Keep a file-configured database URL, but never copy an environment-only
	// database credential into the YAML file during an unrelated dashboard save.
	configPath, err := filepath.Abs(h.configPath)
	if err != nil {
		return err
	}
	if previous, readErr := os.ReadFile(configPath); readErr == nil {
		var fromFile config.Config
		if yaml.Unmarshal(previous, &fromFile) == nil {
			cfg.DatabaseURL = fromFile.DatabaseURL
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return atomicWrite(configPath, data, 0600)
}

func resolveConfigPath(configPath, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return filepath.Join(filepath.Dir(configPath), path)
	}
	return resolved
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".aiwf-config-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeSecret(path, value string) error {
	if value == "" {
		return errors.New("empty secret")
	}
	return atomicWrite(path, []byte(value+"\n"), 0600)
}

func (h *Handlers) command(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
	path = strings.TrimSuffix(path, "/commands")
	id, err := url.PathUnescape(path)
	if err != nil || id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusBadRequest, "invalid workflow id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		Command            string  `json:"command"`
		ExpectedGeneration *int64  `json:"expected_generation,omitempty"`
		ExpectedHeadSHA    *string `json:"expected_head_sha,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid command JSON")
		return
	}
	allowed := map[string]bool{"status": true, "pause": true, "resume": true, "cancel": true, "replan": true, "review deep": true, "takeover": true, "accept": true, "analyze": true, "forceaccept": true, "forceaccept dev": true, "forceaccept merge": true, "reject": true, "defer": true}
	if !allowed[body.Command] {
		writeError(w, http.StatusBadRequest, "unsupported workflow command")
		return
	}
	forceCommand := strings.HasPrefix(body.Command, "forceaccept")
	if forceCommand && (body.ExpectedGeneration == nil || body.ExpectedHeadSHA == nil) {
		writeError(w, http.StatusBadRequest, "forceaccept requires expected_generation and expected_head_sha from the confirmed workflow state")
		return
	}
	if body.ExpectedGeneration != nil {
		current, getErr := h.engine.Store.Get(r.Context(), id)
		if getErr != nil {
			writeError(w, http.StatusConflict, "workflow changed before command confirmation")
			return
		}
		if current.Generation != *body.ExpectedGeneration {
			writeError(w, http.StatusConflict, "workflow generation changed; review the current state and confirm again")
			return
		}
		if body.ExpectedHeadSHA != nil && current.HeadSHA != *body.ExpectedHeadSHA {
			writeError(w, http.StatusConflict, "pull request head changed; review the current state and confirm again")
			return
		}
	}
	commandCtx := r.Context()
	if body.ExpectedGeneration != nil {
		commandCtx = orchestration.WithExpectedGeneration(commandCtx, *body.ExpectedGeneration)
	}
	if body.ExpectedHeadSHA != nil {
		commandCtx = orchestration.WithExpectedHead(commandCtx, *body.ExpectedHeadSHA)
	}
	if body.ExpectedGeneration != nil && body.ExpectedHeadSHA != nil {
		commandCtx = store.WithExpectedState(commandCtx, *body.ExpectedGeneration, *body.ExpectedHeadSHA)
	}
	if err := h.engine.Command(commandCtx, id, "local-dashboard", body.Command, true); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "workflow_id": id, "command": body.Command})
}

func (h *Handlers) audit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	id := r.URL.Query().Get("workflow_id")
	var entries []map[string]any
	var err error
	if id != "" {
		entries, err = h.engine.Store.AuditList(r.Context(), id)
	} else {
		entries, err = h.allAudit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load audit log")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (h *Handlers) allAudit(ctx context.Context) ([]map[string]any, error) {
	rows, err := h.engine.Store.DB.Query(ctx, `SELECT id,workflow_id,kind,data,created_at FROM audit ORDER BY id DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var workflowID *string
		var kind string
		var raw []byte
		var created time.Time
		if err := rows.Scan(&id, &workflowID, &kind, &raw, &created); err != nil {
			return nil, err
		}
		var data any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}
		wid := ""
		if workflowID != nil {
			wid = *workflowID
		}
		entries = append(entries, map[string]any{"id": id, "workflow_id": wid, "kind": kind, "data": data, "created_at": created})
	}
	return entries, rows.Err()
}

func (h *Handlers) demo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	wf, err := h.engine.Demo(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workflow": wf})
}

func (h *Handlers) doctor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cfg := h.engine.Config()
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	results := map[string]any{}
	if err := h.engine.Store.DB.Ping(ctx); err != nil {
		results["database"] = map[string]any{"status": "error", "message": "database ping failed"}
	} else {
		results["database"] = map[string]any{"status": "ok"}
	}
	if cfg.GitHub.AppID == 0 || cfg.GitHub.PrivateKeyFile == "" {
		results["github"] = map[string]any{"status": "skip", "message": "GitHub App is not configured"}
	} else if app, err := h.engine.App(); err != nil {
		results["github"] = map[string]any{"status": "error", "message": err.Error()}
	} else if installs, err := app.ListInstallations(ctx); err != nil {
		results["github"] = map[string]any{"status": "error", "message": err.Error()}
	} else {
		results["github"] = map[string]any{"status": "ok", "installations": len(installs)}
	}
	if cfg.Runner.Mode == "fake" || !cfg.ModelReady() {
		results["model"] = map[string]any{"status": "skip", "message": "real model is not configured or Fake mode is selected"}
	} else if secret, err := os.ReadFile(cfg.Model.TokenFile); err != nil {
		results["model"] = map[string]any{"status": "error", "message": "model token file is unavailable"}
	} else if client, err := agent.NewClient(agent.Config{BaseURL: cfg.Model.BaseURL, Token: strings.TrimSpace(string(secret)), Model: cfg.Model.Dev, Stream: false, MaxOutputTokens: 4}); err != nil {
		results["model"] = map[string]any{"status": "error", "message": err.Error()}
	} else if err := client.Doctor(ctx); err != nil {
		results["model"] = map[string]any{"status": "error", "message": redact(err.Error(), string(secret))}
	} else {
		results["model"] = map[string]any{"status": "ok"}
	}
	if cfg.Runner.Image == "" {
		results["sandbox"] = map[string]any{"status": "skip", "message": "runner image is not configured"}
	} else {
		cmd := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", cfg.Runner.Image)
		out, err := cmd.Output()
		if err != nil {
			results["sandbox"] = map[string]any{"status": "error", "message": "Docker daemon or runner image is unavailable"}
		} else {
			results["sandbox"] = map[string]any{"status": "ok", "image_id": strings.TrimSpace(string(out))}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"health": results})
}

func redact(message, secret string) string {
	secret = strings.TrimSpace(secret)
	if len(secret) > 3 {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	if len(message) > 1500 {
		message = message[:1500] + "…"
	}
	return message
}

func (h *Handlers) githubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cfg := h.engine.Config()
	if cfg.GitHub.WebhookSecretFile == "" {
		writeError(w, http.StatusServiceUnavailable, "webhook secret is not configured")
		return
	}
	secret, err := os.ReadFile(cfg.GitHub.WebhookSecretFile)
	if err != nil || strings.TrimSpace(string(secret)) == "" {
		writeError(w, http.StatusServiceUnavailable, "webhook secret is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhook)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "webhook payload exceeds 2 MiB or could not be read")
		return
	}
	if err := gh.VerifyWebhook(strings.TrimSpace(string(secret)), raw, r.Header.Get("X-Hub-Signature-256")); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	allowed := map[string]bool{"issues": true, "issue_comment": true, "pull_request": true, "push": true, "check_run": true, "check_suite": true, "workflow_run": true, "pull_request_review": true}
	if !allowed[event] {
		writeError(w, http.StatusUnprocessableEntity, "unsupported GitHub event")
		return
	}
	parsed, err := gh.ParseWebhook(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook payload")
		return
	}
	if parsed.Repository == nil || parsed.Repository.ID <= 0 || parsed.Repository.FullName == "" {
		writeError(w, http.StatusUnprocessableEntity, "webhook repository identity is missing")
		return
	}
	matched := false
	for _, repo := range cfg.Repositories {
		if repo.Name == parsed.Repository.FullName && repo.ID > 0 && repo.ID == parsed.Repository.ID {
			matched = true
			break
		}
	}
	if !matched {
		writeError(w, http.StatusForbidden, "repository is not configured or its numeric ID is not bound")
		return
	}
	if parsed.Installation == nil || parsed.Installation.ID <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "webhook installation identity is missing")
		return
	}
	if cfg.GitHub.InstallationID > 0 && parsed.Installation.ID != cfg.GitHub.InstallationID {
		writeError(w, http.StatusForbidden, "webhook installation does not match configured installation")
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" {
		writeError(w, http.StatusBadRequest, "missing GitHub delivery ID")
		return
	}
	if err := h.engine.Store.Inbox(r.Context(), delivery, event, raw); err != nil {
		writeError(w, http.StatusInternalServerError, "could not queue webhook")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handlers) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/setup") {
		http.NotFound(w, r)
		return
	}
	files, err := webFiles.ReadFile("web/index.html")
	if r.URL.Path == "/app.js" {
		files, err = webFiles.ReadFile("web/app.js")
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else if r.URL.Path == "/style.css" {
		files, err = webFiles.ReadFile("web/style.css")
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	if err != nil {
		http.Error(w, "embedded dashboard asset unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		_, _ = w.Write(files)
	}
}

func (h *Handlers) setup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if _, ok := h.sessionID(r); !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	cfg := h.engine.Config()
	message := ""
	if r.URL.Query().Get("registered") == "1" {
		message = "GitHub App 注册成功。接下来请选择该 App 的安装目标仓库。"
	}
	installURL := h.installURL
	if installURL == "" && cfg.GitHub.AppID > 0 {
		installURL = "https://github.com/settings/installations"
	}
	items := make([]map[string]any, 0)
	listError := ""
	if cfg.GitHub.AppID > 0 {
		app, err := h.engine.App()
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			installations, listErr := app.ListInstallations(ctx)
			cancel()
			if listErr != nil {
				listError = listErr.Error()
			} else {
				for _, inst := range installations {
					items = append(items, map[string]any{"id": inst.ID, "login": inst.Account.Login, "type": inst.Account.Type, "selected": inst.ID == cfg.GitHub.InstallationID})
				}
			}
		} else {
			listError = err.Error()
		}
	}
	repositories := make([]map[string]any, 0, len(cfg.Repositories))
	for _, repo := range cfg.Repositories {
		repositories = append(repositories, map[string]any{"name": repo.Name, "id": repo.ID, "bound": repo.ID > 0})
	}
	data := map[string]any{"PublicURL": cfg.GitHub.PublicURL, "AppID": cfg.GitHub.AppID, "InstallationID": cfg.GitHub.InstallationID, "InstallURL": installURL, "Installations": items, "Repositories": repositories, "ListError": listError, "Message": message}
	if r.URL.Query().Get("error") != "" {
		data["Message"] = r.URL.Query().Get("error")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := setupTemplate.Execute(w, data); err != nil {
		http.Error(w, "could not render setup", http.StatusInternalServerError)
	}
}

var setupTemplate = template.Must(template.New("setup").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>GitHub App 设置</title><style>
*{box-sizing:border-box}body{margin:0;padding:38px 18px;background:#f4f6fa;color:#23314b;font:14px -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC",sans-serif}.wrap{max-width:850px;margin:auto}.card{margin:14px 0;padding:22px;background:white;border:1px solid #e4e9f1;border-radius:15px;box-shadow:0 8px 25px #1b2f520d}h1{font-size:27px;letter-spacing:-.04em;margin:5px 0 8px}h2{font-size:15px;margin:0 0 12px}.muted{color:#7c889c;font-size:12px;line-height:1.7}.eyebrow{font-size:10px;font-weight:800;letter-spacing:.12em;color:#6079b7}.row{display:flex;align-items:center;justify-content:space-between;gap:15px;padding:11px 0;border-top:1px solid #eef1f5}.row:first-of-type{border-top:0}.btn{display:inline-flex;align-items:center;justify-content:center;min-height:37px;padding:0 13px;border:0;border-radius:8px;background:#426bd8;color:#fff;font-weight:650;text-decoration:none;cursor:pointer}.btn.secondary{color:#465671;background:#f3f5fa;border:1px solid #e3e8f0}.field{display:grid;gap:7px;margin:14px 0;color:#57647a;font-size:11px;font-weight:650}.field input{height:39px;padding:0 11px;border:1px solid #dfe5ee;border-radius:8px;font:inherit}.message{padding:11px 13px;border-radius:8px;color:#315c9e;background:#eef4ff}.warning{color:#825d20;background:#fff6e6}.grid{display:grid;grid-template-columns:1fr 1fr;gap:12px}.small{font-size:10px;color:#8b96a8}@media(max-width:620px){body{padding:22px 12px}.card{padding:17px}.grid{grid-template-columns:1fr}.row{align-items:flex-start;flex-direction:column}}
</style></head><body><main class="wrap"><p class="eyebrow">AI WORKFLOW · PRIVATE SETUP</p><h1>GitHub App 设置</h1><p class="muted">注册个人 GitHub App，完成安装后仅绑定配置文件中明确列出的仓库。控制台不会自动授权所有仓库。</p>
{{if .Message}}<p class="message">{{.Message}}</p>{{end}}
<section class="card"><h2>1 · 注册 App</h2><p class="muted">应用权限：Contents、Issues、Pull requests、Checks 写入；Actions 与 Metadata 读取。Webhook 事件限定在 Issue、PR、检查和工作流更新。</p><p class="small">请在 GitHub 个人账号 <strong>zymooll</strong> 登录后继续。应用将注册为 Public App；不要选择所有仓库的授权范围。</p><form method="post" action="/setup/manifest"><label class="field">公开 Webhook 基础 URL<input type="url" name="public_url" value="{{.PublicURL}}" placeholder="https://your-tunnel.example" required></label><p class="small">服务器会追加 <code>/webhooks/github</code>，并设置本地回调地址 <code>http://127.0.0.1:8081/setup/callback</code>。</p><button class="btn" type="submit">前往 GitHub 注册</button></form></section>
{{if .AppID}}<section class="card"><h2>2 · 选择安装</h2><p class="muted">App ID：{{.AppID}}　当前 Installation：{{if .InstallationID}}{{.InstallationID}}{{else}}等待选择{{end}}</p>{{if .ListError}}<p class="message warning">安装列表查询失败：{{.ListError}}</p>{{end}}{{if .InstallURL}}<p><a class="btn secondary" href="{{.InstallURL}}" target="_blank" rel="noreferrer">打开 GitHub 安装页 ↗</a></p>{{end}}{{if .Installations}}{{range .Installations}}<form class="row" method="post" action="/setup/installation"><div><strong>{{.login}}</strong> <span class="small">{{.type}} · Installation {{.id}}</span>{{if .selected}} <span class="small">· 当前选择</span>{{end}}</div><input type="hidden" name="installation_id" value="{{.id}}"><button class="btn secondary" type="submit">选择并绑定配置仓库</button></form>{{end}}{{else}}<p class="muted">尚未发现安装。请打开 GitHub 安装页，选择 “Only select repositories”，仅勾选配置中列出的仓库，然后返回此页刷新。</p>{{end}}</section>{{end}}
<section class="card"><h2>3 · 配置仓库绑定</h2><p class="muted">仅绑定以下配置仓库的数字 Repository ID。其他仓库即使在 GitHub App 安装范围内，也不会被该服务接受。</p>{{range .Repositories}}<div class="row"><strong>{{.name}}</strong><span class="small">{{if .bound}}Repository ID {{.id}}{{else}}尚未绑定{{end}}</span></div>{{else}}<p class="muted">配置中没有仓库条目。</p>{{end}}</section>
<p class="muted"><a href="/">返回工作流控制台</a></p></main></body></html>`))

func (h *Handlers) manifestStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if _, ok := h.sessionID(r); !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := h.checkOrigin(r); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid registration form")
		return
	}
	publicURL, err := normalizePublicURL(r.FormValue("public_url"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := h.engine.Config()
	cfg.GitHub.PublicURL = publicURL
	if err := h.persistConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save public URL")
		return
	}
	if err := h.engine.SetConfig(r.Context(), cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "could not reload configuration")
		return
	}
	state, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create registration state")
		return
	}
	h.mu.Lock()
	for old, item := range h.states {
		if time.Now().After(item.expires) {
			delete(h.states, old)
		}
	}
	h.states[state] = pendingState{expires: time.Now().Add(10 * time.Minute)}
	h.mu.Unlock()
	manifest := map[string]any{
		"name": "AI Workflow Bot", "url": strings.TrimRight(publicURL, "/"), "public": true,
		"redirect_url":        "http://127.0.0.1:8081/setup/callback",
		"hook_attributes":     map[string]any{"url": webhookURL(publicURL), "active": true},
		"default_permissions": map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "checks": "write", "actions": "read", "metadata": "read"},
		"default_events":      []string{"issues", "issue_comment", "pull_request", "push", "check_run", "check_suite", "workflow_run", "pull_request_review"},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not encode App manifest")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := manifestFormTemplate.Execute(w, map[string]string{"Manifest": string(manifestJSON), "State": state}); err != nil {
		http.Error(w, "could not render registration handoff", http.StatusInternalServerError)
	}
}

var manifestFormTemplate = template.Must(template.New("manifest").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>继续 GitHub App 注册</title><style>body{margin:0;min-height:100vh;display:grid;place-items:center;padding:20px;background:#f4f6fa;color:#26344d;font:14px -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC",sans-serif}.card{width:min(520px,100%);padding:28px;background:#fff;border:1px solid #e4e9f1;border-radius:15px;box-shadow:0 8px 25px #1b2f520d}h1{font-size:23px;margin:0 0 9px}.muted{color:#7c889c;font-size:12px;line-height:1.7}.btn{height:40px;padding:0 15px;border:0;border-radius:8px;background:#426bd8;color:#fff;font-weight:650;cursor:pointer}</style></head><body><main class="card"><h1>继续到 GitHub</h1><p class="muted">请确认后提交 App Manifest。GitHub 会创建 App 并返回一次性注册码，控制台随后保存生成的私钥与 Webhook Secret。</p><form method="post" action="https://github.com/settings/apps/new"><input type="hidden" name="manifest" value="{{.Manifest}}"><input type="hidden" name="state" value="{{.State}}"><button class="btn" type="submit">继续到 GitHub 注册</button></form><p class="muted">回调仅在 10 分钟内接受此一次性 state。</p></main></body></html>`))

func normalizePublicURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("public URL must be a valid HTTPS base URL without credentials, query, or fragment")
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(path, "/webhooks/github") {
		path = strings.TrimSuffix(path, "/webhooks/github")
	}
	u.Path = path
	return strings.TrimRight(u.String(), "/"), nil
}

func webhookURL(base string) string { return strings.TrimRight(base, "/") + "/webhooks/github" }

func (h *Handlers) manifestCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	h.mu.Lock()
	pending, ok := h.states[state]
	delete(h.states, state)
	h.mu.Unlock()
	if !ok || state == "" || time.Now().After(pending.expires) || code == "" {
		http.Error(w, "registration callback state is invalid, expired, or incomplete; return to dashboard setup", http.StatusBadRequest)
		return
	}
	cfg := h.engine.Config()
	base := strings.TrimRight(cfg.GitHub.APIURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	endpoint := base + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, nil)
	if err != nil {
		http.Error(w, "could not prepare manifest conversion", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := h.client.Do(req)
	if err != nil {
		http.Error(w, "GitHub manifest conversion request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		http.Error(w, "GitHub rejected manifest conversion", http.StatusBadGateway)
		return
	}
	var manifest gh.Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&manifest); err != nil {
		http.Error(w, "GitHub manifest response was invalid", http.StatusBadGateway)
		return
	}
	if manifest.ID <= 0 || manifest.Pem == "" || manifest.WebhookSecret == "" {
		http.Error(w, "GitHub conversion omitted required App credentials", http.StatusBadGateway)
		return
	}
	configDir, err := filepath.Abs(filepath.Dir(h.configPath))
	if err != nil {
		http.Error(w, "could not resolve config directory", http.StatusInternalServerError)
		return
	}
	if cfg.GitHub.PrivateKeyFile == "" {
		cfg.GitHub.PrivateKeyFile = filepath.Join(configDir, "github-app.pem")
	}
	if cfg.GitHub.WebhookSecretFile == "" {
		cfg.GitHub.WebhookSecretFile = filepath.Join(configDir, "webhook-secret")
	}
	if err := writeSecret(resolveConfigPath(h.configPath, cfg.GitHub.PrivateKeyFile), manifest.Pem); err != nil {
		http.Error(w, "could not securely save App private key", http.StatusInternalServerError)
		return
	}
	if err := writeSecret(resolveConfigPath(h.configPath, cfg.GitHub.WebhookSecretFile), manifest.WebhookSecret); err != nil {
		http.Error(w, "could not securely save Webhook secret", http.StatusInternalServerError)
		return
	}
	cfg.GitHub.AppID = manifest.ID
	if err := h.persistConfig(cfg); err != nil {
		http.Error(w, "App credentials saved but configuration update failed", http.StatusInternalServerError)
		return
	}
	if err := h.engine.SetConfig(r.Context(), cfg); err != nil {
		http.Error(w, "App credentials saved but engine reload failed", http.StatusInternalServerError)
		return
	}
	if manifest.HTMLURL != "" {
		h.mu.Lock()
		h.installURL = strings.TrimRight(manifest.HTMLURL, "/") + "/installations/new"
		h.mu.Unlock()
	}
	http.Redirect(w, r, "/setup?registered=1", http.StatusSeeOther)
}

func randomToken(bytesCount int) (string, error) {
	data := make([]byte, bytesCount)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (h *Handlers) discover(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		app, err := h.engine.App()
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		installations, err := app.ListInstallations(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"installations": installations})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		InstallationID int64 `json:"installation_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid installation selection JSON")
		return
	}
	if body.InstallationID == 0 {
		app, err := h.engine.App()
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		installations, err := app.ListInstallations(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"installations": installations})
		return
	}
	result, err := h.bindInstallation(r.Context(), body.InstallationID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handlers) selectInstallation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if _, ok := h.sessionID(r); !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := h.checkOrigin(r); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/setup?error=Invalid+installation+selection", http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(r.FormValue("installation_id"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/setup?error=Invalid+installation+ID", http.StatusSeeOther)
		return
	}
	if _, err := h.bindInstallation(r.Context(), id); err != nil {
		http.Redirect(w, r, "/setup?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/setup?registered=1", http.StatusSeeOther)
}

func (h *Handlers) bindInstallation(ctx context.Context, installationID int64) (map[string]any, error) {
	app, err := h.engine.App()
	if err != nil {
		return nil, err
	}
	installations, err := app.ListInstallations(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, inst := range installations {
		if inst.ID == installationID {
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("installation is not accessible to this GitHub App")
	}
	client := app.ForInstallation(ctx, installationID)
	available := make(map[string]gh.Repository)
	totalCount := 0
	for page := 1; page <= 100; page++ {
		var repos struct {
			TotalCount   int             `json:"total_count"`
			Repositories []gh.Repository `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		if err := client.Do(ctx, http.MethodGet, path, nil, &repos); err != nil {
			return nil, err
		}
		totalCount = repos.TotalCount
		for _, repo := range repos.Repositories {
			available[repo.FullName] = repo
		}
		if len(repos.Repositories) < 100 {
			break
		}
		if page == 100 {
			return nil, errors.New("installation repository pagination limit exceeded")
		}
	}
	cfg := h.engine.Config()
	cfg.GitHub.InstallationID = installationID
	bindings := make([]map[string]any, 0, len(cfg.Repositories))
	for n := range cfg.Repositories {
		configured := &cfg.Repositories[n]
		remote, ok := available[configured.Name]
		if !ok {
			bindings = append(bindings, map[string]any{"name": configured.Name, "bound": false, "reason": "not selected in GitHub installation"})
			continue
		}
		if configured.ID > 0 && configured.ID != remote.ID {
			bindings = append(bindings, map[string]any{"name": configured.Name, "bound": false, "reason": "configured numeric ID does not match GitHub"})
			continue
		}
		configured.ID = remote.ID
		bindings = append(bindings, map[string]any{"name": configured.Name, "id": remote.ID, "bound": true})
	}
	if err := h.persistConfig(cfg); err != nil {
		return nil, err
	}
	if err := h.engine.SetConfig(ctx, cfg); err != nil {
		return nil, err
	}
	return map[string]any{"installation_id": installationID, "repositories": bindings, "available_repositories": totalCount}, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}
