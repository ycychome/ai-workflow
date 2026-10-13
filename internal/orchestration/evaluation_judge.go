package orchestration

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

// EvaluationJudgment 只由真实隐藏判定生成，不能由公开测试或模型结论推断。
type EvaluationJudgment struct {
	Status           string    `json:"status"`
	Passed           bool      `json:"passed"`
	ExitCode         int       `json:"exit_code"`
	StoppedConfirmed bool      `json:"stopped_confirmed"`
	Evidence         string    `json:"evidence"`
	TrialID          string    `json:"trial_id"`
	ConfigDigest     string    `json:"config_digest"`
	ManifestDigest   string    `json:"manifest_digest"`
	VerificationKey  string    `json:"verification_key"`
	WorkflowID       string    `json:"workflow_id"`
	Generation       int64     `json:"generation"`
	HeadSHA          string    `json:"head_sha"`
	BaseSHA          string    `json:"base_sha"`
	TestedSHA        string    `json:"tested_sha"`
	TreeSHA          string    `json:"tree_sha"`
	JudgeTreeSHA     string    `json:"judge_tree_sha"`
	RunID            string    `json:"run_id"`
	Command          []string  `json:"command"`
	CompletedAt      time.Time `json:"completed_at"`
}
type evaluationJudgeJournal struct {
	ciExecutorJournal
	StartedAt time.Time `json:"started_at"`
	StoppedAt time.Time `json:"stopped_at"`
}

type evaluationJudgeManifest struct {
	Version       string            `json:"version"`
	TrustedFiles  map[string]string `json:"trusted_files"`
	BaselineFiles map[string]string `json:"baseline_files"`
	Tasks         []struct {
		ID          string `json:"id"`
		IssuePath   string `json:"issue_path"`
		IssueSHA    string `json:"issue_sha256"`
		TestPattern string `json:"test_pattern"`
	} `json:"tasks"`
}
type evaluationJudgeBundle struct {
	Driver             []byte
	HarnessFiles       map[string]sandbox.File
	Hidden             []byte
	Pattern, TestName  string
	GoModSHA, GoSumSHA string
}

func judgeSHA(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func readJudgeFile(root, relative, expected string) ([]byte, error) {
	if !safePath(relative) || len(expected) != 64 {
		return nil, errors.New("隐藏判定材料路径或摘要不完整")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errors.New("无法读取受信判定目录")
	}
	realFile, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return nil, errors.New("可信判定材料缺失")
	}
	rel, err := filepath.Rel(realRoot, realFile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, errors.New("隐藏判定材料越出受信目录")
	}
	info, err := os.Stat(realFile)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return nil, errors.New("隐藏判定材料类型或大小无效")
	}
	body, err := os.ReadFile(realFile)
	if err != nil || judgeSHA(body) != expected {
		return nil, errors.New("隐藏判定材料与冻结摘要不符")
	}
	return body, nil
}
func loadEvaluationJudgeBundle(path string, evaluation domain.EvaluationContext) (evaluationJudgeBundle, error) {
	var bundle evaluationJudgeBundle
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return bundle, errors.New("受信评估清单缺失或无效")
	}
	body, err := os.ReadFile(path)
	if err != nil || judgeSHA(body) != evaluation.ManifestDigest {
		return bundle, errors.New("评估清单不匹配试验冻结摘要")
	}
	var manifest evaluationJudgeManifest
	if json.Unmarshal(body, &manifest) != nil || manifest.Version != evaluation.ManifestVersion {
		return bundle, errors.New("评估清单版本不匹配")
	}
	bundle.GoModSHA = manifest.BaselineFiles["go.mod"]
	bundle.GoSumSHA = manifest.BaselineFiles["go.sum"]
	if len(bundle.GoModSHA) != 64 || len(bundle.GoSumSHA) != 64 {
		return bundle, errors.New("冻结清单缺少 Go 依赖身份")
	}
	bundle.HarnessFiles = map[string]sandbox.File{}
	root := filepath.Dir(path)
	for relative, digest := range manifest.TrustedFiles {
		data, err := readJudgeFile(root, relative, digest)
		if err != nil {
			return bundle, err
		}
		switch relative {
		case "trusted/driver/main.go.txt":
			bundle.Driver = data
		case "trusted/harness/go.mod", "trusted/harness/go.sum", "trusted/harness/judge_test.go":
			bundle.HarnessFiles[strings.TrimPrefix(relative, "trusted/harness/")] = sandbox.File{Data: data, Mode: 0644}
			if relative == "trusted/harness/judge_test.go" {
				bundle.Hidden = data
			}
		}
	}
	if len(bundle.Hidden) == 0 || len(bundle.Driver) == 0 || len(bundle.HarnessFiles) != 3 {
		return bundle, errors.New("冻结清单缺少隐藏判定源码")
	}
	found := false
	for _, task := range manifest.Tasks {
		if task.ID != evaluation.TaskID {
			continue
		}
		if found {
			return bundle, errors.New("冻结清单任务编号重复")
		}
		found = true
		if _, err := readJudgeFile(root, task.IssuePath, task.IssueSHA); err != nil {
			return bundle, err
		}
		if !regexp.MustCompile(`^\^TestHiddenT[0-9]{2}[A-Za-z0-9]+\$$`).MatchString(task.TestPattern) {
			return bundle, errors.New("隐藏测试名称必须精确绑定一个冻结任务")
		}
		bundle.Pattern = task.TestPattern
		bundle.TestName = strings.TrimSuffix(strings.TrimPrefix(task.TestPattern, "^"), "$")
	}
	if !found {
		return bundle, errors.New("试验未绑定清单任务")
	}
	return bundle, nil
}

// 只允许解析独立可信 harness 进程输出；候选输出即使伪造此协议也不可传入。
func judgeTestPassed(result sandbox.RunResult, testName string) bool {
	if result.ExitCode != 0 || result.TimedOut {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(result.Stdout))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	ran, passed, packagePassed := false, false, false
	for scanner.Scan() {
		var event struct {
			Action  string
			Test    string
			Package string
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return false
		}
		if event.Package != "ai-workflow-trusted-judge" {
			continue
		}
		if event.Test == testName {
			switch event.Action {
			case "run":
				if ran {
					return false
				}
				ran = true
			case "pass":
				if passed {
					return false
				}
				passed = true
			case "fail", "skip":
				return false
			}
		}
		if event.Test == "" && event.Action == "pass" {
			packagePassed = true
		}
	}
	return scanner.Err() == nil && ran && passed && packagePassed
}

// 外部 harness 自身编译失败、找不到目标或跳过不能冒充候选 FAIL。
func judgeTestCompleted(result sandbox.RunResult, testName string) bool {
	if result.TimedOut {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(result.Stdout))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	ran, finished := false, false
	for scanner.Scan() {
		var event struct{ Action, Test, Package string }
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return false
		}
		if event.Package != "ai-workflow-trusted-judge" || event.Test != testName {
			continue
		}
		switch event.Action {
		case "run":
			if ran {
				return false
			}
			ran = true
		case "pass", "fail":
			if !ran || finished {
				return false
			}
			finished = true
		case "skip":
			return false
		}
	}
	return scanner.Err() == nil && ran && finished
}

func judgeWorkflowReady(w domain.Workflow) bool {
	return w.Evaluation != nil && w.Control == "ACTIVE" && w.Spec.Approved && !w.LeaseUntil.After(time.Now()) && (w.Phase == "EVALUATION_COMPLETE" || w.Phase == "AWAITING_MAINTAINER" || w.Phase == "BLOCKED") && ciSHA(w.HeadSHA) && ciSHA(w.BaseSHA) && ciSHA(w.TestedSHA) && ciSHA(w.TestedTreeSHA)
}
func sameJudgeCandidate(a, b domain.Workflow) bool {
	return judgeWorkflowReady(a) && a.Generation == b.Generation && a.HeadSHA == b.HeadSHA && a.BaseSHA == b.BaseSHA && a.TestedSHA == b.TestedSHA && a.TestedTreeSHA == b.TestedTreeSHA && a.Spec.Digest == b.Spec.Digest && domain.Digest(a.Evaluation) == domain.Digest(b.Evaluation)
}

// JudgeEvaluation 使用管理员挂载的清单；隐藏源码仅注入本次独立副本，绝不进入 Agent 工作区。
func (e *Engine) JudgeEvaluation(ctx context.Context, workflowID, manifestPath string) (judgment EvaluationJudgment, retErr error) {
	w, err := e.Store.Get(ctx, workflowID)
	if err != nil {
		return judgment, err
	}
	if !judgeWorkflowReady(w) {
		return judgment, errors.New("评估必须在终态且持有完整冻结候选、无活跃工作流租约后判定")
	}
	if err = e.checkEvaluationConfig(ctx, w); err != nil {
		return judgment, err
	}
	bundle, err := loadEvaluationJudgeBundle(manifestPath, *w.Evaluation)
	if err != nil {
		return judgment, err
	}
	key := e.key(ctx, w)
	judgment = EvaluationJudgment{Status: "UNVERIFIED", ExitCode: -1, TrialID: w.Evaluation.TrialID, ConfigDigest: w.Evaluation.ConfigDigest, ManifestDigest: w.Evaluation.ManifestDigest, VerificationKey: key, WorkflowID: w.ID, Generation: w.Generation, HeadSHA: w.HeadSHA, BaseSHA: w.BaseSHA, TestedSHA: w.TestedSHA, TreeSHA: w.TestedTreeSHA}
	identity := domain.Digest([]any{w.ID, w.Generation, w.HeadSHA, w.BaseSHA, w.TestedSHA, w.TestedTreeSHA, w.Evaluation, key})
	judgment.RunID = "evaluation-judge-" + domain.Digest([]any{identity, time.Now().UnixNano(), e.owner})[:32]
	outputPath := filepath.Join(e.Config().DataDir, "evaluation-judgments", domain.Digest(w.ID), identity+".json")
	conn, err := e.Store.DB.Acquire(ctx)
	if err != nil {
		return judgment, err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, trustedExecutionLockKey(w.ID)).Scan(&locked); err != nil {
		return judgment, err
	}
	if !locked {
		return judgment, errors.New("该工作流的可信隐藏判定正在执行")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, trustedExecutionLockKey(w.ID)); err != nil {
			_ = conn.Hijack().Close(cleanup)
			retErr = errors.Join(retErr, errors.New("隐藏判定互斥锁释放失败"))
		}
	}()
	if err = e.requireOtherTrustedExecutionSettled(ctx, w.ID, "ci-executors"); err != nil {
		return judgment, err
	}
	lockedCurrent, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return judgment, err
	}
	if !sameJudgeCandidate(lockedCurrent, w) {
		return judgment, store.ErrStale
	}
	if data, err := os.ReadFile(outputPath); err == nil {
		var prior EvaluationJudgment
		if json.Unmarshal(data, &prior) != nil {
			return judgment, errors.New("持久隐藏判定结果无法读取")
		}
		if !strings.HasPrefix(prior.RunID, "evaluation-judge-") || prior.TrialID != judgment.TrialID || prior.VerificationKey != judgment.VerificationKey || prior.HeadSHA != judgment.HeadSHA || prior.TreeSHA != judgment.TreeSHA || prior.ManifestDigest != judgment.ManifestDigest || prior.ConfigDigest != judgment.ConfigDigest || !prior.StoppedConfirmed || prior.Evidence == "" || (prior.Status != "PASS" && prior.Status != "FAIL") {
			return judgment, errors.New("持久隐藏判定身份不完整")
		}
		current, err := e.Store.Get(ctx, w.ID)
		if err != nil || !sameJudgeCandidate(current, w) {
			return judgment, store.ErrStale
		}
		ev, ok := integrationEvidence(current, prior.Evidence)
		if !ok {
			return judgment, errors.New("隐藏判定缺少已提交的真实证据")
		}
		proof, err := readIntegrationEvidence(e.Config().DataDir, w.ID, ev)
		if err != nil {
			return judgment, err
		}
		var recorded struct {
			Judgment EvaluationJudgment `json:"judgment"`
		}
		expected := prior
		expected.Evidence = ""
		if json.Unmarshal(proof, &recorded) != nil || domain.Digest(recorded.Judgment) != domain.Digest(expected) {
			return judgment, errors.New("持久判定与实际执行证据不匹配")
		}
		if err := e.validateJudgeRemote(ctx, current); err != nil {
			return judgment, err
		}
		return prior, nil
	} else if !os.IsNotExist(err) {
		return judgment, errors.New("无法读取既有隐藏判定")
	}
	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil {
		return judgment, err
	}
	for _, run := range runs {
		if run.Status == domain.AgentActive || run.Status == domain.AgentStopping || (!run.StartedAt.IsZero() && run.StoppedAt.IsZero()) {
			return judgment, errors.New("Agent 尚未停止确认，不能运行可信隐藏判定")
		}
	}
	used, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil {
		return judgment, err
	}
	limit := int64(e.Config().Budget.IssueMinutes) * 60
	if used >= limit {
		return judgment, errors.New("Issue 时间预算已耗尽，隐藏判定未执行")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(limit-used)*time.Second)
	defer cancel()
	started := time.Now()
	charged := false
	charge := func() error {
		if charged {
			return nil
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := e.chargeTrustedRuntime(cleanup, w.ID, judgment.RunID, int64((time.Since(started)+time.Second-1)/time.Second)); err != nil {
			return err
		}
		charged = true
		return nil
	}
	journalStarted, stopped := false, false
	defer func() {
		// 停止未确认时由恢复器按最终停止时间补记，不能提前封存幂等账目。
		if !journalStarted || stopped {
			retErr = errors.Join(retErr, charge())
		}
	}()
	manager := e.sandboxManager()
	if manager == nil {
		return judgment, errors.New("隐藏判定沙箱管理器不可用")
	}
	journalPath := filepath.Join(e.Config().DataDir, "evaluation-judges", domain.Digest(w.ID)+".json")
	if data, err := os.ReadFile(journalPath); err == nil {
		var old evaluationJudgeJournal
		if json.Unmarshal(data, &old) != nil || old.WorkflowID != w.ID || !strings.HasPrefix(old.RunID, "evaluation-judge-") {
			return judgment, errors.New("隐藏判定恢复记录无效")
		}
		if err = manager.StopRun(ctx, old.RunID); err != nil {
			return judgment, errors.New("旧隐藏判定资源停止确认失败")
		}
		if old.StartedAt.IsZero() {
			return judgment, errors.New("旧隐藏判定缺少开始时间")
		}
		if old.StoppedAt.IsZero() {
			old.StoppedAt = time.Now().UTC()
		}
		if err = e.chargeTrustedRuntime(ctx, w.ID, old.RunID, int64((old.StoppedAt.Sub(old.StartedAt)+time.Second-1)/time.Second)); err != nil {
			return judgment, err
		}
		used, err = e.Store.AgentRuntimeSeconds(ctx, w.ID)
		if err != nil {
			return judgment, err
		}
		if used >= limit {
			return judgment, errors.New("恢复旧隐藏判定后时间预算已耗尽")
		}
		// 恢复补记时长后收紧本次截止时间，避免沿用旧余额。
		var recoveredCancel context.CancelFunc
		ctx, recoveredCancel = context.WithTimeout(ctx, time.Duration(limit-used)*time.Second)
		defer recoveredCancel()

	} else if !os.IsNotExist(err) {
		return judgment, err
	}
	journal := evaluationJudgeJournal{ciExecutorJournal: ciExecutorJournal{RunID: judgment.RunID, WorkflowID: w.ID, Status: "PREPARING", UpdatedAt: time.Now().UTC()}, StartedAt: started}
	if err = writeCIExecutorJSON(journalPath, journal); err != nil {
		return judgment, err
	}
	journalStarted = true
	defer func() {
		if stopped {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := manager.StopRun(cleanup, judgment.RunID); err != nil {
			journal.Status = "CLEANUP_PENDING"
			retErr = errors.Join(retErr, errors.New("隐藏判定资源停止确认失败"))
		} else {
			stopped = true
			journal.Status = "STOPPED"
			journal.StoppedAt = time.Now().UTC()
		}
		journal.UpdatedAt = time.Now().UTC()
		retErr = errors.Join(retErr, writeCIExecutorJSON(journalPath, journal))
	}()
	if err = e.validateJudgeRemote(ctx, w); err != nil {
		return judgment, err
	}
	files, err := e.source(ctx, w, w.TestedSHA)
	if err != nil {
		return judgment, err
	}
	tree, err := GitTreeSHA(files)
	if err != nil || tree != w.TestedTreeSHA {
		return judgment, errors.New("隐藏判定候选真实 Git 树不匹配")
	}
	const destination = ".aiwf-judge-driver/main.go"
	if _, exists := files[destination]; exists {
		return judgment, errors.New("候选占用了可信判定专用路径")
	}
	// 冻结 go.mod 防止候选改变包身份或通过工具链指令替换受信 go test。
	if judgeSHA(files["go.mod"].Data) != bundle.GoModSHA || judgeSHA(files["go.sum"].Data) != bundle.GoSumSHA {
		return judgment, errors.New("候选模块身份与隐藏判定协议不一致")
	}
	files = cloneSnapshot(files)
	files[destination] = sandbox.File{Data: bundle.Driver, Mode: 0644}
	judgeTree, err := GitTreeSHA(files)
	if err != nil {
		return judgment, err
	}
	candidateDriverTree := judgeTree
	workspace, err := manager.CreateWithServices(ctx, judgment.RunID, files, []string{"postgres"})
	if err != nil {
		return judgment, err
	}
	snapshot, err := manager.Snapshot(ctx, workspace)
	if err != nil {
		return judgment, err
	}
	observed, err := GitTreeSHA(snapshot)
	if err != nil || observed != judgeTree {
		return judgment, errors.New("隐藏判定初始化快照不一致")
	}
	if err = manager.SealReadOnly(ctx, workspace, false); err != nil {
		return judgment, errors.New("隐藏判定候选及驱动只读封存失败")
	}
	peer, err := manager.CreateJudgePeer(ctx, workspace, bundle.HarnessFiles)
	if err != nil {
		return judgment, err
	}
	trustedTree, err := GitTreeSHA(bundle.HarnessFiles)
	if err != nil {
		return judgment, err
	}
	peerSnapshot, err := manager.Snapshot(ctx, peer)
	if err != nil {
		return judgment, err
	}
	peerTree, err := GitTreeSHA(peerSnapshot)
	if err != nil || peerTree != trustedTree {
		return judgment, errors.New("外部可信判定工作区身份不匹配")
	}
	judgment.JudgeTreeSHA = trustedTree
	if err = manager.SealReadOnly(ctx, peer, false); err != nil {
		return judgment, err
	}
	timeout := e.Config().AttemptTimeout()
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	testTimeout := timeout - time.Second
	if testTimeout <= 0 {
		return judgment, errors.New("剩余隐藏判定时间不足")
	}
	command := []string{"sh", "-ceu", `export GOTOOLCHAIN=local GOWORK=off GOFLAGS=; exec go test -json -count=1 -timeout "$1" -run "$2" .`, "judge", testTimeout.String(), bundle.Pattern}
	judgment.Command = command
	candidateCommand := []string{"sh", "-ceu", `export GOTOOLCHAIN=local GOWORK=off GOFLAGS=; exec go run ./.aiwf-judge-driver`}
	type candidateOutcome struct {
		Result sandbox.RunResult
		Err    error
	}
	candidateCtx, candidateCancel := context.WithCancel(ctx)
	defer candidateCancel()
	candidateDone := make(chan candidateOutcome, 1)
	go func() {
		result, err := manager.Run(candidateCtx, workspace, candidateCommand, timeout)
		candidateDone <- candidateOutcome{result, err}
	}()
	// 判定进程没有候选源码、依赖导入或候选 stdout；私有 HTTP/数据库是唯一数据通道。
	result, runErr := manager.Run(ctx, peer, command, timeout)
	candidateCancel()
	var candidateResult sandbox.RunResult
	select {
	case outcome := <-candidateDone:
		candidateResult = outcome.Result
	case <-time.After(45 * time.Second):
		return judgment, errors.New("候选服务停止等待超时")
	}
	if runErr != nil || result.TimedOut || !judgeTestCompleted(result, bundle.TestName) {
		return judgment, errors.New("隐藏判定基础设施未成功完成")
	}
	judgment.ExitCode = result.ExitCode
	judgment.Passed = judgeTestPassed(result, bundle.TestName)
	judgment.Status = "FAIL"
	if judgment.Passed {
		judgment.Status = "PASS"
	}
	cleanup, stopCancel := context.WithTimeout(context.Background(), 45*time.Second)
	err = manager.StopRun(cleanup, judgment.RunID)
	stopCancel()
	if err != nil {
		return judgment, errors.New("隐藏判定停止确认失败，不能提交结果")
	}
	stopped = true
	judgment.StoppedConfirmed = true
	journal.Status = "STOPPED"
	journal.StoppedAt = time.Now().UTC()
	journal.UpdatedAt = time.Now().UTC()
	if err = writeCIExecutorJSON(journalPath, journal); err != nil {
		return judgment, err
	}
	if err = charge(); err != nil {
		return judgment, err
	}
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil || !sameJudgeCandidate(current, w) || e.key(ctx, current) != key {
		return judgment, store.ErrStale
	}
	if err = e.checkEvaluationConfig(ctx, current); err != nil {
		return judgment, err
	}
	if err = e.validateJudgeRemote(ctx, current); err != nil {
		return judgment, err
	}
	judgment.CompletedAt = time.Now().UTC()
	ev, err := e.artifact(ctx, w, "judge:"+identity, "TRUSTED_EVALUATION_JUDGMENT", map[string]any{"judgment": judgment, "test_name": bundle.TestName, "test_source_sha256": judgeSHA(bundle.Hidden), "driver_source_sha256": judgeSHA(bundle.Driver), "candidate_driver_tree_sha": candidateDriverTree, "candidate_result": candidateResult, "result": result})
	if err != nil {
		return judgment, err
	}
	ev.TestedCommitSHA = w.TestedSHA
	ev.TestedTreeSHA = w.TestedTreeSHA
	ev.Command = command
	ev.ExitCode = result.ExitCode
	judgment.Evidence = ev.ID
	err = e.Store.UpdateCoordination(store.WithExpectedState(ctx, w.Generation, w.HeadSHA), w.ID, "evaluation_judged", func(latest *domain.Workflow) (any, error) {
		if !sameJudgeCandidate(*latest, w) {
			return nil, store.ErrStale
		}
		latest.Evidence = mergeEvidence(latest.Evidence, []domain.Evidence{ev})
		return judgment, nil
	})
	if err != nil {
		return judgment, err
	}
	if err = writeCIExecutorJSON(outputPath, judgment); err != nil {
		return judgment, err
	}
	return judgment, nil
}

// 使用现有幂等命令表将运行时间与唯一执行标签原子绑定，崩溃恢复不会漏计或重复扣减。
func (e *Engine) chargeTrustedRuntime(ctx context.Context, workflowID, runID string, seconds int64) error {
	if seconds < 0 {
		return errors.New("隐藏判定运行时长无效")
	}
	tx, err := e.Store.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, `INSERT INTO processed_commands(command_id,workflow_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, "runtime:"+runID, workflowID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	tag, err = tx.Exec(ctx, `UPDATE workflow_metrics SET controller_runtime_seconds=controller_runtime_seconds+$2,runtime_seconds=GREATEST(runtime_seconds,agent_runtime_seconds+controller_runtime_seconds)+$2 WHERE workflow_id=$1`, workflowID, seconds)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return store.ErrStale
	}
	return tx.Commit(ctx)
}
func (e *Engine) validateJudgeRemote(ctx context.Context, w domain.Workflow) error {
	client, owner, repo, err := e.client(ctx, w)
	if err != nil {
		return err
	}
	pr, err := client.GetCIPullRequestSnapshot(ctx, owner, repo, w.PR)
	if err != nil || pr.Number != w.PR || pr.State != "open" || pr.Merged || pr.Head.Repo == nil || pr.Base.Repo == nil || pr.Head.Repo.ID != w.RepositoryID || pr.Base.Repo.ID != w.RepositoryID || pr.Head.SHA != w.HeadSHA || pr.Base.SHA != w.BaseSHA || pr.MergeCommitSHA != w.TestedSHA {
		return errors.New("隐藏判定结束后远端候选身份已变化")
	}
	merge, err := client.GetCommitInfo(ctx, owner, repo, w.TestedSHA)
	if err != nil || merge.SHA != w.TestedSHA || merge.TreeSHA != w.TestedTreeSHA || len(merge.Parents) != 2 || !((merge.Parents[0] == w.BaseSHA && merge.Parents[1] == w.HeadSHA) || (merge.Parents[1] == w.BaseSHA && merge.Parents[0] == w.HeadSHA)) {
		return errors.New("隐藏判定结束后远端树及父提交无法核验")
	}
	return nil
}
