package orchestration

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	gh "github.com/ycychome/ai-workflow/internal/github"
)

var ErrCIReceiptSuperseded = errors.New("CI 已产生更新的运行重试，需要重新签发")

// TrustedCIPolicy 必须来自控制平面的受信配置，不能从 PR、模型输出或候选文件加载。
// SourceCommitSHA/SourceBlobSHA 固定经过人工审查的工作流来源；ReceiptPublicKey 对应
// 不向候选代码暴露签名能力的独立 CI 控制器，不能使用 PR 作业环境中的普通密钥。
type TrustedCIPolicy struct {
	RepositoryID        int64  `json:"repository_id"`
	Repository          string `json:"repository"`
	WorkflowID          int64  `json:"workflow_id"`
	WorkflowPath        string `json:"workflow_path"`
	SourceCommitSHA     string `json:"source_commit_sha"`
	SourceBlobSHA       string `json:"source_blob_sha"`
	ReceiptPublicKey    string `json:"receipt_public_key"`
	MaxRecordAgeSeconds int64  `json:"max_record_age_seconds"`
}

// CICandidateIdentity 固定当前验证轮次及候选。RunAttempt 不得使用“任一成功重试”。
type CICandidateIdentity struct {
	RepositoryID    int64     `json:"repository_id"`
	PullRequest     int       `json:"pull_request"`
	Generation      int64     `json:"generation"`
	VerificationKey string    `json:"verification_key"`
	HeadSHA         string    `json:"head_sha"`
	BaseSHA         string    `json:"base_sha"`
	TestMergeSHA    string    `json:"test_merge_sha"`
	TreeSHA         string    `json:"tree_sha"`
	RunID           int64     `json:"run_id"`
	RunAttempt      int64     `json:"run_attempt"`
	NotBefore       time.Time `json:"not_before"`
}

// CIExecutionReceipt 是可信执行器实际 checkout 与测试结束后的回执，不是模型结论。
// Signature 对 ReceiptPayload 的 JSON 字节签名，禁止把未验证的 artifact JSON 直接转换为此证据。
type CIReceiptPayload struct {
	Schema            string              `json:"schema"`
	Candidate         CICandidateIdentity `json:"candidate"`
	WorkflowID        int64               `json:"workflow_id"`
	WorkflowPath      string              `json:"workflow_path"`
	WorkflowSourceSHA string              `json:"workflow_source_sha"`
	WorkflowBlobSHA   string              `json:"workflow_blob_sha"`
	Conclusion        string              `json:"conclusion"`
	CompletedAt       time.Time           `json:"completed_at"`
}
type SignedCIReceipt struct {
	Payload   CIReceiptPayload `json:"payload"`
	Signature string           `json:"signature"`
}

// CIReceiptSigningBytes 为签名方提供确定性的结构体编码；它不读取或持有私钥。
func CIReceiptSigningBytes(payload CIReceiptPayload) ([]byte, error) { return json.Marshal(payload) }

// TrustedCIRecord 可持久化入证据表，使用前需 Current 检查，最终发布前重新读取远端。
type TrustedCIRecord struct {
	Candidate         CICandidateIdentity `json:"candidate"`
	PolicyDigest      string              `json:"policy_digest"`
	ReceiptDigest     string              `json:"receipt_digest"`
	WorkflowID        int64               `json:"workflow_id"`
	WorkflowPath      string              `json:"workflow_path"`
	WorkflowSourceSHA string              `json:"workflow_source_sha"`
	WorkflowBlobSHA   string              `json:"workflow_blob_sha"`
	Status            string              `json:"status"`
	Conclusion        string              `json:"conclusion"`
	Verdict           string              `json:"verdict"`
	Reason            string              `json:"reason"`
	ObservedAt        time.Time           `json:"observed_at"`
	CompletedAt       time.Time           `json:"completed_at"`
	RunURL            string              `json:"run_url"`
}

func ciDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func ciSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}
func (p TrustedCIPolicy) Validate() error {
	parts := strings.Split(p.Repository, "/")
	if p.RepositoryID <= 0 || p.WorkflowID <= 0 || len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(p.Repository, "\\\r\n") {
		return errors.New("可信 CI 仓库或工作流身份不完整")
	}
	if !strings.HasPrefix(p.WorkflowPath, ".github/workflows/") || path.Clean(p.WorkflowPath) != p.WorkflowPath || strings.ContainsAny(p.WorkflowPath, "\\\r\n@") || (!strings.HasSuffix(p.WorkflowPath, ".yml") && !strings.HasSuffix(p.WorkflowPath, ".yaml")) {
		return errors.New("可信 CI 工作流路径无效")
	}
	if !ciSHA(p.SourceCommitSHA) || !ciSHA(p.SourceBlobSHA) || p.MaxRecordAgeSeconds <= 0 {
		return errors.New("可信 CI 来源提交、文件摘要或时效策略缺失")
	}
	key, err := base64.StdEncoding.DecodeString(p.ReceiptPublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("可信 CI 回执公钥无效")
	}
	return nil
}
func (c CICandidateIdentity) Validate() error {
	if c.RepositoryID <= 0 || c.PullRequest <= 0 || c.Generation <= 0 || c.VerificationKey == "" || c.RunID <= 0 || c.RunAttempt <= 0 || c.NotBefore.IsZero() {
		return errors.New("CI 候选、运行或验证轮次身份不完整")
	}
	if !ciSHA(c.HeadSHA) || !ciSHA(c.BaseSHA) || !ciSHA(c.TestMergeSHA) || !ciSHA(c.TreeSHA) || c.HeadSHA == c.BaseSHA || c.TestMergeSHA == c.HeadSHA || c.TestMergeSHA == c.BaseSHA {
		return errors.New("CI 候选缺少独立且完整的 head/base/test-merge/tree SHA")
	}
	return nil
}
func sameCICandidate(a, b CICandidateIdentity) bool {
	return a.RepositoryID == b.RepositoryID && a.PullRequest == b.PullRequest && a.Generation == b.Generation && a.VerificationKey == b.VerificationKey && a.HeadSHA == b.HeadSHA && a.BaseSHA == b.BaseSHA && a.TestMergeSHA == b.TestMergeSHA && a.TreeSHA == b.TreeSHA && a.RunID == b.RunID && a.RunAttempt == b.RunAttempt && a.NotBefore.Equal(b.NotBefore)
}
func verifyCIReceipt(p TrustedCIPolicy, c CICandidateIdentity, r SignedCIReceipt, now time.Time) error {
	if r.Payload.Schema != "ai-workflow-ci-v1" || !sameCICandidate(r.Payload.Candidate, c) || r.Payload.WorkflowID != p.WorkflowID || r.Payload.WorkflowPath != p.WorkflowPath || r.Payload.WorkflowSourceSHA != c.TestMergeSHA || r.Payload.WorkflowBlobSHA != p.SourceBlobSHA {
		return errors.New("CI 回执与当前候选或工作流来源不匹配")
	}
	if r.Payload.Conclusion != "success" || r.Payload.CompletedAt.IsZero() || r.Payload.CompletedAt.Before(c.NotBefore) || r.Payload.CompletedAt.After(now) {
		return errors.New("CI 回执未成功完成或完成时间无效")
	}
	key, err := base64.StdEncoding.DecodeString(p.ReceiptPublicKey)
	if err != nil {
		return err
	}
	signature, err := base64.StdEncoding.DecodeString(r.Signature)
	if err != nil {
		return errors.New("CI 回执签名编码无效")
	}
	body, err := CIReceiptSigningBytes(r.Payload)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(key), body, signature) {
		return errors.New("CI 回执不是受信执行器签发")
	}
	return nil
}

// CIIdentityClient 只暴露读取能力，可由 GitHub InstallationClient 或确定性测试替身实现。
type CIIdentityClient interface {
	Repository(context.Context, string, string) (gh.Repository, error)
	GetActionsWorkflow(context.Context, string, string, int64) (gh.ActionsWorkflow, error)
	GetActionsRun(context.Context, string, string, int64) (gh.ActionsRun, error)
	GetActionsRunAttempt(context.Context, string, string, int64, int64) (gh.ActionsRun, error)
	GetRepositoryFileIdentity(context.Context, string, string, string, string) (gh.RepositoryFileIdentity, error)
	GetCommitInfo(context.Context, string, string, string) (gh.CommitInfo, error)
	GetCIPullRequestSnapshot(context.Context, string, string, int) (gh.CIPullRequestSnapshot, error)
}

// FetchTrustedCIRecord 对当前 PR、最新 run attempt、来源文件及 test-merge 对账。
// 任何网络错误或身份不完整均返回非 PASS；初次排队/正在运行与结论未知均不能通过。
func FetchTrustedCIRecord(ctx context.Context, client CIIdentityClient, p TrustedCIPolicy, c CICandidateIdentity, receipt SignedCIReceipt, now time.Time) (TrustedCIRecord, error) {
	record := TrustedCIRecord{Candidate: c, PolicyDigest: ciDigest(p), Verdict: "INVALID", ObservedAt: now}
	reject := func(reason string) (TrustedCIRecord, error) {
		record.Reason = reason
		return record, errors.New(reason)
	}
	if err := p.Validate(); err != nil {
		return reject(err.Error())
	}
	if err := c.Validate(); err != nil {
		return reject(err.Error())
	}
	if client == nil || now.IsZero() || c.RepositoryID != p.RepositoryID || c.NotBefore.After(now) {
		return reject("CI 调用缺少可信客户端或匹配的候选身份")
	}
	if err := verifyCIReceipt(p, c, receipt, now); err != nil {
		return reject(err.Error())
	}
	parts := strings.Split(p.Repository, "/")
	owner, repo := parts[0], parts[1]
	repository, err := client.Repository(ctx, owner, repo)
	if err != nil {
		return reject("无法读取 CI 仓库身份")
	}
	if repository.ID != p.RepositoryID || !strings.EqualFold(repository.FullName, p.Repository) {
		return reject("CI 仓库数字身份或名称发生变化")
	}
	pr, err := client.GetCIPullRequestSnapshot(ctx, owner, repo, c.PullRequest)
	if err != nil {
		return reject("无法读取当前 PR 身份")
	}
	if pr.Mergeable == nil || !*pr.Mergeable || pr.MergeCommitSHA != c.TestMergeSHA || pr.Number != c.PullRequest || pr.State != "open" || pr.Merged || pr.Head.SHA != c.HeadSHA || pr.Base.SHA != c.BaseSHA || pr.Head.Repo == nil || pr.Base.Repo == nil || pr.Head.Repo.ID != p.RepositoryID || pr.Base.Repo.ID != p.RepositoryID {
		return reject("PR 已变化或来自不受信的 fork 来源")
	}
	latest, err := client.GetActionsRun(ctx, owner, repo, c.RunID)
	if err != nil {
		return reject("无法读取 CI 最新运行状态")
	}
	run, err := client.GetActionsRunAttempt(ctx, owner, repo, c.RunID, c.RunAttempt)
	if err != nil {
		return reject("无法读取 CI 指定重试记录")
	}
	if latest.RunAttempt > c.RunAttempt {
		updated := c
		updated.RunAttempt = latest.RunAttempt
		if err := validateActionsRun(p, updated, latest); err == nil {
			if latest.Status == "completed" && latest.Conclusion != "success" {
				return reject("CI 最新重试未成功完成")
			}
			record.Reason = ErrCIReceiptSuperseded.Error()
			return record, ErrCIReceiptSuperseded
		}
	}
	if latest.RunAttempt != c.RunAttempt || latest.ID != c.RunID || latest.WorkflowID != p.WorkflowID {
		return reject("CI 记录属于旧重试或不同工作流")
	}
	if err = validateActionsRun(p, c, run); err != nil {
		return reject(err.Error())
	}
	if err = validateActionsRun(p, c, latest); err != nil {
		return reject("CI 最新记录不匹配：" + err.Error())
	}
	record.Status = run.Status
	record.Conclusion = run.Conclusion
	record.RunURL = run.HTMLURL
	if run.Status != "completed" || latest.Status != "completed" {
		record.Verdict = "PENDING"
		record.Reason = "CI 尚未完成"
		return record, nil
	}
	if run.Conclusion != "success" || latest.Conclusion != "success" {
		record.Verdict = "FAIL"
		record.Reason = "CI 结论不是成功"
		return record, nil
	}
	if receipt.Payload.CompletedAt.Before(run.UpdatedAt) || run.UpdatedAt.After(now) {
		return reject("CI 回执与 API 完成时间不一致")
	}
	workflow, err := client.GetActionsWorkflow(ctx, owner, repo, p.WorkflowID)
	if err != nil {
		return reject("无法读取可信工作流注册身份")
	}
	if workflow.ID != p.WorkflowID || workflow.Path != p.WorkflowPath || workflow.State != "active" {
		return reject("可信工作流路径、编号或启用状态发生变化")
	}
	// 同时核验受信来源和实际执行 merge 的工作流 blob；PR 同名替换不能继承信任。
	for _, ref := range []string{p.SourceCommitSHA, c.BaseSHA, c.HeadSHA, c.TestMergeSHA} {
		identity, err := client.GetRepositoryFileIdentity(ctx, owner, repo, p.WorkflowPath, ref)
		if err != nil {
			return reject("无法核验工作流来源文件")
		}
		if identity.Type != "file" || identity.Path != p.WorkflowPath || identity.SHA != p.SourceBlobSHA {
			return reject("工作流来源文件与受信版本不一致")
		}
	}
	merge, err := client.GetCommitInfo(ctx, owner, repo, c.TestMergeSHA)
	if err != nil {
		return reject("无法核验 test-merge 提交")
	}
	if merge.SHA != c.TestMergeSHA || merge.TreeSHA != c.TreeSHA || len(merge.Parents) != 2 || !((merge.Parents[0] == c.BaseSHA && merge.Parents[1] == c.HeadSHA) || (merge.Parents[1] == c.BaseSHA && merge.Parents[0] == c.HeadSHA)) {
		return reject("test-merge 的提交、树或父提交身份不匹配")
	}
	record.ReceiptDigest = ciDigest(receipt)
	record.WorkflowID = p.WorkflowID
	record.WorkflowPath = p.WorkflowPath
	record.WorkflowSourceSHA = c.TestMergeSHA
	record.WorkflowBlobSHA = p.SourceBlobSHA
	record.CompletedAt = receipt.Payload.CompletedAt
	record.Verdict = "PASS"
	record.Reason = "可信 CI 来源、执行回执与当前候选身份均匹配"
	return record, nil
}
func validateActionsRun(p TrustedCIPolicy, c CICandidateIdentity, r gh.ActionsRun) error {
	if r.ID != c.RunID || r.RunAttempt != c.RunAttempt || r.WorkflowID != p.WorkflowID || r.Path != p.WorkflowPath || r.Event != "pull_request" || r.HeadSHA != c.HeadSHA || r.Repository.ID != p.RepositoryID || r.HeadRepository.ID != p.RepositoryID || !strings.EqualFold(r.Repository.FullName, p.Repository) || !strings.EqualFold(r.HeadRepository.FullName, p.Repository) {
		return errors.New("CI 运行来源、事件或 head 身份不匹配")
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) || r.CreatedAt.Before(c.NotBefore) {
		return errors.New("CI 运行时间缺失或早于当前验证轮次")
	}
	if len(r.PullRequests) != 1 {
		return errors.New("CI 运行没有唯一、完整的关联 PR")
	}
	pr := r.PullRequests[0]
	if pr.Number != c.PullRequest || pr.Head.SHA != c.HeadSHA || pr.Base.SHA != c.BaseSHA || pr.Head.Repo.ID != p.RepositoryID || pr.Base.Repo.ID != p.RepositoryID {
		return errors.New("CI 关联 PR 的 head/base/仓库不匹配")
	}
	return nil
}

// Current 仅检查已记录证据仍绑定当前控制平面身份；不能替代最终发布前远端重读。
func (r TrustedCIRecord) Current(p TrustedCIPolicy, c CICandidateIdentity, now time.Time) bool {
	return p.Validate() == nil && c.Validate() == nil && r.Verdict == "PASS" && r.Status == "completed" && r.Conclusion == "success" && r.PolicyDigest == ciDigest(p) && sameCICandidate(r.Candidate, c) && r.ReceiptDigest != "" && r.WorkflowID == p.WorkflowID && r.WorkflowPath == p.WorkflowPath && r.WorkflowSourceSHA == c.TestMergeSHA && r.WorkflowBlobSHA == p.SourceBlobSHA && !r.ObservedAt.IsZero() && !r.ObservedAt.After(now) && now.Sub(r.ObservedAt) <= time.Duration(p.MaxRecordAgeSeconds)*time.Second && !r.CompletedAt.IsZero() && !r.CompletedAt.After(r.ObservedAt)
}

// HighRiskChange 只表达额外审查义务，不授予改动或发布权限。
type HighRiskChange struct {
	Path     string `json:"path"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

// ClassifyHighRiskPaths 在策略层之外提供默认风险分类。调用者还必须检查删除与重命名前路径。
func ClassifyHighRiskPaths(paths []string) ([]HighRiskChange, error) {
	out := []HighRiskChange{}
	seen := map[string]bool{}
	for _, name := range paths {
		if name == "" || path.Clean(name) != name || name == "." || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\r\n") {
			return nil, fmt.Errorf("变更路径不规范：%q", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		lower := strings.ToLower(name)
		category, reason := "", ""
		switch {
		case strings.HasPrefix(lower, ".github/workflows/") || strings.HasPrefix(lower, ".github/actions/"):
			category = "ci_trust"
			reason = "工作流或本地 Action 影响可信验证来源"
		case strings.Contains("/"+lower, "/migrations/") || strings.HasSuffix(lower, "schema.sql"):
			category = "data_migration"
			reason = "结构变更需要兼容性、数据保留与回滚审查"
		case strings.Contains("/"+lower, "/auth/") || strings.Contains("/"+lower, "/permissions/") || strings.Contains(lower, "authorization") || strings.Contains(lower, "tenant"):
			category = "authorization"
			reason = "身份、租户或权限改动需要越权反例验证"
		case strings.Contains(lower, "policy") || strings.Contains(lower, "guard") || strings.Contains(lower, "sandbox") || strings.HasPrefix(lower, "configs/"):
			category = "control_policy"
			reason = "控制策略改动不能由候选代码自行降低门禁"
		case path.Base(lower) == "go.mod" || path.Base(lower) == "go.sum" || path.Base(lower) == "package-lock.json" || path.Base(lower) == "cargo.lock":
			category = "dependencies"
			reason = "依赖变更需要来源、版本与构建可重复性审查"
		}
		if category != "" {
			out = append(out, HighRiskChange{name, category, reason})
		}
	}
	return out, nil
}
