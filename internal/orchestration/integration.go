package orchestration

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

// IntegrationInput 只接受已持久化且由有效 Agent 租约提交的来源，不接收模型拼装的发布命令。
type IntegrationInput struct {
	Task     domain.Task
	Artifact domain.SourceArtifact
	Changes  []sandbox.Change
}
type IntegrationResult struct {
	Record    domain.IntegrationRecord
	Artifacts []domain.SourceArtifact
	Changes   []sandbox.Change
}

// IntegrationConflict 给规划者明确的修复范围；冲突不能自动覆盖或被解释为成功。
type IntegrationConflict struct {
	Kind    string   `json:"kind"`
	Paths   []string `json:"paths"`
	TaskIDs []string `json:"task_ids"`
	Reason  string   `json:"reason"`
}

func (c *IntegrationConflict) Error() string {
	return fmt.Sprintf("集成冲突 %s：%s（任务 %s，路径 %s）", c.Kind, c.Reason, strings.Join(c.TaskIDs, "、"), strings.Join(c.Paths, "、"))
}

// PendingIntegration 可在创建任何资源前持久化。ID 不依赖调用顺序、时间或进程。
func PendingIntegration(w domain.Workflow, inputs []IntegrationInput) domain.IntegrationRecord {
	ids := make([]string, 0, len(inputs))
	for _, in := range inputs {
		ids = append(ids, in.Artifact.ID)
	}
	sort.Strings(ids)
	id := "integration-" + domain.Digest([]any{w.ID, w.Generation, w.Spec.Digest, w.HeadSHA, ids})[:32]
	return domain.IntegrationRecord{ID: id, ArtifactIDs: ids, ParentSHA: w.HeadSHA, Generation: w.Generation, SpecDigest: w.Spec.Digest, Status: "PREPARING", CreatedAt: time.Now().UTC()}
}
func IntegrationRunID(w domain.Workflow, inputs []IntegrationInput) string {
	return PendingIntegration(w, inputs).ID
}

// IntegrationRecordRunID 用于工作流已被接管或旧租约确定失效后的 StopRun，不能用于取消活跃控制器。
func IntegrationRecordRunID(record domain.IntegrationRecord) string { return record.ID }

// LoadIntegrationInputs 从受信记录及按哈希核验的证据恢复输入，不读取候选声称的文件路径。
func (e *Engine) LoadIntegrationInputs(ctx context.Context, w domain.Workflow, record domain.IntegrationRecord) ([]IntegrationInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if record.Generation != w.Generation || record.ParentSHA != w.HeadSHA || record.SpecDigest != w.Spec.Digest || record.Status != "PREPARING" || len(record.ArtifactIDs) == 0 {
		return nil, store.ErrStale
	}
	out := make([]IntegrationInput, 0, len(record.ArtifactIDs))
	seen := map[string]bool{}
	for _, id := range record.ArtifactIDs {
		if seen[id] {
			return nil, errors.New("集成记录包含重复产物")
		}
		seen[id] = true
		var artifact *domain.SourceArtifact
		for n := range w.SourceArtifacts {
			if w.SourceArtifacts[n].ID == id {
				if artifact != nil {
					return nil, errors.New("来源产物编号重复")
				}
				a := w.SourceArtifacts[n]
				artifact = &a
			}
		}
		if artifact == nil {
			return nil, errors.New("待集成来源产物不存在")
		}
		task, ok := integrationTask(w, artifact.TaskID)
		if !ok {
			return nil, errors.New("来源任务不存在")
		}
		ev, ok := integrationEvidence(w, artifact.EvidenceID)
		if !ok {
			return nil, errors.New("来源产物证据不存在")
		}
		body, err := readIntegrationEvidence(e.Config().DataDir, w.ID, ev)
		if err != nil {
			return nil, err
		}
		var payload sourceArtifactPayload
		if err = json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		out = append(out, IntegrationInput{task, *artifact, payload.Changes})
	}
	if PendingIntegration(w, out).ID != record.ID {
		return nil, errors.New("集成记录内容身份不匹配")
	}
	return out, nil
}

type sourceArtifactPayload struct {
	Generation int64            `json:"generation"`
	LeaseEpoch int64            `json:"lease_epoch"`
	BaseSHA    string           `json:"base_sha"`
	Changes    []sandbox.Change `json:"changes"`
}

func integrationTask(w domain.Workflow, id string) (domain.Task, bool) {
	for _, task := range w.Tasks {
		if task.ID == id {
			return task, true
		}
	}
	return domain.Task{}, false
}
func integrationEvidence(w domain.Workflow, id string) (domain.Evidence, bool) {
	var found domain.Evidence
	count := 0
	for _, ev := range w.Evidence {
		if ev.ID == id {
			found = ev
			count++
		}
	}
	return found, count == 1
}
func integrationArtifact(w domain.Workflow, id string) (domain.SourceArtifact, bool) {
	var found domain.SourceArtifact
	count := 0
	for _, a := range w.SourceArtifacts {
		if a.ID == id {
			found = a
			count++
		}
	}
	return found, count == 1
}
func (e *Engine) integrationStillOwned(ctx context.Context, w domain.Workflow) error {
	if e.Store == nil || e.Store.DB == nil || w.LeaseOwner == "" {
		return errors.New("集成必须持有当前工作流租约")
	}
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return err
	}
	if current.Generation != w.Generation || current.HeadSHA != w.HeadSHA || current.BaseSHA != w.BaseSHA || current.Spec.Digest != w.Spec.Digest || current.Control != "ACTIVE" {
		return store.ErrStale
	}
	var live bool
	err = e.Store.DB.QueryRow(ctx, `SELECT lease_owner=$2 AND lease_until>clock_timestamp() AND generation=$3 AND control='ACTIVE' FROM workflows WHERE id=$1`, w.ID, w.LeaseOwner, w.Generation).Scan(&live)
	if err != nil {
		return err
	}
	if !live {
		return store.ErrStale
	}
	return nil
}
func (e *Engine) validateIntegrationInput(ctx context.Context, w domain.Workflow, in IntegrationInput, runs map[string]domain.AgentRun) error {
	a := in.Artifact
	stored, ok := integrationArtifact(w, a.ID)
	if !ok || domain.Digest(stored) != domain.Digest(a) {
		return errors.New("来源产物未按当前内容持久化")
	}
	task, ok := integrationTask(w, a.TaskID)
	if !ok || task.ID != in.Task.ID || task.LeaseEpoch != in.Task.LeaseEpoch || task.PlanRevision != in.Task.PlanRevision || task.AgentRunID != in.Task.AgentRunID || domain.Digest(task.Paths) != domain.Digest(in.Task.Paths) {
		return errors.New("来源任务合同与当前记录不匹配")
	}
	if a.ID == "" || a.AgentRunID == "" || a.Generation != w.Generation || a.ParentSHA != w.HeadSHA || a.SpecDigest != w.Spec.Digest || a.PlanRevision < 1 || a.PlanRevision != task.PlanRevision || task.AgentRunID != a.AgentRunID || task.Status != "VALIDATED" || taskKind(task) != "code" || len(in.Changes) == 0 || a.ChangesDigest != domain.Digest(in.Changes) {
		return errors.New("来源产物代际、父提交、任务状态或变更摘要无效")
	}
	run, ok := runs[a.AgentRunID]
	if !ok || run.Status != domain.AgentFinished || run.StoppedAt.IsZero() || run.WorkflowID != w.ID || run.Generation != w.Generation || run.TaskID != task.ID || run.Role != "builder" || run.PlanRevision != a.PlanRevision || run.LeaseEpoch < 1 || run.ContextHash == "" {
		return errors.New("来源 Agent 尚未停止或运行身份无效")
	}
	var state string
	var epoch int64
	var accepted []byte
	if err := e.Store.DB.QueryRow(ctx, `SELECT state,lease_epoch,evidence FROM attempts WHERE id=$1 AND workflow_id=$2 AND intent_id=$3 AND generation=$4 AND report IS NOT NULL AND report<>'null'::jsonb`, run.ID, w.ID, task.ID, w.Generation).Scan(&state, &epoch, &accepted); err != nil {
		return fmt.Errorf("来源 Agent 缺少已接受提交回执：%w", err)
	}
	if (state != "OUTPUT_READY" && state != "ACCEPTED") || epoch != task.LeaseEpoch {
		return errors.New("来源提交不是当前任务租约下已接受的结果")
	}
	ev, ok := integrationEvidence(w, a.EvidenceID)
	if !ok || ev.Kind != "SOURCE_ARTIFACT" || ev.AgentRunID != a.AgentRunID || ev.HeadSHA != w.HeadSHA || ev.BaseSHA != w.BaseSHA || ev.Hash == "" {
		return errors.New("来源证据身份不匹配")
	}
	if a.ID != "source-"+domain.Digest([]any{a.AgentRunID, ev.Hash})[:24] {
		return errors.New("来源产物编号没有绑定原始证据摘要")
	}
	var acceptedEvidence []domain.Evidence
	if err := json.Unmarshal(accepted, &acceptedEvidence); err != nil {
		return err
	}
	matches := 0
	for _, item := range acceptedEvidence {
		if item.ID == ev.ID && domain.Digest(item) == domain.Digest(ev) {
			matches++
		}
	}
	if matches != 1 {
		return errors.New("来源证据没有被有效运行提交回执接受")
	}
	body, err := readIntegrationEvidence(e.Config().DataDir, w.ID, ev)
	if err != nil {
		return err
	}
	var payload sourceArtifactPayload
	if err = json.Unmarshal(body, &payload); err != nil {
		return err
	}
	if payload.Generation != w.Generation || payload.LeaseEpoch != task.LeaseEpoch || payload.BaseSHA != w.HeadSHA || domain.Digest(payload.Changes) != a.ChangesDigest {
		return errors.New("来源证据内容与变更合同不匹配")
	}
	for _, change := range in.Changes {
		if !allowedPath(change.Path, e.repository(w), task.Paths) {
			return &IntegrationConflict{Kind: "scope_violation", Paths: []string{change.Path}, TaskIDs: []string{task.ID}, Reason: "来源变更超出当前受信路径范围"}
		}
	}
	return nil
}

// readIntegrationEvidence 专用于含补丁的受信大证据，不降低普通 Agent 证据读取的 256 KiB 限制。
func readIntegrationEvidence(dataDir, workflowID string, ev domain.Evidence) ([]byte, error) {
	root, err := filepath.EvalSymlinks(filepath.Join(dataDir, "evidence", workflowID))
	if err != nil {
		return nil, err
	}
	target, err := filepath.EvalSymlinks(ev.Path)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, errors.New("集成证据越出当前工作流目录")
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("集成证据不是普通文件")
	}
	const limit = 128 << 20
	if info.Size() > limit {
		return nil, errors.New("集成证据超过 128 MiB 控制层上限，请拆分任务")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errors.New("集成证据读取时超过上限")
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, body); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(compact.Bytes())
	if hex.EncodeToString(sum[:]) != ev.Hash {
		return nil, errors.New("集成证据摘要不匹配")
	}
	return compact.Bytes(), nil
}

// IntegrateSourceArtifacts 在独立无凭据沙箱串行应用，计算真实 Git 树并核验导出快照。
// 调用前需持久化 PendingIntegration；调用方持有工作流租约并负责续租/失效取消。
// 返回 VALIDATED 只代表本地树已核验且资源停止确认，不代表 GitHub 已发布 CommitSHA。
func (e *Engine) IntegrateSourceArtifacts(ctx context.Context, w domain.Workflow, inputs []IntegrationInput) (result IntegrationResult, retErr error) {
	pending := PendingIntegration(w, inputs)
	result.Record = pending
	if len(inputs) == 0 {
		return result, errors.New("集成至少需要一个已接受的来源产物")
	}
	if err := e.integrationStillOwned(ctx, w); err != nil {
		return result, err
	}
	// 跨进程串行锁与工作流租约分别防止并行集成及失效控制器继续操作。
	connection, err := e.Store.DB.Acquire(ctx)
	if err != nil {
		return result, err
	}
	defer connection.Release()
	var locked bool
	if err = connection.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, "integration:"+w.ID).Scan(&locked); err != nil {
		return result, err
	}
	if !locked {
		return result, errors.New("同一工作流已有集成器运行")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := connection.Exec(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "integration:"+w.ID)
		if err != nil {
			_ = connection.Hijack().Close(cleanup)
			retErr = errors.Join(retErr, fmt.Errorf("集成串行锁释放失败：%w", err))
		}
	}()
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		return result, err
	}
	w.SourceArtifacts = current.SourceArtifacts
	w.Evidence = current.Evidence
	w.Tasks = current.Tasks
	found := false
	for _, record := range current.IntegrationRecords {
		if record.ID == pending.ID {
			if found || record.Status != "PREPARING" || record.ParentSHA != pending.ParentSHA || record.Generation != pending.Generation || record.SpecDigest != pending.SpecDigest || domain.Digest(record.ArtifactIDs) != domain.Digest(pending.ArtifactIDs) {
				return result, errors.New("持久集成记录与当前输入不匹配")
			}
			found = true
			result.Record.CreatedAt = record.CreatedAt
		}
	}
	if !found {
		return result, errors.New("创建集成资源前必须先持久化 PREPARING 记录")
	}
	runs, err := e.Store.ListAgentRuns(ctx, w.ID)
	if err != nil {
		return result, err
	}
	byRun := map[string]domain.AgentRun{}
	for _, run := range runs {
		if run.Generation == w.Generation && (run.Status == domain.AgentActive || run.Status == domain.AgentStopping) {
			return result, errors.New("集成前仍有未停止确认的 Agent 运行")
		}
		byRun[run.ID] = run
	}
	unique := map[string]bool{}
	for _, in := range inputs {
		if unique[in.Artifact.ID] {
			return result, errors.New("集成输入包含重复产物")
		}
		unique[in.Artifact.ID] = true
		if err = e.validateIntegrationInput(ctx, w, in, byRun); err != nil {
			return result, err
		}
	}
	if err = e.recoverIntegrationRuntime(ctx, w.ID, pending.ID); err != nil {
		return result, err
	}
	// 来源身份和串行准入完成后才累计实际控制器工作时间，不重复计入外层时钟。
	used, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil {
		return result, err
	}
	remaining := int64(e.Config().Budget.IssueMinutes)*60 - used
	if remaining <= 0 {
		return result, errors.New("Issue 时间预算已耗尽，不能启动集成")
	}
	budgetCtx, budgetCancel := context.WithTimeout(ctx, time.Duration(remaining)*time.Second)
	defer budgetCancel()
	ctx = budgetCtx
	started := time.Now().UTC()
	journal := integrationRuntimeJournal{ciExecutorJournal: ciExecutorJournal{RunID: pending.ID, WorkflowID: w.ID, Status: "PREPARING", StartedAt: started, UpdatedAt: started}, AccountingID: "integration-runtime-" + domain.Digest([]any{pending.ID, started.UnixNano(), e.owner})[:32]}
	journalPath := e.integrationRuntimePath(w.ID, pending.ID)
	if err = writeCIExecutorJSON(journalPath, journal); err != nil {
		return result, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := e.recoverIntegrationRuntime(cleanup, w.ID, pending.ID); err != nil {
			result.Record.Status = "CLEANUP_PENDING"
			result.Record.Reason = "集成资源停止确认或执行时间补记未完成"
			retErr = errors.Join(retErr, err)
		}
	}()
	base, err := e.source(ctx, w, w.HeadSHA)
	if err != nil {
		return result, err
	}
	baseTree, err := GitTreeSHA(base)
	if err != nil {
		return result, err
	}
	if w.Repository != "local/pilot" {
		client, owner, repo, err := e.client(ctx, w)
		if err != nil {
			return result, err
		}
		commit, err := client.GetCommitInfo(ctx, owner, repo, w.HeadSHA)
		if err != nil {
			return result, err
		}
		if commit.SHA != w.HeadSHA || commit.TreeSHA != baseTree {
			return result, errors.New("独立父提交快照与 GitHub Git 树不一致")
		}
	}
	expected, artifacts, changes, err := prepareIntegrationTree(base, inputs)
	if err != nil {
		return result, err
	}
	result.Artifacts = artifacts
	result.Changes = changes
	tree, err := GitTreeSHA(expected)
	if err != nil {
		return result, err
	}
	if err = e.integrationStillOwned(ctx, w); err != nil {
		return result, err
	}
	manager := e.sandboxManager()
	if manager == nil {
		return result, errors.New("集成沙箱管理器不可用")
	}
	workspace, err := manager.CreateWithServices(ctx, pending.ID, base, nil)
	if err != nil {
		return result, err
	}
	for _, change := range changes {
		if err = e.integrationStillOwned(ctx, w); err != nil {
			return result, err
		}
		if change.Deleted {
			run, err := manager.Run(ctx, workspace, []string{"rm", "-f", "--", change.Path}, 30*time.Second)
			if err != nil {
				return result, err
			}
			if run.ExitCode != 0 || run.TimedOut {
				return result, errors.New("集成删除文件失败")
			}
		} else {
			if err = manager.WriteFile(ctx, workspace, change.Path, sandbox.File{Data: change.Data, Mode: change.Mode}); err != nil {
				return result, err
			}
		}
	}
	collected, err := manager.CollectChanges(ctx, workspace)
	if err != nil {
		return result, err
	}
	if !sameIntegrationChanges(changes, collected) {
		return result, errors.New("沙箱导出的实际变更与已接受来源不一致")
	}
	snapshot, err := manager.Snapshot(ctx, workspace)
	if err != nil {
		return result, err
	}
	observedTree, err := GitTreeSHA(snapshot)
	if err != nil {
		return result, err
	}
	if observedTree != tree || !sameIntegrationFiles(expected, snapshot) {
		return result, errors.New("集成树与沙箱最终快照不一致")
	}
	if err = e.integrationStillOwned(ctx, w); err != nil {
		return result, err
	}
	result.Record.TreeSHA = tree
	result.Record.Status = "VALIDATED"
	result.Record.Reason = "来源已核验，独立无凭据沙箱树与变更一致；等待发布对账"
	return result, nil
}

func prepareIntegrationTree(base map[string]sandbox.File, inputs []IntegrationInput) (map[string]sandbox.File, []domain.SourceArtifact, []sandbox.Change, error) {
	ordered := append([]IntegrationInput(nil), inputs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Artifact.ID < ordered[j].Artifact.ID })
	owner := map[string]string{}
	all := []sandbox.Change{}
	artifacts := []domain.SourceArtifact{}
	for _, in := range ordered {
		individual := cloneSnapshot(base)
		seen := map[string]bool{}
		for _, change := range in.Changes {
			if seen[change.Path] {
				return nil, nil, nil, &IntegrationConflict{"duplicate_path", []string{change.Path}, []string{in.Task.ID}, "同一来源产物重复修改路径"}
			}
			seen[change.Path] = true
			if previous, ok := owner[change.Path]; ok {
				return nil, nil, nil, &IntegrationConflict{"overlapping_path", []string{change.Path}, []string{previous, in.Task.ID}, "并行产物修改同一路径，必须显式解决冲突"}
			}
			for other, task := range owner {
				if strings.HasPrefix(other, change.Path+"/") || strings.HasPrefix(change.Path, other+"/") {
					return nil, nil, nil, &IntegrationConflict{"file_directory_conflict", []string{other, change.Path}, []string{task, in.Task.ID}, "并行产物存在文件与目录冲突"}
				}
			}
			if err := applyIntegrationChange(individual, base, change); err != nil {
				return nil, nil, nil, &IntegrationConflict{"invalid_change", []string{change.Path}, []string{in.Task.ID}, err.Error()}
			}
			owner[change.Path] = in.Task.ID
			all = append(all, change)
		}
		tree, err := GitTreeSHA(individual)
		if err != nil {
			return nil, nil, nil, err
		}
		artifact := in.Artifact
		if artifact.TreeSHA != "" && artifact.TreeSHA != tree {
			return nil, nil, nil, errors.New("来源产物声称的 Git 树与实际变更不一致")
		}
		artifact.TreeSHA = tree
		artifacts = append(artifacts, artifact)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
	merged := cloneSnapshot(base)
	for _, change := range all {
		if err := applyIntegrationChange(merged, base, change); err != nil {
			return nil, nil, nil, err
		}
	}
	if _, err := GitTreeSHA(merged); err != nil {
		return nil, nil, nil, err
	}
	return merged, artifacts, all, nil
}
func applyIntegrationChange(target, base map[string]sandbox.File, c sandbox.Change) error {
	if !safePath(c.Path) || strings.ContainsRune(c.Path, 0) {
		return errors.New("来源变更路径不安全")
	}
	original, exists := base[c.Path]
	if c.Deleted {
		if c.Status != "D" || len(c.Data) != 0 || !exists {
			return errors.New("删除变更缺少对应基线文件或格式无效")
		}
		delete(target, c.Path)
		return nil
	}
	if c.Mode != fs.FileMode(0644) && c.Mode != fs.FileMode(0755) {
		return errors.New("来源变更不是允许的普通文件模式")
	}
	if (c.Status == "A" && exists) || (c.Status == "M" && !exists) || (c.Status != "A" && c.Status != "M") {
		return errors.New("来源变更类型与基线不匹配")
	}
	if exists && bytes.Equal(original.Data, c.Data) && original.Mode.Perm() == c.Mode.Perm() {
		return errors.New("来源产物包含没有实际变化的路径")
	}
	for name := range base {
		if name != c.Path && (strings.HasPrefix(name, c.Path+"/") || strings.HasPrefix(c.Path, name+"/")) {
			return errors.New("文件与目录结构替换需要独立冲突修复任务")
		}
	}
	target[c.Path] = sandbox.File{Data: append([]byte(nil), c.Data...), Mode: c.Mode}
	return nil
}
func sameIntegrationFiles(a, b map[string]sandbox.File) bool {
	if len(a) != len(b) {
		return false
	}
	for name, file := range a {
		other, ok := b[name]
		if !ok || file.Mode.Perm() != other.Mode.Perm() || !bytes.Equal(file.Data, other.Data) {
			return false
		}
	}
	return true
}
func sameIntegrationChanges(a, b []sandbox.Change) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]sandbox.Change(nil), a...)
	y := append([]sandbox.Change(nil), b...)
	sort.Slice(x, func(i, j int) bool { return x[i].Path < x[j].Path })
	sort.Slice(y, func(i, j int) bool { return y[i].Path < y[j].Path })
	for i := range x {
		if x[i].Path != y[i].Path || x[i].Status != y[i].Status || x[i].Deleted != y[i].Deleted || !bytes.Equal(x[i].Data, y[i].Data) || !x[i].Deleted && x[i].Mode.Perm() != y[i].Mode.Perm() {
			return false
		}
	}
	return true
}

type gitTreeNode struct {
	files map[string]sandbox.File
	dirs  map[string]*gitTreeNode
}

// GitTreeSHA 按 Git 对象格式计算 SHA-1 tree；不运行候选代码、Git hooks 或 shell。
// 当前快照只支持普通文件与可执行位；符号链接、子模块和非法路径必须显式拒绝。
func GitTreeSHA(files map[string]sandbox.File) (string, error) {
	root := &gitTreeNode{map[string]sandbox.File{}, map[string]*gitTreeNode{}}
	for name, file := range files {
		if !safePath(name) || strings.ContainsRune(name, 0) || file.Mode&fs.ModeType != 0 {
			return "", errors.New("Git 树包含不支持的路径或非普通文件")
		}
		if file.Mode.Perm() != 0644 && file.Mode.Perm() != 0755 {
			return "", fmt.Errorf("Git 树文件模式不支持：%s", name)
		}
		parts := strings.Split(name, "/")
		for _, part := range parts {
			if strings.EqualFold(part, ".git") {
				return "", errors.New("Git 树不能包含仓库元数据目录")
			}
		}
		node := root
		for _, part := range parts[:len(parts)-1] {
			if _, ok := node.files[part]; ok {
				return "", errors.New("Git 树存在文件与目录冲突")
			}
			if node.dirs[part] == nil {
				node.dirs[part] = &gitTreeNode{map[string]sandbox.File{}, map[string]*gitTreeNode{}}
			}
			node = node.dirs[part]
		}
		leaf := parts[len(parts)-1]
		if node.dirs[leaf] != nil {
			return "", errors.New("Git 树存在目录与文件冲突")
		}
		node.files[leaf] = file
	}
	sum := gitTreeHash(root)
	return hex.EncodeToString(sum[:]), nil
}
func gitObjectHash(kind string, body []byte) [sha1.Size]byte {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "%s %d\x00", kind, len(body))
	_, _ = h.Write(body)
	var out [sha1.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}
func gitTreeHash(node *gitTreeNode) [sha1.Size]byte {
	names := make([]string, 0, len(node.files)+len(node.dirs))
	for name := range node.files {
		names = append(names, name)
	}
	for name := range node.dirs {
		names = append(names, name+"/")
	}
	sort.Strings(names)
	var body bytes.Buffer
	for _, entry := range names {
		if strings.HasSuffix(entry, "/") {
			name := strings.TrimSuffix(entry, "/")
			sum := gitTreeHash(node.dirs[name])
			_, _ = fmt.Fprintf(&body, "40000 %s\x00", name)
			body.Write(sum[:])
		} else {
			file := node.files[entry]
			mode := "100644"
			if file.Mode.Perm()&0111 != 0 {
				mode = "100755"
			}
			sum := gitObjectHash("blob", file.Data)
			_, _ = fmt.Fprintf(&body, "%s %s\x00", mode, entry)
			body.Write(sum[:])
		}
	}
	return gitObjectHash("tree", body.Bytes())
}
