package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func promptFixture() PromptContext {
	return PromptContext{WorkflowID: "workflow-1", TaskID: "task-1", Attempt: 1, Generation: 1, PlanRevision: 1, StartReason: "首次启动", Versions: PromptVersions{Prompt: PromptVersion, Policy: "policy-1", Snapshot: "尚未建立"}, Budget: PromptBudget{MaxCalls: 4, MaxRequests: 8, RemainingTokens: 1000}, Capabilities: []string{"read_file"}, Dependencies: []string{}, OtherTasks: []TaskStateSummary{}}
}
func TestRoleCatalogAndRenderedBoundaries(t *testing.T) {
	for _, role := range RoleCatalog() {
		prompt, err := RenderPrompt(role.ID, promptFixture())
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{string(role.ID), role.Instructions, "不可信参考资料边界", "受信运行状态", "简体中文", "校验拒绝", "重新检查"} {
			if !strings.Contains(prompt, required) {
				t.Errorf("role %s missing %q", role.ID, required)
			}
		}
		if role.ID == RoleReviewer && role.ModelClass != ModelClassReview {
			t.Fatal("review role routed to development model")
		}
	}
	roles := RoleCatalog()
	roles[0].ID = "corrupted"
	if _, err := LookupRole(RoleTriage); err != nil {
		t.Fatal("catalog was mutable")
	}
	if _, err := RenderPrompt("unknown", promptFixture()); err == nil {
		t.Fatal("unknown role accepted")
	}
}
func TestPromptContextRejectsMissingState(t *testing.T) {
	changes := []func(*PromptContext){func(c *PromptContext) { c.WorkflowID = "" }, func(c *PromptContext) { c.TaskID = "" }, func(c *PromptContext) { c.Attempt = 0 }, func(c *PromptContext) { c.Generation = 0 }, func(c *PromptContext) { c.PlanRevision = 0 }, func(c *PromptContext) { c.Versions.Prompt = "stale" }, func(c *PromptContext) { c.Versions.Snapshot = "" }, func(c *PromptContext) { c.Capabilities = nil }, func(c *PromptContext) { c.QueueReason = "等待资源" }, func(c *PromptContext) { c.Budget.MaxRequests = 0 }}
	for i, change := range changes {
		c := promptFixture()
		change(&c)
		if _, err := RenderPrompt(RoleBuilder, c); err == nil {
			t.Errorf("case %d accepted omitted state", i)
		}
	}
	c := promptFixture()
	now := time.Now()
	c.QueueReason = "等待资源"
	c.QueuedAt = &now
	if _, err := RenderPrompt(RoleBuilder, c); err != nil {
		t.Fatal(err)
	}
}
func TestChineseProseValidationPreservesEvidence(t *testing.T) {
	report := json.RawMessage(`{"summary":"检查 go test ./... 失败，详见证据。","evidence":["FAIL TestExample"],"findings":[{"description":"接口返回错误。","path":"a.go"}]}`)
	before := string(report)
	if err := ValidateChineseProse(report, "summary", "findings.*.description"); err != nil {
		t.Fatal(err)
	}
	if string(report) != before {
		t.Fatal("report or evidence modified")
	}
	if err := ValidateChineseProse(json.RawMessage(`{"summary":"All checks passed"}`), "summary"); err == nil {
		t.Fatal("English-only prose accepted")
	}
	if err := ValidateChineseProse(report, "missing"); err == nil {
		t.Fatal("missing prose field accepted")
	}
}
func TestChineseToolCatalog(t *testing.T) {
	for _, name := range []string{"workflow_state", "read_file", "list_files", "search", "write_file", "run", "read_evidence", "read_base_file", "candidate_diff", "submit_report"} {
		if description, ok := ChineseToolDescription(name); !ok || description == "" {
			t.Errorf("missing tool %s", name)
		}
	}
	if _, ok := ChineseToolDescription("spawn_anything"); ok {
		t.Fatal("unknown capability described")
	}
}
