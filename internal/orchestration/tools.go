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
	e         *Engine
	w         domain.Workflow
	workspace *sandbox.Workspace
	scope     []string
	review    bool
	baseFiles map[string]sandbox.File
	mu        sync.Mutex
	evidence  []domain.Evidence
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
func (t *toolSession) handler(ctx context.Context, name string, args json.RawMessage) (string, error) {
	current, err := t.e.Store.Get(ctx, t.w.ID)
	if err != nil {
		return "", err
	}
	if current.Generation != t.w.Generation || current.Control != "ACTIVE" {
		return "", errors.New("run authorization revoked")
	}
	var p struct {
		Path    string   `json:"path"`
		Content string   `json:"content"`
		Query   string   `json:"query"`
		Args    []string `json:"args"`
		Timeout int      `json:"timeout_seconds"`
	}
	if err = json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	m := t.e.Sandbox
	switch name {
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
		{Name: "read_file", Description: "Read a source file and obtain immutable evidence ID", Parameters: props(map[string]any{"path": str()}), Handler: t.handler},
		{Name: "list_files", Description: "List workspace files; path can be .", Parameters: props(map[string]any{"path": str()}), Handler: t.handler},
		{Name: "search", Description: "Search source text and obtain evidence ID", Parameters: props(map[string]any{"query": str()}), Handler: t.handler},
		{Name: "write_file", Description: "Write assigned source; reviewers only supplemental tests under review_tests/", Parameters: props(map[string]any{"path": str(), "content": str()}), Handler: t.handler},
		{Name: "run", Description: "Run an argument vector in an isolated, credential-free, network-disabled container. Use sh -lc for shell commands. Obtain trusted exit/log evidence", Parameters: props(map[string]any{"args": array(str()), "timeout_seconds": map[string]any{"type": "integer"}}), Handler: t.handler},
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
