package orchestration

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"io"
	"path"
	"sort"
	"strings"
)

func safePath(p string) bool {
	return p != "" && p != "." && !strings.Contains(p, "\\") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && p != ".git" && !strings.HasPrefix(p, ".git/")
}
func matches(pattern, p string) bool {
	if pattern == "**" {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	}
	ok, _ := path.Match(pattern, p)
	return ok
}
func allowedPath(p string, r config.Repository, scope []string) bool {
	if !safePath(p) {
		return false
	}
	for _, x := range r.ProtectedPaths {
		if matches(x, p) {
			return false
		}
	}
	allowed := false
	for _, x := range r.AllowedPaths {
		if matches(x, p) {
			allowed = true
		}
	}
	if !allowed {
		return false
	}
	for _, x := range scope {
		if matches(x, p) {
			return true
		}
	}
	return false
}

// Planner-authored IDs must not collide with controller-owned tasks. Keep this
// separate from validatePlan: internal Skeptic and evidence intents legitimately
// use the reserved namespace and are not planner submissions.
func validatePlannerTaskIDs(tasks []domain.Task) error {
	for _, task := range tasks {
		if task.ID == "skeptic" || task.ID == "trusted-verification" || isEvidenceTask(task) {
			return fmt.Errorf("规划任务 %q 不能使用控制器保留的任务 ID 或前缀", task.ID)
		}
	}
	return nil
}

func validatePlan(tasks []domain.Task) error {
	if len(tasks) == 0 {
		return errors.New("计划必须至少包含一个任务")
	}
	seen := map[string]int{}
	for i, t := range tasks {
		if t.ID == "" || len(t.ID) > 80 || t.Description == "" || (taskKind(t) == "code" && len(t.Paths) == 0) {
			return fmt.Errorf("任务 %q 必须包含有效 ID 和描述；代码任务必须限定路径范围", t.ID)
		}
		if t.Kind != "" && t.Kind != "code" && t.Kind != "verification" && t.Kind != "analysis" {
			return fmt.Errorf("任务 %s 的 kind %q 不受支持", t.ID, t.Kind)
		}
		if _, ok := seen[t.ID]; ok {
			return errors.New("任务 ID 重复")
		}
		seen[t.ID] = i
		for _, p := range t.Paths {
			scope := p
			if taskKind(t) != "code" {
				scope = strings.TrimSuffix(scope, "/")
			}
			if scope != "**" && !(taskKind(t) != "code" && scope == ".") && !safePath(strings.TrimSuffix(scope, "/**")) {
				return fmt.Errorf("任务 %s 的路径范围 %q 不安全", t.ID, p)
			}
		}
	}
	visiting := map[string]bool{}
	done := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if done[id] {
			return nil
		}
		if visiting[id] {
			return errors.New("任务依赖图存在环")
		}
		idx, ok := seen[id]
		if !ok {
			return errors.New("未知任务依赖 " + id)
		}
		visiting[id] = true
		for _, d := range tasks[idx].DependsOn {
			if err := visit(d); err != nil {
				return err
			}
		}
		visiting[id] = false
		done[id] = true
		return nil
	}
	for _, t := range tasks {
		if e := visit(t.ID); e != nil {
			return e
		}
	}
	return nil
}

func taskKind(t domain.Task) string {
	if t.Kind == "" {
		return "code"
	}
	return t.Kind
}

func dependencyReports(w domain.Workflow, task domain.Task) map[string]json.RawMessage {
	reports := make(map[string]json.RawMessage, len(task.DependsOn))
	for _, dependencyID := range task.DependsOn {
		for _, dependency := range w.Tasks {
			if dependency.ID == dependencyID && len(dependency.Report) > 0 {
				reports[dependencyID] = append(json.RawMessage(nil), dependency.Report...)
				break
			}
		}
	}
	return reports
}
func scopesConflict(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == "**" || y == "**" || x == y || matches(x, strings.TrimSuffix(y, "/**")) || matches(y, strings.TrimSuffix(x, "/**")) {
				return true
			}
		}
	}
	return false
}
func readyTasks(tasks []domain.Task, limit int, review bool) []int {
	done := map[string]bool{}
	for _, t := range tasks {
		done[t.ID] = t.Status == "INTEGRATED" || t.Status == "ACCEPTED"
	}
	var ready []int
	for i, t := range tasks {
		if t.Status == "SUPERSEDED" || t.Status == "VALIDATED" || (review && (t.ID == "skeptic" || isEvidenceTask(t))) {
			continue
		}
		if done[t.ID] {
			continue
		}
		ok := true
		for _, d := range t.DependsOn {
			if !done[d] {
				ok = false
			}
		}
		if !ok {
			continue
		}
		if !review {
			for j, other := range tasks {
				if i != j && (other.Status == "VALIDATED" || other.Status == "RUNNING") && scopesConflict(t.Paths, other.Paths) {
					ok = false
				}
			}
		}
		for _, j := range ready {
			if !review && scopesConflict(t.Paths, tasks[j].Paths) {
				ok = false
			}
		}
		if ok {
			ready = append(ready, i)
		}
		if len(ready) >= limit {
			break
		}
	}
	return ready
}
func decodeArchive(b []byte) (map[string]sandbox.File, error) {
	z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		return nil, e
	}
	out := map[string]sandbox.File{}
	var total int64
	for _, f := range z.File {
		if f.FileInfo().IsDir() {
			continue
		}
		parts := strings.SplitN(f.Name, "/", 2)
		if len(parts) != 2 {
			continue
		}
		p := parts[1]
		if !safePath(p) || !f.Mode().IsRegular() {
			return nil, fmt.Errorf("归档条目 %s 不受支持", p)
		}
		mode, e := sandbox.NormalizeGitFileMode(f.Mode())
		if e != nil {
			return nil, fmt.Errorf("归档条目 %s 的文件模式不受支持: %w", p, e)
		}
		if f.UncompressedSize64 > 8<<20 {
			return nil, errors.New("源码文件超过 8 MiB")
		}
		total += int64(f.UncompressedSize64)
		if total > 64<<20 {
			return nil, errors.New("源码快照超过 64 MiB")
		}
		rd, e := f.Open()
		if e != nil {
			return nil, e
		}
		data, e := io.ReadAll(io.LimitReader(rd, 8<<20+1))
		rd.Close()
		if e != nil {
			return nil, e
		}
		if len(data) > 8<<20 {
			return nil, errors.New("源码文件过大")
		}
		out[p] = sandbox.File{Data: data, Mode: mode}
	}
	if len(out) == 0 {
		return nil, errors.New("源码快照为空")
	}
	return out, nil
}
func copyFiles(files map[string]sandbox.File) map[string]sandbox.File {
	out := map[string]sandbox.File{}
	for p, f := range files {
		out[p] = sandbox.File{Data: append([]byte(nil), f.Data...), Mode: f.Mode}
	}
	return out
}
func contentDigest(files map[string]sandbox.File) string {
	names := make([]string, 0, len(files))
	for p := range files {
		names = append(names, p)
	}
	sort.Strings(names)
	var rows []any
	for _, p := range names {
		f := files[p]
		rows = append(rows, []any{p, domain.Digest(f.Data), f.Mode})
	}
	return domain.Digest(rows)
}
func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func object(properties map[string]any) map[string]any {
	required := []string{}
	for p := range properties {
		required = append(required, p)
	}
	sort.Strings(required)
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func str() map[string]any            { return map[string]any{"type": "string"} }
func array(items any) map[string]any { return map[string]any{"type": "array", "items": items} }
func taskSchema() map[string]any {
	return taskSchemaForKinds([]string{"code", "verification", "analysis"})
}
func taskSchemaForKinds(kinds []string) map[string]any {
	return object(map[string]any{"id": str(), "kind": map[string]any{"type": "string", "enum": kinds}, "description": str(), "paths": array(str()), "depends_on": array(str()), "obligation_ids": array(str()), "finding_ids": array(str())})
}
func triageSchema() json.RawMessage {
	return raw(object(map[string]any{"decision": map[string]any{"type": "string", "enum": []string{"accepted", "needs_info", "rejected", "deferred"}}, "title": str(), "description": str(), "criteria": array(object(map[string]any{"id": str(), "description": str()})), "non_goals": array(str()), "risk": str(), "questions": array(str())}))
}
func planSchema() json.RawMessage {
	return raw(object(map[string]any{"expected_revision": map[string]any{"type": "integer", "minimum": 0}, "reason": str(), "supersede": array(str()), "tasks": array(taskSchema())}))
}
func obligationSchema() map[string]any {
	return object(map[string]any{"id": str(), "description": str(), "criterion_id": str(), "result": str(), "evidence": array(str())})
}
func findingSchema() map[string]any {
	return object(map[string]any{"id": str(), "severity": map[string]any{"type": "string", "enum": []string{"critical", "high", "medium", "low", "info"}}, "category": str(), "path": str(), "line": map[string]any{"type": "integer"}, "description": str(), "expected": str(), "actual": str(), "preconditions": str(), "evidence_type": map[string]any{"type": "string", "enum": []string{"MODEL_CLAIM", "SOURCE_ANALYSIS", "TOOL_VERIFIED"}}, "evidence": array(str()), "state": map[string]any{"type": "string", "enum": []string{"PROPOSED", "CONFIRMED", "REFUTED", "DISPUTED", "RESOLVED"}}})
}
func reviewPlanSchema() json.RawMessage {
	return raw(object(map[string]any{"tasks": array(taskSchemaForKinds([]string{"verification"})), "obligations": array(obligationSchema()), "evidence_requests": array(evidenceProposalSchema())}))
}
func reviewSchema() json.RawMessage {
	return raw(object(map[string]any{"obligations": array(obligationSchema()), "findings": array(findingSchema()), "summary": str(), "evidence_requests": array(evidenceProposalSchema())}))
}
func builderSchema() json.RawMessage {
	return raw(object(map[string]any{"summary": str(), "rebuttals": array(object(map[string]any{"finding_id": str(), "argument": str(), "evidence": array(str())}))}))
}
func nonCodeBuilderSchema() json.RawMessage {
	return raw(object(map[string]any{
		"summary":  str(),
		"result":   map[string]any{"type": "string", "enum": []string{"satisfied", "failed", "unverified"}},
		"evidence": map[string]any{"type": "array", "items": str(), "minItems": 1},
	}))
}

// Stable role identifiers; the engine renders role instructions with trusted state.
const triagePrompt = agent.RoleTriage
const devPlanPrompt = agent.RoleDevPlanner
const builderPrompt = agent.RoleBuilder
const reviewPlanPrompt = agent.RoleReviewPlanner
const reviewerPrompt = agent.RoleReviewer
const skepticPrompt = agent.RoleSkeptic
