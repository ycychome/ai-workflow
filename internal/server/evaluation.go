package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
)

var evaluationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,100}$`)

var (
	errEvaluationEvidencePath        = errors.New("评估证据路径越界")
	errEvaluationEvidenceUnavailable = errors.New("评估证据文件不可用")
	errEvaluationEvidenceLimit       = errors.New("评估证据超出传输限制")
	errEvaluationEvidenceDigest      = errors.New("评估证据摘要校验失败")
)

// evaluationSnapshot is an operator-installed receipt, never supplied by an Issue
// or a model. The preparation adapter must hash the complete candidate source.
type evaluationSnapshot struct {
	domain.EvaluationContext
	Repository       string       `json:"repository"`
	RepositoryID     int64        `json:"repository_id"`
	InstallationID   int64        `json:"installation_id"`
	SourceSHA256     string       `json:"source_sha256"`
	BaseSourceSHA256 string       `json:"base_source_sha256"`
	PullRequest      int          `json:"pull_request"`
	CandidateBranch  string       `json:"candidate_branch"`
	Spec             *domain.Spec `json:"spec,omitempty"`
}
type evaluationManifest struct {
	Version string `json:"version"`
	Tasks   []struct {
		ID          string `json:"id"`
		IssuePath   string `json:"issue_path"`
		IssueSHA256 string `json:"issue_sha256"`
		Baseline    string `json:"baseline_source_sha256"`
		Defect      string `json:"defect_source_sha256"`
		Fix         string `json:"fix_source_sha256"`
	} `json:"tasks"`
}

func sha256Text(b []byte) string { x := sha256.Sum256(b); return hex.EncodeToString(x[:]) }
func readEvaluationJSON(path string, target any) ([]byte, error) {
	if path == "" {
		return nil, errors.New("尚未配置受信评估文件")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	if len(b) > 16<<20 {
		return nil, errors.New("评估文件过大")
	}
	e = json.Unmarshal(b, target)
	return b, e
}

// readEvaluationEvidence returns the compact JSON representation whose digest
// is stored in Evidence.Hash. MarshalIndent is used when evidence is saved, so
// hashing its raw pretty-printed file bytes would not match that digest. Compact
// preserves key order, number spelling, and JSON escapes without decoding and
// re-marshaling values through floating point.
func readEvaluationEvidence(dataDir, workflowID string, ev domain.Evidence) ([]byte, error) {
	root, err := filepath.EvalSymlinks(filepath.Join(dataDir, "evidence", workflowID))
	if err != nil {
		return nil, errEvaluationEvidenceUnavailable
	}
	target, err := filepath.EvalSymlinks(ev.Path)
	if err != nil {
		return nil, errEvaluationEvidenceUnavailable
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, errEvaluationEvidencePath
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, errEvaluationEvidenceUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errEvaluationEvidenceUnavailable
	}
	const limit = 32 << 20
	if info.Size() > limit {
		return nil, errEvaluationEvidenceLimit
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, errEvaluationEvidenceUnavailable
	}
	if len(body) > limit {
		return nil, errEvaluationEvidenceLimit
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return nil, errEvaluationEvidenceDigest
	}
	if sha256Text(compact.Bytes()) != ev.Hash {
		return nil, errEvaluationEvidenceDigest
	}
	return compact.Bytes(), nil
}

func (h *Handlers) evaluationAPI(w http.ResponseWriter, r *http.Request) {
	effective, configErr := h.engine.ExecutionConfig(r.Context())
	if configErr != nil {
		writeError(w, 503, "无法读取实际执行配置："+configErr.Error())
		return
	}
	if r.URL.Path == "/api/evaluation/config" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		c := effective
		cfg := effective.EvaluationExecutionConfig()
		writeJSON(w, 200, map[string]any{"protocol": domain.EvaluationProtocolVersion, "config": cfg, "config_sha256": domain.Digest(cfg), "models": map[string]any{"dev": c.Model.Dev, "review": c.Model.Review}, "profiles": []string{"builder_ci", "single_review", "full"}, "human_review": "pending", "human_merge": "pending"})
		return
	}
	if r.URL.Path == "/api/evaluation/runs" && r.Method == http.MethodPost {
		h.createEvaluation(w, r)
		return
	}
	resume := strings.HasSuffix(r.URL.Path, "/resume")
	judge := strings.HasSuffix(r.URL.Path, "/judge")
	trial := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/evaluation/runs/"), "/resume"), "/judge")
	if !evaluationID.MatchString(trial) || (r.Method != http.MethodGet && !((resume || judge) && r.Method == http.MethodPost)) {
		writeError(w, 400, "评估地址或请求方法无效")
		return
	}
	var workflowID string
	err := h.engine.Store.DB.QueryRow(r.Context(), `SELECT workflow_id FROM evaluation_trials WHERE trial_id=$1 AND workflow_id IS NOT NULL`, trial).Scan(&workflowID)
	if err != nil {
		writeError(w, 404, "评估尚未创建工作流；请用原请求恢复创建")
		return
	}
	if judge {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		judgment, err := h.engine.JudgeEvaluation(r.Context(), workflowID, os.Getenv("AI_WORKFLOW_EVAL_MANIFEST"))
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, judgment)
		return
	}
	if resume {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if err := h.engine.ResumeDailyBudget(r.Context(), workflowID); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"resumed": true, "budgets_reset": false})
		return
	}
	wf, err := h.engine.Store.Get(r.Context(), workflowID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if evidenceID := r.URL.Query().Get("evidence_id"); evidenceID != "" {
		for _, ev := range wf.Evidence {
			if ev.ID != evidenceID {
				continue
			}
			data, e := readEvaluationEvidence(h.engine.Config().DataDir, wf.ID, ev)
			if errors.Is(e, errEvaluationEvidencePath) {
				writeError(w, 403, "证据路径越界")
				return
			}
			if errors.Is(e, errEvaluationEvidenceUnavailable) {
				writeError(w, 404, "证据文件不可用")
				return
			}
			if errors.Is(e, errEvaluationEvidenceLimit) {
				writeError(w, 409, "证据文件不可用或超出传输限制")
				return
			}
			if e != nil {
				writeError(w, 409, "证据摘要校验失败")
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(200)
			_, _ = w.Write(data)
			return
		}
		writeError(w, 404, "未找到实际执行证据")
		return
	}
	runs, err := h.engine.Store.ListAgentRuns(r.Context(), wf.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	judgeReady := wf.LeaseOwner == "" || !wf.LeaseUntil.After(time.Now())
	for _, run := range runs {
		if run.Status == domain.AgentActive || run.Status == domain.AgentStopping {
			judgeReady = false
		}
	}
	cfg := effective
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	var daily int
	err = h.engine.Store.DB.QueryRow(r.Context(), `SELECT COALESCE((SELECT calls FROM daily_usage WHERE day=$1),0)`, day).Scan(&daily)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"protocol": domain.EvaluationProtocolVersion, "workflow": wf, "agent_runs": runs, "judge_ready": judgeReady, "config": effective.EvaluationExecutionConfig(), "config_sha256": domain.Digest(effective.EvaluationExecutionConfig()), "models": map[string]string{"dev": cfg.Model.Dev, "review": cfg.Model.Review}, "daily_calls": daily, "daily_day": day, "daily_limit": cfg.Budget.DailyCalls, "cost": map[string]any{"currency": "USD", "estimated": true, "value": (float64(wf.InputTokens)*cfg.Budget.InputPrice + float64(wf.OutputTokens)*cfg.Budget.OutputPrice) / 1e6}, "issue_url": fmt.Sprintf("https://github.com/%s/issues/%d", wf.Repository, wf.Issue), "pull_request_url": evaluationPRURL(wf), "human_review": "pending", "human_merge": "pending"})
}
func evaluationPRURL(w domain.Workflow) string {
	if w.PR == 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", w.Repository, w.PR)
}

func (h *Handlers) createEvaluation(w http.ResponseWriter, r *http.Request) {
	effective, configErr := h.engine.ExecutionConfig(r.Context())
	if configErr != nil {
		writeError(w, 503, "无法读取实际执行配置："+configErr.Error())
		return
	}
	var req struct {
		TrialID        string `json:"trial_id"`
		ConfigDigest   string `json:"config_digest"`
		ManifestDigest string `json:"manifest_digest"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	fail := func(e error) { writeError(w, http.StatusConflict, e.Error()) }
	if !evaluationID.MatchString(req.TrialID) || req.ConfigDigest != domain.Digest(effective.EvaluationExecutionConfig()) {
		fail(errors.New("评估 ID 无效或实际配置已变化"))
		return
	}
	var manifest evaluationManifest
	manifestPath := os.Getenv("AI_WORKFLOW_EVAL_MANIFEST")
	raw, err := readEvaluationJSON(manifestPath, &manifest)
	if err != nil {
		fail(err)
		return
	}
	if sha256Text(raw) != req.ManifestDigest {
		fail(errors.New("冻结清单摘要不匹配"))
		return
	}
	var snapshots struct {
		Trials []evaluationSnapshot `json:"trials"`
	}
	_, err = readEvaluationJSON(os.Getenv("AI_WORKFLOW_EVAL_SNAPSHOTS"), &snapshots)
	if err != nil {
		fail(err)
		return
	}
	var snap *evaluationSnapshot
	for i := range snapshots.Trials {
		if snapshots.Trials[i].TrialID == req.TrialID {
			if snap != nil {
				fail(errors.New("受信快照包含重复试验 ID"))
				return
			}
			snap = &snapshots.Trials[i]
		}
	}
	if snap == nil || !domain.ValidEvaluationProfile(snap.Profile) || snap.ManifestDigest != req.ManifestDigest || snap.ManifestVersion != manifest.Version || snap.ConfigDigest != req.ConfigDigest {
		fail(errors.New("没有匹配的受信试验快照"))
		return
	}
	if snap.Kind != "smoke" && snap.Kind != "development" && snap.Kind != "review" {
		fail(errors.New("未知试验类型"))
		return
	}
	var issueBody string
	for _, task := range manifest.Tasks {
		if task.ID != snap.TaskID {
			continue
		}
		path := filepath.Clean(task.IssuePath)
		if filepath.IsAbs(path) || strings.HasPrefix(path, "..") {
			fail(errors.New("清单题面路径无效"))
			return
		}
		b, e := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), path))
		if e != nil || sha256Text(b) != task.IssueSHA256 {
			fail(errors.New("冻结题面校验失败"))
			return
		}
		issueBody = string(b)
		expected := task.Defect
		if snap.CandidateID == task.ID+"-fix" {
			expected = task.Fix
		} else if snap.CandidateID != task.ID+"-defect" {
			fail(errors.New("候选 ID 不在冻结清单中"))
			return
		}
		if snap.SourceSHA256 != expected || (snap.Kind == "review" && snap.BaseSourceSHA256 != task.Baseline) {
			fail(errors.New("快照源码摘要不匹配"))
			return
		}
	}
	if issueBody == "" || snap.BaseBranch == "" || len(snap.BaseSHA) != 40 || len(snap.CandidateSHA) != 40 {
		fail(errors.New("冻结题面或 Git 快照不完整"))
		return
	}
	if snap.Kind != "review" && snap.CandidateSHA != snap.BaseSHA {
		fail(errors.New("开发试验必须从独立缺陷基线开始"))
		return
	}
	cfg := effective
	repo, ok := cfg.Repository(snap.Repository)
	if !ok || repo.ID != snap.RepositoryID || snap.InstallationID != cfg.GitHub.InstallationID || cfg.Runner.Mode != "responses" {
		fail(errors.New("仓库、App 安装或真实模型运行模式未授权"))
		return
	}
	parts := strings.Split(snap.Repository, "/")
	if len(parts) != 2 {
		fail(errors.New("仓库格式无效"))
		return
	}
	app, err := h.engine.App()
	if err != nil {
		fail(err)
		return
	}
	client := app.ForInstallation(r.Context(), snap.InstallationID)
	remote, err := client.Repository(r.Context(), parts[0], parts[1])
	if err != nil {
		fail(err)
		return
	}
	if remote.ID != snap.RepositoryID || remote.FullName != snap.Repository {
		fail(errors.New("远端仓库身份不匹配"))
		return
	}
	base, err := client.GetBranchSHA(r.Context(), parts[0], parts[1], snap.BaseBranch)
	if err != nil {
		fail(err)
		return
	}
	if base != snap.BaseSHA {
		fail(errors.New("试验基线分支已变化"))
		return
	}
	var pr *gh.PullRequest
	if snap.Kind == "review" {
		if snap.Spec == nil || snap.PullRequest <= 0 || snap.CandidateBranch == "" {
			fail(errors.New("审查试验需要冻结 Spec 和真实候选 PR"))
			return
		}
		p, e := client.GetPullRequest(r.Context(), parts[0], parts[1], snap.PullRequest)
		if e != nil {
			fail(e)
			return
		}
		if p.State != "open" || p.Merged || p.Head.SHA != snap.CandidateSHA || p.Base.SHA != snap.BaseSHA || p.Head.Ref != snap.CandidateBranch || p.Base.Ref != snap.BaseBranch || p.Head.Repo == nil || p.Base.Repo == nil || p.Head.Repo.ID != repo.ID || p.Base.Repo.ID != repo.ID {
			fail(errors.New("候选 PR 身份或冻结 head/base 不匹配"))
			return
		}
		pr = &p
	}
	marker := "<!-- ai-workflow-evaluation-trial:" + req.TrialID + " -->"
	title := snap.TaskID + " 评估任务"
	body := marker + "\n\n" + issueBody
	if snap.Kind == "review" && snap.Spec.SourceHash != domain.IssueSourceHash(title, body) {
		fail(errors.New("冻结 Spec 必须精确绑定含评估标记的题面 source hash"))
		return
	}
	// Serialize creation and retain an unknown-outcome reservation before GitHub.
	// Retrying CREATING only searches the marker; it never creates a second Issue.
	conn, err := h.engine.Store.DB.Acquire(r.Context())
	if err != nil {
		fail(err)
		return
	}
	defer conn.Release()
	_, err = conn.Exec(r.Context(), `CREATE TABLE IF NOT EXISTS evaluation_trials(trial_id text PRIMARY KEY,request_digest text NOT NULL,state text NOT NULL,workflow_id text,issue integer,created_at timestamptz NOT NULL DEFAULT now())`)
	if err != nil {
		fail(err)
		return
	}
	tx, err := conn.Begin(r.Context())
	if err != nil {
		fail(err)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "evaluation:"+req.TrialID)
	if err != nil {
		fail(err)
		return
	}
	digest := domain.Digest(snap)
	var saved, state string
	var workflowID *string
	var issueNumber *int
	err = tx.QueryRow(r.Context(), `SELECT request_digest,state,workflow_id,issue FROM evaluation_trials WHERE trial_id=$1`, req.TrialID).Scan(&saved, &state, &workflowID, &issueNumber)
	fresh := false
	if err != nil {
		var exists bool
		if e := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM evaluation_trials WHERE trial_id=$1)`, req.TrialID).Scan(&exists); e != nil {
			fail(e)
			return
		}
		if exists {
			fail(err)
			return
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO evaluation_trials(trial_id,request_digest,state) VALUES($1,$2,'CREATING')`, req.TrialID, digest)
		fresh = true
	} else if saved != digest {
		fail(errors.New("试验 ID 已绑定另一份不可变请求"))
		return
	}
	if err != nil {
		fail(err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(err)
		return
	}
	if workflowID != nil {
		writeJSON(w, 200, map[string]any{"trial_id": req.TrialID, "workflow_id": *workflowID})
		return
	}
	var issue gh.Issue
	if issueNumber != nil {
		issue, err = client.GetIssue(r.Context(), parts[0], parts[1], *issueNumber)
	} else if fresh {
		err = client.Do(r.Context(), http.MethodPost, "/repos/"+snap.Repository+"/issues", map[string]any{"title": title, "body": body}, &issue)
	} else {
		for page := 1; page <= 20; page++ {
			var list []gh.Issue
			list, err = client.ListOpenIssuesPage(r.Context(), parts[0], parts[1], page)
			if err != nil {
				break
			}
			for _, item := range list {
				if item.PullRequest == nil && strings.HasPrefix(item.Body, marker+"\n") {
					if issue.Number != 0 {
						fail(errors.New("检测到重复评估 Issue，需人工处理"))
						return
					}
					issue = item
				}
			}
			if len(list) < 100 {
				break
			}
		}
		if err == nil && issue.Number == 0 {
			err = errors.New("创建结果未知且尚未找到评估 Issue；保留预留记录，需人工确认，禁止盲目重建")
		}
	}
	if err != nil {
		fail(err)
		return
	}
	if issue.Title != title || issue.Body != body || issue.State != "open" {
		fail(errors.New("评估 Issue 已编辑或关闭"))
		return
	}
	_, err = conn.Exec(r.Context(), `UPDATE evaluation_trials SET issue=$2,state='ISSUE_CREATED' WHERE trial_id=$1 AND request_digest=$3`, req.TrialID, issue.Number, digest)
	if err != nil {
		fail(err)
		return
	}
	if err = conn.QueryRow(r.Context(), `SELECT created_at FROM evaluation_trials WHERE trial_id=$1`, req.TrialID).Scan(&snap.CreatedAt); err != nil {
		fail(err)
		return
	}
	wf, err := h.engine.CreateEvaluationWorkflow(r.Context(), repo, snap.InstallationID, issue, snap.EvaluationContext, snap.Spec, pr)
	if err != nil {
		fail(err)
		return
	}
	_, err = conn.Exec(r.Context(), `UPDATE evaluation_trials SET workflow_id=$2,state='CREATED' WHERE trial_id=$1 AND request_digest=$3`, req.TrialID, wf.ID, digest)
	if err != nil {
		fail(err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"trial_id": req.TrialID, "workflow_id": wf.ID})
}
