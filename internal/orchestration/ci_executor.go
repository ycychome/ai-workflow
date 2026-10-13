package orchestration

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

// CIExecutorOptions 只接受工作流和远端运行编号；候选、命令和输出路径由控制平面推导。
// 私钥文件仅由宿主读取，永不传入沙箱、环境变量或候选输出。
var ErrCIExecutorBusy = errors.New("可信签发器仍在运行")

type CIExecutorOptions struct {
	WorkflowID     string
	RunID          int64
	PrivateKeyFile string
}
type CIExecutorResult struct {
	RunID           string `json:"run_id"`
	VerificationKey string `json:"verification_key"`
	ReceiptDigest   string `json:"receipt_digest"`
	EvidenceID      string `json:"evidence_id"`
}
type ciExecutorJournal struct {
	RunID      string    `json:"run_id"`
	WorkflowID string    `json:"workflow_id"`
	Status     string    `json:"status"`
	UpdatedAt  time.Time `json:"updated_at"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
}

func readCIExecutorKey(filename, public string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return nil, errors.New("签发密钥不可读、权限过宽或格式无效")
	}
	body, err := os.ReadFile(filename)
	if err != nil {
		return nil, errors.New("无法读取签发密钥")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, errors.New("签发密钥必须是受保护的 Ed25519 私钥编码")
	}
	key := ed25519.PrivateKey(decoded)
	expected, err := base64.StdEncoding.DecodeString(public)
	if err != nil || !key.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(expected)) {
		return nil, errors.New("签发密钥与受信策略公钥不匹配")
	}
	return key, nil
}
func writeCIExecutorJSON(filename string, value any) error {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("无法准备可信回执目录")
	}
	file, err := os.CreateTemp(dir, ".receipt-*")
	if err != nil {
		return errors.New("无法创建可信回执暂存文件")
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0600); err == nil {
		err = json.NewEncoder(file).Encode(value)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("无法持久化可信回执")
	}
	if err = os.Rename(temporary, filename); err != nil {
		return errors.New("无法原子替换可信回执")
	}
	directory, err := os.Open(dir)
	if err != nil {
		return errors.New("无法同步可信回执目录")
	}
	defer directory.Close()
	if err = directory.Sync(); err != nil {
		return errors.New("无法同步可信回执目录")
	}
	return nil
}

type ciCommandRunner interface {
	Run(context.Context, *sandbox.Workspace, []string, time.Duration) (sandbox.RunResult, error)
}

func runCIExecutorCommands(ctx context.Context, runner ciCommandRunner, workspace *sandbox.Workspace, commands [][]string, timeout time.Duration) ([]sandbox.RunResult, error) {
	if len(commands) == 0 || timeout <= 0 {
		return nil, errors.New("可信验证命令或执行期限缺失")
	}
	results := make([]sandbox.RunResult, 0, len(commands))
	for _, command := range commands {
		if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
			return results, errors.New("可信验证配置含空命令")
		}
		result, err := runner.Run(ctx, workspace, append([]string(nil), command...), timeout)
		results = append(results, result)
		if err != nil {
			return results, errors.New("可信沙箱验证执行失败")
		}
		if result.TimedOut || result.ExitCode != 0 {
			return results, errors.New("可信沙箱验证未成功完成")
		}
	}
	return results, nil
}

// ExecuteTrustedCIReceipt 不提供任意 JSON 签名入口。真实沙箱成功且停止确认后才签名；
// API、最新 attempt、工作流源码、当前候选任一变化都禁止保存回执。
func (e *Engine) ExecuteTrustedCIReceipt(ctx context.Context, options CIExecutorOptions) (out CIExecutorResult, retErr error) {
	if options.WorkflowID == "" || options.RunID <= 0 {
		return out, errors.New("必须指定当前工作流及 CI 运行编号")
	}
	w, err := e.Store.Get(ctx, options.WorkflowID)
	if err != nil {
		return out, errors.New("无法读取目标工作流")
	}
	if !ciExecutorWorkflowReady(w) {
		return out, errors.New("可信 CI 签发必须等待固定候选检查点且无有效工作流租约")
	}
	if err = e.checkEvaluationConfig(ctx, w); err != nil {
		return out, err
	}
	used, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil {
		return out, errors.New("无法读取 CI 签发时间预算")
	}
	limit := int64(e.Config().Budget.IssueMinutes) * 60
	if used >= limit {
		return out, errors.New("Issue 执行时间预算已耗尽，不能启动可信 CI 签发")
	}
	repo := e.repository(w)
	configured := repo.TrustedCI
	if configured == nil {
		return out, errors.New("仓库尚未配置可信 CI 身份")
	}
	policy := TrustedCIPolicy{RepositoryID: configured.RepositoryID, Repository: configured.Repository, WorkflowID: configured.WorkflowID, WorkflowPath: configured.WorkflowPath, SourceCommitSHA: configured.SourceCommitSHA, SourceBlobSHA: configured.SourceBlobSHA, ReceiptPublicKey: configured.ReceiptPublicKey, MaxRecordAgeSeconds: configured.MaxRecordAgeSeconds}
	if err = policy.Validate(); err != nil {
		return out, err
	}
	if policy.RepositoryID != w.RepositoryID || policy.Repository != w.Repository || w.Control != "ACTIVE" || !w.Spec.Approved {
		return out, errors.New("当前工作流身份或批准状态不允许签发")
	}
	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil {
		return out, errors.New("无法确认候选 Agent 已停止")
	}
	for _, run := range runs {
		if run.Status == domain.AgentActive || run.Status == domain.AgentStopping || (!run.StartedAt.IsZero() && run.StoppedAt.IsZero()) {
			return out, errors.New("候选 Agent 尚未停止确认，不能启动独立 CI 签发")
		}
	}
	key, err := readCIExecutorKey(options.PrivateKeyFile, policy.ReceiptPublicKey)
	if err != nil {
		return out, err
	}
	defer clear(key)
	client, owner, name, err := e.client(ctx, w)
	if err != nil {
		return out, errors.New("无法初始化受信 GitHub 客户端")
	}
	run, err := client.GetActionsRun(ctx, owner, name, options.RunID)
	if err != nil {
		return out, errors.New("无法读取指定 CI 运行")
	}
	candidate := CICandidateIdentity{RepositoryID: w.RepositoryID, PullRequest: w.PR, Generation: w.Generation, VerificationKey: e.key(ctx, w), HeadSHA: w.HeadSHA, BaseSHA: w.BaseSHA, TestMergeSHA: w.TestedSHA, TreeSHA: w.TestedTreeSHA, RunID: run.ID, RunAttempt: run.RunAttempt, NotBefore: w.CreatedAt}
	if err = candidate.Validate(); err != nil {
		return out, err
	}
	if err = validateActionsRun(policy, candidate, run); err != nil {
		return out, err
	}
	if run.Status != "completed" || run.Conclusion != "success" {
		return out, errors.New("指定 CI 最新运行尚未成功完成")
	}
	// 独立签发器锁不占用模型运行槽；同工作流跨进程互斥，持锁后才能回收旧 journal。
	connection, err := e.Store.DB.Acquire(ctx)
	if err != nil {
		return out, errors.New("无法取得签发器锁连接")
	}
	defer connection.Release()
	var locked bool
	if err = connection.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, trustedExecutionLockKey(w.ID)).Scan(&locked); err != nil || !locked {
		return out, errors.New("目标工作流已有可信签发器运行")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := connection.Exec(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, trustedExecutionLockKey(w.ID)); err != nil {
			_ = connection.Hijack().Close(cleanup)
			retErr = errors.Join(retErr, errors.New("可信签发器锁释放失败"))
		}
	}()
	if err = e.requireOtherTrustedExecutionSettled(ctx, w.ID, "evaluation-judges"); err != nil {
		return out, err
	}
	latest, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return out, err
	}
	if !ciExecutorWorkflowReady(latest) || latest.Control != "ACTIVE" || !latest.Spec.Approved || latest.Generation != w.Generation || latest.HeadSHA != w.HeadSHA || latest.BaseSHA != w.BaseSHA || latest.TestedSHA != w.TestedSHA || latest.TestedTreeSHA != w.TestedTreeSHA {
		return out, store.ErrStale
	}
	runID := "ci-executor-" + domain.Digest([]any{w.ID, candidate, repo.Verification, repo.VerificationServices, e.Config().Runner.Image, time.Now().UnixNano(), e.owner})[:32]
	out.RunID = runID
	out.VerificationKey = candidate.VerificationKey
	journalPath := filepath.Join(e.Config().DataDir, "ci-executors", domain.Digest(w.ID)+".json")
	manager := e.sandboxManager()
	if manager == nil {
		return out, errors.New("可信沙箱管理器不可用")
	}
	if body, err := os.ReadFile(journalPath); err == nil {
		var previous ciExecutorJournal
		if json.Unmarshal(body, &previous) != nil || previous.WorkflowID != w.ID || !strings.HasPrefix(previous.RunID, "ci-executor-") {
			return out, errors.New("可信签发器恢复记录无效")
		}
		if err = manager.StopRun(ctx, previous.RunID); err != nil {
			return out, errors.New("旧可信签发器资源停止确认失败")
		}
		if err = e.chargeStoppedCIExecutor(ctx, &previous); err != nil {
			return out, err
		}
	} else if !os.IsNotExist(err) {
		return out, errors.New("无法读取可信签发器恢复记录")
	}
	used, err = e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil {
		return out, errors.New("无法重新核验 CI 签发时间预算")
	}
	if used >= limit {
		return out, errors.New("Issue 执行时间预算已耗尽，不能创建 CI 沙箱")
	}
	remaining := time.Duration(limit-used) * time.Second
	budgetCtx, budgetCancel := context.WithTimeout(ctx, remaining)
	defer budgetCancel()
	ctx = budgetCtx
	started := time.Now()
	charged := false
	chargeRuntime := func() error {
		if charged {
			return nil
		}
		chargeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		seconds := int64((time.Since(started) + time.Second - 1) / time.Second)
		if err := e.chargeTrustedRuntime(chargeCtx, w.ID, runID, seconds); err != nil {
			return errors.New("可信 CI 执行时间记录失败")
		}
		charged = true
		return nil
	}
	journalStarted, stopped := false, false
	defer func() {
		// 停止未确认时由恢复器按最终停止时间补记，不能提前封存幂等账目。
		if !journalStarted || stopped {
			retErr = errors.Join(retErr, chargeRuntime())
		}
	}()
	journal := ciExecutorJournal{RunID: runID, WorkflowID: w.ID, Status: "PREPARING", UpdatedAt: time.Now().UTC(), StartedAt: started}
	if err = writeCIExecutorJSON(journalPath, journal); err != nil {
		return out, err
	}
	journalStarted = true
	defer func() {
		if stopped {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := manager.StopRun(cleanup, runID); err != nil {
			journal.Status = "CLEANUP_PENDING"
			retErr = errors.Join(retErr, errors.New("可信签发器资源尚未停止确认"))
		} else {
			stopped = true
			journal.Status = "STOPPED"
			journal.StoppedAt = time.Now().UTC()
		}
		journal.UpdatedAt = time.Now().UTC()
		retErr = errors.Join(retErr, writeCIExecutorJSON(journalPath, journal))
	}()
	files, err := e.source(ctx, w, candidate.TestMergeSHA)
	if err != nil {
		return out, errors.New("无法读取固定候选快照")
	}
	tree, err := GitTreeSHA(files)
	if err != nil || tree != candidate.TreeSHA {
		return out, errors.New("实际候选快照与固定 Git 树不一致")
	}
	workspace, err := manager.CreateWithServices(ctx, runID, files, repo.VerificationServices)
	if err != nil {
		return out, errors.New("无法创建独立可信验证沙箱")
	}
	initial, err := manager.Snapshot(ctx, workspace)
	if err != nil {
		return out, errors.New("无法读取可信沙箱初始快照")
	}
	initialTree, err := GitTreeSHA(initial)
	if err != nil || initialTree != tree {
		return out, errors.New("可信沙箱初始 Git 树不匹配")
	}
	if err = manager.SealReadOnly(ctx, workspace, false); err != nil {
		return out, errors.New("可信 CI 候选只读封存失败")
	}
	timeout := time.Duration(e.Config().Runner.AttemptMinutes) * time.Minute
	executionCtx, cancel := context.WithTimeout(ctx, timeout)
	results, executionErr := runCIExecutorCommands(executionCtx, manager, workspace, repo.Verification, timeout)
	cancel()
	proof := map[string]any{"candidate": candidate, "profile_digest": domain.Digest(repo.Verification), "services": repo.VerificationServices, "runner_image": e.Config().Runner.Image, "run_id": runID, "commands": repo.Verification, "results": results, "success": executionErr == nil}
	evidence, err := e.artifact(ctx, w, "ci-executor:"+runID, "TRUSTED_CI_EXECUTION", proof)
	if err != nil {
		return out, errors.New("无法保存可信执行证据")
	}
	out.EvidenceID = evidence.ID
	if executionErr != nil {
		return out, executionErr
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	err = manager.StopRun(cleanup, runID)
	cancel()
	if err != nil {
		return out, errors.New("可信签发器资源停止确认失败")
	}
	stopped = true
	journal.Status = "STOPPED"
	journal.StoppedAt = time.Now().UTC()
	journal.UpdatedAt = time.Now().UTC()
	if err = writeCIExecutorJSON(journalPath, journal); err != nil {
		return out, err
	}
	if err = chargeRuntime(); err != nil {
		return out, err
	}
	used, err = e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil {
		return out, errors.New("无法确认签发后的剩余预算")
	}
	if used >= limit || ctx.Err() != nil {
		return out, errors.New("可信 CI 执行达到时间预算，不能签发回执")
	}
	receipt := SignedCIReceipt{Payload: CIReceiptPayload{Schema: "ai-workflow-ci-v1", Candidate: candidate, WorkflowID: policy.WorkflowID, WorkflowPath: policy.WorkflowPath, WorkflowSourceSHA: candidate.TestMergeSHA, WorkflowBlobSHA: policy.SourceBlobSHA, Conclusion: "success", CompletedAt: time.Now().UTC()}}
	body, err := CIReceiptSigningBytes(receipt.Payload)
	if err != nil {
		return out, errors.New("无法编码可信执行回执")
	}
	receipt.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))
	record, err := FetchTrustedCIRecord(ctx, client, policy, candidate, receipt, time.Now().UTC())
	if err != nil {
		return out, err
	}
	if !record.Current(policy, candidate, time.Now().UTC()) {
		return out, errors.New("可信 CI 身份对账未通过")
	}
	receiptPath := filepath.Join(e.Config().DataDir, "ci-receipts", w.ID, candidate.VerificationKey+".json")
	// 当前状态在事务锁内重读；审计与工作流证据同事务保存，文件采用同目录原子替换。
	err = e.Store.UpdateCoordination(ctx, w.ID, "trusted_ci_receipt_issued", func(current *domain.Workflow) (any, error) {
		if current.Control != "ACTIVE" || !current.Spec.Approved || current.Generation != w.Generation || current.HeadSHA != w.HeadSHA || current.BaseSHA != w.BaseSHA || current.TestedSHA != w.TestedSHA || current.TestedTreeSHA != w.TestedTreeSHA || e.key(ctx, *current) != candidate.VerificationKey {
			return nil, store.ErrStale
		}
		if err := writeCIExecutorJSON(receiptPath, receipt); err != nil {
			return nil, err
		}
		current.Evidence = mergeEvidence(current.Evidence, []domain.Evidence{evidence})
		return map[string]any{"run_id": runID, "candidate": candidate, "receipt_digest": ciDigest(receipt), "evidence_id": evidence.ID}, nil
	})
	if err != nil {
		_ = os.Remove(receiptPath)
		return out, fmt.Errorf("可信回执提交失败：%w", err)
	}
	out.ReceiptDigest = ciDigest(receipt)
	return out, nil
}

// RecoverTrustedCIExecutor 仅清理已失去进程锁的专用签发资源；即使工作流已取消也可恢复。
// 它不签名、不执行候选命令，也不修改 Agent 运行状态。
func (e *Engine) RecoverTrustedCIExecutor(ctx context.Context, workflowID string) error {
	if workflowID == "" {
		return errors.New("恢复必须指定工作流")
	}
	if _, err := e.Store.Get(ctx, workflowID); err != nil {
		return errors.New("无法读取待恢复工作流")
	}
	tx, err := e.Store.DB.Begin(ctx)
	if err != nil {
		return errors.New("无法创建签发器恢复事务")
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, trustedExecutionLockKey(workflowID)).Scan(&locked); err != nil {
		return errors.New("无法检查可信签发器恢复锁")
	}
	if !locked {
		return ErrCIExecutorBusy
	}
	journalPath := filepath.Join(e.Config().DataDir, "ci-executors", domain.Digest(workflowID)+".json")
	body, err := os.ReadFile(journalPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("无法读取签发器恢复记录")
	}
	var journal ciExecutorJournal
	if json.Unmarshal(body, &journal) != nil || journal.WorkflowID != workflowID || !strings.HasPrefix(journal.RunID, "ci-executor-") {
		return errors.New("签发器恢复记录身份错误")
	}
	manager := e.sandboxManager()
	if manager == nil {
		return errors.New("可信沙箱管理器不可用")
	}
	if err = manager.StopRun(ctx, journal.RunID); err != nil {
		return errors.New("签发器资源停止确认失败")
	}
	if err = e.chargeStoppedCIExecutor(ctx, &journal); err != nil {
		return err
	}
	journal.Status = "STOPPED"
	journal.UpdatedAt = time.Now().UTC()
	if err = writeCIExecutorJSON(journalPath, journal); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// 停止确认后按唯一执行标签补记时长；重复恢复不会重复扣减。
func (e *Engine) chargeStoppedCIExecutor(ctx context.Context, journal *ciExecutorJournal) error {
	if journal.StartedAt.IsZero() {
		return errors.New("可信签发恢复记录缺少开始时间，不能推断运行预算")
	}
	if journal.StoppedAt.IsZero() {
		journal.StoppedAt = time.Now().UTC()
	}
	return e.chargeTrustedRuntime(ctx, journal.WorkflowID, journal.RunID, int64((journal.StoppedAt.Sub(journal.StartedAt)+time.Second-1)/time.Second))
}

// CI 与外部判定共享同一 Issue 的串行执行锁，不能同时消费同一剩余时间预算。
func trustedExecutionLockKey(workflowID string) string { return "trusted-execution:" + workflowID }
func ciExecutorWorkflowReady(w domain.Workflow) bool {
	return !w.LeaseUntil.After(time.Now()) && (w.Phase == "WAITING_CI" || w.Phase == "AWAITING_MAINTAINER" || w.Phase == "EVALUATION_COMPLETE")
}

// 调用者已经持有共享执行锁；另一类执行遗留记录必须先停止并完成预算结算。
func (e *Engine) requireOtherTrustedExecutionSettled(ctx context.Context, workflowID, directory string) error {
	body, err := os.ReadFile(filepath.Join(e.Config().DataDir, directory, domain.Digest(workflowID)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("无法核验另一可信执行器的恢复记录")
	}
	var journal ciExecutorJournal
	if json.Unmarshal(body, &journal) != nil || journal.WorkflowID != workflowID || journal.RunID == "" {
		return errors.New("另一可信执行器的恢复记录身份无效")
	}
	if journal.Status != "STOPPED" || journal.StoppedAt.IsZero() {
		return errors.New("另一可信执行器尚未停止确认，请先恢复遗留运行")
	}
	var charged bool
	if err = e.Store.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM processed_commands WHERE command_id=$1 AND workflow_id=$2)`, "runtime:"+journal.RunID, workflowID).Scan(&charged); err != nil {
		return err
	}
	if !charged {
		return errors.New("另一可信执行器尚未完成预算结算，请先恢复遗留运行")
	}
	return nil
}
