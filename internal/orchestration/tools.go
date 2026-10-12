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
	workspace        *sandbox.Workspace
	scope            []string
	review           bool
	baseFiles        map[string]sandbox.File
	mu               sync.Mutex
	evidence         []domain.Evidence
	readableEvidence []domain.Evidence
}

func (e *Engine) plannerSession(ctx context.Context, w *domain.Workflow, role string) (*toolSession, func(), error) {
	if w.BaseSHA == "" && w.Repository != "local/pilot" {
		c, o, r, err := e.client(ctx, *w)
		if err != nil {
			return nil, nil, err
		}
		sha, err := c.GetBranchSHA(ctx, o, r, w.BaseBranch)
		if err != nil {
			return nil, nil, err
		}
		w.BaseSHA = sha
	}
	sha := w.HeadSHA
	if sha == "" {
		sha = w.BaseSHA
	}
	files, err := e.source(ctx, *w, sha)
	if err != nil {
		return nil, nil, err
	}
	workspace, err := e.Sandbox.Create(ctx, fmt.Sprintf("%s-g%d-%s", w.ID, w.Generation, role), files)
	if err != nil {
		return nil, nil, err
	}
	session := &toolSession{e: e, w: *w, workspace: workspace, review: true}
	return session, func() { e.Sandbox.Destroy(context.Background(), workspace) }, nil
}
func (t *toolSession) readTools() []agent.Tool {
	var out []agent.Tool
	for _, tool := range t.tools() {
		if tool.Name == "workflow_state" || tool.Name == "read_file" || tool.Name == "list_files" || tool.Name == "search" || tool.Name == "read_base_file" || tool.Name == "candidate_diff" {
			out = append(out, tool)
		}
	}
	return out
}

func (t *toolSession) verificationTools() []agent.Tool {
	var out []agent.Tool
	for _, tool := range t.tools() {
		if tool.Name == "workflow_state" || tool.Name == "read_file" || tool.Name == "list_files" || tool.Name == "search" || tool.Name == "run" || tool.Name == "read_base_file" || tool.Name == "candidate_diff" {
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
		ev.Command = command
		ev.ExitCode = code
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
		if retErr != nil {
			t.e.Store.Audit(context.WithoutCancel(ctx), t.w.ID, "tool_error", map[string]any{"generation": t.w.Generation, "tool": name, "arguments": t.e.redact(string(args)), "error": t.e.redact(retErr.Error())})
		}
	}()
	current, err := t.e.Store.Get(ctx, t.w.ID)
	if err != nil {
		return "", err
	}
	if current.Generation != t.w.Generation || current.Control != "ACTIVE" {
		return "", errors.New("run authorization revoked")
	}
	var p struct {
		Path       string   `json:"path"`
		Content    string   `json:"content"`
		Query      string   `json:"query"`
		Args       []string `json:"args"`
		Timeout    int      `json:"timeout_seconds"`
		EvidenceID string   `json:"evidence_id"`
	}
	if err = json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	m := t.e.Sandbox
	switch name {
	case "read_evidence":
		for _, ev := range t.readableEvidence {
			if ev.ID == p.EvidenceID {
				content, err := readRecordedEvidence(t.e.Config().DataDir, t.w.ID, ev)
				if err != nil {
					return "", err
				}
				read, err := t.record(ctx, "SOURCE_ANALYSIS", map[string]any{"recorded_evidence_id": ev.ID, "recorded_hash": ev.Hash, "content": json.RawMessage(t.e.redact(string(content)))}, nil, 0)
				return string(raw(map[string]any{"recorded_evidence_id": ev.ID, "kind": ev.Kind, "exit_code": ev.ExitCode, "content": json.RawMessage(t.e.redact(string(content))), "evidence_id": read.ID})), err
			}
		}
		return "", errors.New("evidence ID is not in the assigned proof set")
	case "workflow_state":
		// This projection comes from the control store, not model-written reports.
		// The publisher has no merge operation; human_merge_required is supervisor policy.
		tasks := make([]map[string]any, 0, len(current.Tasks))
		for _, task := range current.Tasks {
			tasks = append(tasks, map[string]any{"id": task.ID, "kind": taskKind(task), "status": task.Status, "integrated_sha": task.IntegratedSHA})
		}
		facts := map[string]any{"workflow_id": current.ID, "generation": current.Generation, "phase": current.Phase, "control": current.Control, "spec_approved": current.Spec.Approved, "dev_tasks": tasks, "pull_request": current.PR, "head_sha": current.HeadSHA, "human_merge_required": true, "runner_mode": t.e.Config().Runner.Mode}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", facts, nil, 0)
		return string(raw(map[string]any{"facts": facts, "evidence_id": ev.ID})), err
	case "read_base_file":
		f, ok := t.baseFiles[p.Path]
		if !ok {
			return "", errors.New("path is absent from base snapshot")
		}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", map[string]any{"base_sha": t.w.BaseSHA, "path": p.Path, "content": t.e.redact(string(f.Data))}, nil, 0)
		return string(raw(map[string]any{"content": t.e.redact(string(f.Data)), "evidence_id": ev.ID})), err
	case "candidate_diff":
		changes, err := t.e.candidateDiff(ctx, t.w, t.baseFiles)
		if err != nil {
			return "", err
		}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", changes, nil, 0)
		return string(raw(map[string]any{"changes": changes, "evidence_id": ev.ID})), err
	case "read_file":
		b, err := m.ReadFile(ctx, t.workspace, p.Path)
		if err != nil {
			return "", err
		}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", map[string]any{"path": p.Path, "content": t.e.redact(string(b))}, nil, 0)
		return string(raw(map[string]any{"content": t.e.redact(string(b)), "evidence_id": ev.ID})), err
	case "list_files":
		entries, err := m.List(ctx, t.workspace, p.Path)
		return string(raw(entries)), err
	case "search":
		matches, err := m.Search(ctx, t.workspace, p.Query)
		if err != nil {
			return "", err
		}
		ev, err := t.record(ctx, "SOURCE_ANALYSIS", matches, nil, 0)
		return string(raw(map[string]any{"matches": matches, "evidence_id": ev.ID})), err
	case "write_file":
		if t.review {
			if !safePath(p.Path) || !strings.HasPrefix(p.Path, "review_tests/") {
				return "", errors.New("review may only write supplemental files under review_tests/")
			}
		} else if !allowedPath(p.Path, t.e.repository(t.w), t.scope) {
			return "", errors.New("path is outside assigned task or protected by policy")
		}
		if len(p.Content) > 1<<20 {
			return "", errors.New("file exceeds tool limit")
		}
		err := m.WriteFile(ctx, t.workspace, p.Path, sandbox.File{Data: []byte(p.Content), Mode: 0644})
		return "file written", err
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
		return string(raw(map[string]any{"exit_code": code, "stdout": result.Stdout, "stderr": result.Stderr, "timed_out": result.TimedOut, "evidence_id": ev.ID, "error": errString(err)})), nil
	}
	return "", errors.New("unknown tool")
}
func (t *toolSession) tools() []agent.Tool {
	props := func(p map[string]any) json.RawMessage { return raw(object(p)) }
	tools := []agent.Tool{
		{Name: "workflow_state", Description: "Read independently recorded control state, Dev artifact/task completion, and human merge policy. Provides evidence for workflow process constraints, not source correctness", Parameters: props(map[string]any{}), Handler: t.handler},
		{Name: "read_file", Description: "Read a source file and obtain immutable evidence ID", Parameters: props(map[string]any{"path": str()}), Handler: t.handler},
		{Name: "list_files", Description: "List workspace files; path can be .", Parameters: props(map[string]any{"path": str()}), Handler: t.handler},
		{Name: "search", Description: "Search source text and obtain evidence ID", Parameters: props(map[string]any{"query": str()}), Handler: t.handler},
		{Name: "write_file", Description: "Write assigned source; reviewers only supplemental tests under review_tests/", Parameters: props(map[string]any{"path": str(), "content": str()}), Handler: t.handler},
		{Name: "run", Description: "Run an argument vector in an isolated, credential-free, network-disabled container. Use sh -lc for shell commands. Obtain trusted exit/log evidence", Parameters: props(map[string]any{"args": array(str()), "timeout_seconds": map[string]any{"type": "integer"}}), Handler: t.handler},
	}
	if len(t.readableEvidence) > 0 {
		tools = append(tools, agent.Tool{Name: "read_evidence", Description: "Read the hash-verified JSON body of an assigned evidence ID from accepted peer reports. Controller file paths are inaccessible through read_file; use this tool with the unchanged evidence ID. It grants no arbitrary file access.", Parameters: props(map[string]any{"evidence_id": str()}), Handler: t.handler})
	}
	if t.baseFiles != nil {
		tools = append(tools, agent.Tool{Name: "read_base_file", Description: "Read a file at the fixed target branch base snapshot", Parameters: props(map[string]any{"path": str()}), Handler: t.handler}, agent.Tool{Name: "candidate_diff", Description: "Read independently computed base-to-candidate source changes", Parameters: props(map[string]any{}), Handler: t.handler})
	}
	return tools
}

func (e *Engine) candidateDiff(ctx context.Context, w domain.Workflow, base map[string]sandbox.File) ([]map[string]any, error) {
	head, err := e.source(ctx, w, w.HeadSHA)
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
		if aok == bok && domain.Digest(a.Data) == domain.Digest(b.Data) && a.Mode == b.Mode {
			continue
		}
		changes = append(changes, map[string]any{"path": p, "deleted": !bok, "added": !aok, "base": e.redact(string(a.Data)), "candidate": e.redact(string(b.Data))})
	}
	return changes, nil
}
