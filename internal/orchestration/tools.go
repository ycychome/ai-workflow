package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"strings"
	"sync"
	"time"
)

type toolSession struct {
	e                *Engine
	w                domain.Workflow
	ctx              context.Context
	execution        *agentExecution
	preflight        *domain.Preflight
	inspections      map[string]string
	workspace        *sandbox.Workspace
	manager          *sandbox.Manager
	snapshotSHA      string
	snapshotTreeSHA  string
	scope            []string
	review           bool
	baseFiles        map[string]sandbox.File
	mu               sync.Mutex
	evidence         []domain.Evidence
	readableEvidence []domain.Evidence
}

func (e *Engine) plannerSession(ctx context.Context, w *domain.Workflow, role string) (*toolSession, func() error, error) {
	roleID := agent.RoleDevPlanner
	switch role {
	case "triage":
		roleID = agent.RoleTriage
	case "review-plan":
		roleID = agent.RoleReviewPlanner
	}
	task := domain.Task{ID: role, Attempts: 1, Paths: []string{}, DependsOn: []string{}}
	execution, err := e.beginAgent(ctx, *w, task, roleID)
	if err != nil {
		return nil, nil, err
	}
	*w = execution.w
	ctx = execution.ctx
	var files map[string]sandbox.File
	if roleID == agent.RoleReviewPlanner {
		files, err = e.reviewSnapshot(ctx, *w)
	} else {
		sha := w.HeadSHA
		if sha == "" {
			sha = w.BaseSHA
		}
		files, err = e.source(ctx, *w, sha)
	}
	if err != nil {
		_ = execution.finish()
		return nil, nil, err
	}
	workspace, err := execution.manager.Create(ctx, execution.run.ID, files)
	if err != nil {
		_ = execution.finish()
		return nil, nil, err
	}
	if err = execution.manager.SealReadOnly(ctx, workspace, false); err != nil {
		_ = execution.finish()
		return nil, nil, err
	}
	session := &toolSession{e: e, w: *w, workspace: workspace, manager: execution.manager, review: true, ctx: ctx, execution: execution}
	if roleID == agent.RoleReviewPlanner {
		session.snapshotSHA = w.TestedSHA
		session.snapshotTreeSHA = w.TestedTreeSHA
		session.baseFiles, err = e.baseSnapshot(ctx, *w)
	}
	if err != nil {
		_ = execution.finish()
		return nil, nil, err
	}
	return session, func() error {
		if err := execution.finish(); err != nil {
			e.Store.Audit(context.Background(), w.ID, "agent_stop_pending", map[string]any{"agent_run_id": execution.run.ID, "error": err.Error()})
			return err
		}
		return nil
	}, nil
}
func (t *toolSession) ask(input string, tools []agent.Tool, schema json.RawMessage, validators ...func(json.RawMessage) error) (json.RawMessage, error) {
	validators = append([]func(json.RawMessage) error{t.validatePreflight}, validators...)
	return t.e.ask(t.ctx, t.w, t.execution.role, input, tools, schema, validators...)
}
func (t *toolSession) validatePreflight(_ json.RawMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.preflight == nil {
		return errors.New("尚未提交启动检查；先读取 workflow_state 和当前项目，再调用 submit_preflight")
	}
	return nil
}
func (t *toolSession) fakePreflight(ctx context.Context) error {
	ids := []string{}
	for _, name := range []string{"workflow_state", "list_files"} {
		args := raw(map[string]any{})
		if name == "list_files" {
			args = raw(map[string]string{"path": "."})
		}
		result, err := t.handler(ctx, name, args)
		if err != nil {
			return err
		}
		var value struct {
			EvidenceID string `json:"evidence_id"`
		}
		if err := json.Unmarshal([]byte(result), &value); err != nil {
			return err
		}
		ids = append(ids, value.EvidenceID)
	}
	_, err := t.handler(ctx, "submit_preflight", raw(map[string]any{"evidence_ids": ids}))
	return err
}
func (t *toolSession) readTools() []agent.Tool {
	var out []agent.Tool
	for _, tool := range t.tools() {
		if tool.Name == "submit_preflight" || tool.Name == "workflow_state" || tool.Name == "read_file" || tool.Name == "list_files" || tool.Name == "search" || tool.Name == "read_base_file" || tool.Name == "candidate_diff" {
			out = append(out, tool)
		}
	}
	return out
}

func (t *toolSession) verificationTools() []agent.Tool {
	var out []agent.Tool
	for _, tool := range t.tools() {
		if tool.Name == "submit_preflight" || tool.Name == "workflow_state" || tool.Name == "read_file" || tool.Name == "list_files" || tool.Name == "search" || tool.Name == "run" || tool.Name == "read_base_file" || tool.Name == "candidate_diff" {
			out = append(out, tool)
		}
	}
	return out
}

func (t *toolSession) record(ctx context.Context, kind string, data any, command []string, code int) (domain.Evidence, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := fmt.Sprintf("%s:%s:%d", t.workspace.ID, kind, len(t.evidence)+1)
	ev, err := t.e.artifact(ctx, t.w, id, kind, data)
	if err == nil {
		if t.execution != nil {
			ev.AgentRunID = t.execution.run.ID
		}
		ev.Command = command
		ev.ExitCode = code
		ev.TestedCommitSHA = t.snapshotSHA
		ev.TestedTreeSHA = t.snapshotTreeSHA
		t.evidence = append(t.evidence, ev)
	}
	return ev, err
}
func (t *toolSession) listEvidence() []domain.Evidence {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]domain.Evidence(nil), t.evidence...)
}
func (t *toolSession) handler(ctx context.Context, name string, args json.RawMessage) (output string, retErr error) {
	defer func() {
		if retErr == nil && (name == "workflow_state" || name == "read_file" || name == "read_base_file" || name == "list_files" || name == "search" || name == "candidate_diff") {
			var value struct {
				EvidenceID string `json:"evidence_id"`
			}
			if json.Unmarshal([]byte(output), &value) == nil && value.EvidenceID != "" {
				t.mu.Lock()
				if t.inspections == nil {
					t.inspections = map[string]string{}
				}
				t.inspections[value.EvidenceID] = name
				t.mu.Unlock()
			}
		}
		if retErr != nil {
			t.e.Store.Audit(context.WithoutCancel(ctx), t.w.ID, "tool_error", map[string]any{"generation": t.w.Generation, "tool": name, "arguments": t.e.redact(string(args)), "error": t.e.redact(retErr.Error())})
		}
	}()
	if t.execution != nil {
		if err := t.execution.check(ctx); err != nil {
			return "", err
		}
	}
	current, err := t.e.Store.Get(ctx, t.w.ID)
	if err != nil {
		return "", err
	}
	if current.Generation != t.w.Generation || current.Control != "ACTIVE" {
		return "", errors.New("本次运行授权已撤销")
	}
	var p struct {
		Path        string   `json:"path"`
		Content     string   `json:"content"`
		Query       string   `json:"query"`
		Args        []string `json:"args"`
		Timeout     int      `json:"timeout_seconds"`
		EvidenceID  string   `json:"evidence_id"`
		EvidenceIDs []string `json:"evidence_ids"`
	}
	if err = json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if (name == "write_file" || name == "run") && t.execution != nil {
		if err := t.validatePreflight(nil); err != nil {
			return "", err
		}
	}
	m := t.manager
	switch name {
	case "submit_preflight":
		if t.execution == nil {
			return "", errors.New("缺少有效的 Agent 运行身份")
		}
		t.mu.Lock()
		state, source := false, false
		for _, id := range p.EvidenceIDs {
			kind, ok := t.inspections[id]
			if !ok {
				t.mu.Unlock()
				return "", errors.New("启动检查引用了本次运行未读取的证据")
			}
			if kind == "workflow_state" {
				state = true
			} else {
				source = true
			}
		}
		if !state || !source {
			t.mu.Unlock()
			return "", errors.New("启动检查必须同时引用当前 workflow_state 与项目检查证据")
		}
		preflight := domain.Preflight{AgentRunID: t.execution.run.ID, ContextHash: t.execution.run.ContextHash, Evidence: append([]string{}, p.EvidenceIDs...), CheckedAt: time.Now().UTC()}
		t.preflight = &preflight
		t.mu.Unlock()
		t.e.Store.Audit(ctx, t.w.ID, "agent_preflight", preflight)
		return encodeToolOutput(preflight, nil)
	case "read_evidence":
		for _, ev := range t.readableEvidence {
			if ev.ID == p.EvidenceID {
				content, err := readRecordedEvidence(t.e.Config().DataDir, t.w.ID, ev)
				if err != nil {
					return "", err
				}
				// Redaction can insert quotes, and a preview can end inside a JSON token.
				// Encode the preview as a string instead of trusting it as RawMessage.
				preview := t.e.redact(string(content))
				body := map[string]any{"recorded_evidence_id": ev.ID, "recorded_hash": ev.Hash, "kind": ev.Kind, "exit_code": ev.ExitCode, "content": preview, "content_format": "json_text", "truncated": strings.HasSuffix(preview, "\n[truncated]")}
				if body["truncated"] == true {
					body["notice"] = "证据预览已截断；依赖未展示内容前，请检查候选源码或执行针对性命令。"
				}
				// Reading a proof is not a new observation. Keep its original ID and
				// trust metadata; a fresh SOURCE_ANALYSIS alias could launder a
				// failed command into success or independent counterevidence.
				body["evidence_id"] = ev.ID
				output, err := encodeToolOutput(body, nil)
				if err != nil {
					return "", err
				}
				t.e.Store.Audit(ctx, t.w.ID, "evidence_read", map[string]any{"evidence_id": ev.ID, "hash": ev.Hash, "truncated": body["truncated"]})
				return output, nil
			}
		}
		return "", errors.New("证据 ID 不在已分配的证据集合中")
	case "workflow_state":
		// This projection comes from the control store, not model-written reports.
		// The publisher has no merge operation; human_merge_required is supervisor policy.
		tasks := make([]map[string]any, 0, len(current.Tasks))
		for _, task := range current.Tasks {
			tasks = append(tasks, map[string]any{"id": task.ID, "kind": taskKind(task), "status": task.Status, "integrated_sha": task.IntegratedSHA})
		}
		runs, err := t.e.Store.ListAgentRuns(ctx, current.ID)
		if err != nil {
			return "", err
		}
		active := []map[string]any{}
		for _, run := range runs {
			if run.Generation == current.Generation && (run.Status == domain.AgentQueued || run.Status == domain.AgentActive || run.Status == domain.AgentStopping) {
				active = append(active, map[string]any{"task_id": run.TaskID, "role": run.Role, "status": run.Status, "run_id": run.ID})
			}
		}
		facts := map[string]any{"active_and_queued_agents": active, "plan_revision": current.PlanRevision, "review_plan_revision": current.Review.PlanRevision, "spec_digest": current.Spec.Digest, "verification_key": current.Review.Key, "workflow_id": current.ID, "generation": current.Generation, "phase": current.Phase, "control": current.Control, "spec_approved": current.Spec.Approved, "dev_tasks": tasks, "pull_request": current.PR, "head_sha": current.HeadSHA, "human_merge_required": true, "runner_mode": t.e.Config().Runner.Mode}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", facts, nil, 0)
		return encodeToolOutput(map[string]any{"facts": facts, "evidence_id": ev.ID}, err)
	case "read_base_file":
		f, ok := t.baseFiles[p.Path]
		if !ok {
			return "", errors.New("该路径不在固定基线快照中")
		}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", map[string]any{"base_sha": t.w.BaseSHA, "path": p.Path, "content": t.e.redact(string(f.Data))}, nil, 0)
		return encodeToolOutput(map[string]any{"content": t.e.redact(string(f.Data)), "evidence_id": ev.ID}, err)
	case "candidate_diff":
		changes, err := t.e.candidateDiff(ctx, t.w, t.baseFiles, t.snapshotSHA)
		if err != nil {
			return "", err
		}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", changes, nil, 0)
		return encodeToolOutput(map[string]any{"changes": changes, "evidence_id": ev.ID}, err)
	case "read_file":
		b, err := m.ReadFile(ctx, t.workspace, p.Path)
		if err != nil {
			return "", err
		}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", map[string]any{"path": p.Path, "content": t.e.redact(string(b))}, nil, 0)
		return encodeToolOutput(map[string]any{"content": t.e.redact(string(b)), "evidence_id": ev.ID}, err)
	case "list_files":
		entries, err := m.List(ctx, t.workspace, p.Path)
		if err != nil {
			return "", err
		}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", entries, nil, 0)
		return encodeToolOutput(map[string]any{"entries": entries, "evidence_id": ev.ID}, err)
	case "search":
		matches, err := m.Search(ctx, t.workspace, p.Query)
		if err != nil {
			return "", err
		}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", matches, nil, 0)
		return encodeToolOutput(map[string]any{"matches": matches, "evidence_id": ev.ID}, err)
	case "write_file":
		if t.review {
			if !safePath(p.Path) || !strings.HasPrefix(p.Path, "review_tests/") {
				return "", errors.New("审查只能在 review_tests/ 下写入补充文件")
			}
		} else if !allowedPath(p.Path, t.e.repository(t.w), t.scope) {
			return "", errors.New("路径超出任务范围或受策略保护")
		}
		if len(p.Content) > 1<<20 {
			return "", errors.New("文件超过工具大小限制")
		}
		err := m.WriteFile(ctx, t.workspace, p.Path, sandbox.File{Data: []byte(p.Content), Mode: 0644})
		return "文件已写入", err
	case "run":
		timeout := time.Duration(p.Timeout) * time.Second
		if timeout <= 0 || timeout > 5*time.Minute {
			timeout = 5 * time.Minute
		}
		result, err := m.Run(ctx, t.workspace, p.Args, timeout)
		code := result.ExitCode
		if err != nil {
			code = -1
		}
		result.Stdout = t.e.redact(result.Stdout)
		result.Stderr = t.e.redact(result.Stderr)
		ev, recordErr := t.record(context.WithoutCancel(ctx), "TOOL_VERIFIED", result, p.Args, code)
		if recordErr != nil {
			return "", recordErr
		}
		return encodeToolOutput(map[string]any{"exit_code": code, "stdout": result.Stdout, "stderr": result.Stderr, "timed_out": result.TimedOut, "evidence_id": ev.ID, "error": errString(err)}, nil)
	}
	return "", errors.New("未知工具")
}
func (t *toolSession) tools() []agent.Tool {
	props := func(p map[string]any) json.RawMessage { return raw(object(p)) }
	tools := []agent.Tool{
		{Name: "submit_preflight", Parameters: props(map[string]any{"evidence_ids": array(str())}), Handler: t.handler},
		{Name: "workflow_state", Description: "读取控制器独立记录的控制状态、开发产物与任务完成状态、人工合并策略。这些证据用于验证工作流过程约束，不能证明源码正确。", Parameters: props(map[string]any{}), Handler: t.handler},
		{Name: "read_file", Description: "读取源码文件并取得不可变证据 ID", Parameters: props(map[string]any{"path": str()}), Handler: t.handler},
		{Name: "list_files", Description: "列出工作区文件；path 可设为 .", Parameters: props(map[string]any{"path": str()}), Handler: t.handler},
		{Name: "search", Description: "搜索源码文本并取得证据 ID", Parameters: props(map[string]any{"query": str()}), Handler: t.handler},
		{Name: "write_file", Description: "写入分配范围内的源码；审查者只能在 review_tests/ 下添加补充测试", Parameters: props(map[string]any{"path": str(), "content": str()}), Handler: t.handler},
		{Name: "run", Description: "在隔离、无凭据、无网络的容器中执行。优先直接传参数数组，不经 shell；确需 shell 时使用 sh -ceu '命令'，不要使用 sh -lc（login shell 会重置 PATH）。每次 run 都启动新的临时容器，/tmp 不跨 run 保留；临时脚本须在同一次 run 内创建并执行。只有获授权的工作区路径在不同 run 间持久。返回受信退出状态与日志证据。", Parameters: props(map[string]any{"args": array(str()), "timeout_seconds": map[string]any{"type": "integer"}}), Handler: t.handler},
	}
	if len(t.readableEvidence) > 0 {
		tools = append(tools, agent.Tool{Name: "read_evidence", Description: "读取已接受同伴报告中分配给你的证据 ID，返回通过哈希校验并脱敏的 JSON 文本预览。预览截断会明确标记；内容不全时请针对性读取源码或执行命令。read_file 不能访问控制器文件路径。引用返回的原始 evidence_id；读取行为不会产生新的独立证明，也不授予任意文件访问权。", Parameters: props(map[string]any{"evidence_id": str()}), Handler: t.handler})
	}
	if t.baseFiles != nil {
		tools = append(tools, agent.Tool{Name: "read_base_file", Description: "读取固定目标分支基线快照中的文件", Parameters: props(map[string]any{"path": str()}), Handler: t.handler}, agent.Tool{Name: "candidate_diff", Description: "读取控制器独立计算的基线到候选源码差异", Parameters: props(map[string]any{}), Handler: t.handler})
	}
	for i := range tools {
		if description, ok := agent.ChineseToolDescription(tools[i].Name); ok {
			tools[i].Description = description
		}
	}
	return tools
}

func (e *Engine) candidateDiff(ctx context.Context, w domain.Workflow, base map[string]sandbox.File, snapshotSHA string) ([]map[string]any, error) {
	var head map[string]sandbox.File
	var err error
	if snapshotSHA != "" {
		if snapshotSHA != w.TestedSHA {
			return nil, errors.New("审查源码身份与已验证快照不一致")
		}
		head, err = e.reviewSnapshot(ctx, w)
	} else {
		// Read-only development tasks also have this tool, before a test merge
		// exists. Only review sessions carry an explicit verified snapshot SHA.
		sha := w.HeadSHA
		if sha == "" {
			sha = w.BaseSHA
		}
		head, err = e.source(ctx, w, sha)
	}
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for p := range base {
		names[p] = true
	}
	for p := range head {
		names[p] = true
	}
	var changes []map[string]any
	for p := range names {
		a, aok := base[p]
		b, bok := head[p]
		modeUnchanged := !aok
		if aok && bok {
			aMode, aErr := sandbox.NormalizeGitFileMode(a.Mode)
			bMode, bErr := sandbox.NormalizeGitFileMode(b.Mode)
			modeUnchanged = aErr == nil && bErr == nil && aMode == bMode
		}
		if aok == bok && domain.Digest(a.Data) == domain.Digest(b.Data) && modeUnchanged {
			continue
		}
		changes = append(changes, map[string]any{"path": p, "deleted": !bok, "added": !aok, "base": e.redact(string(a.Data)), "candidate": e.redact(string(b.Data))})
	}
	return changes, nil
}

func encodeToolOutput(value any, priorErr error) (string, error) {
	if priorErr != nil {
		return "", priorErr
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("编码工具输出失败：%w", err)
	}
	return string(b), nil
}
