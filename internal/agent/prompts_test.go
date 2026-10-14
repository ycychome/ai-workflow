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

func TestPromptVersionAndBudgetEfficiencyBoundaries(t *testing.T) {
	if PromptVersion != "zh-CN/v5" {
		t.Fatalf("PromptVersion=%q, want zh-CN/v5", PromptVersion)
	}
	checks := []struct {
		role RoleID
		want []string
	}{
		{RoleTriage, []string{"批量提交彼此独立", "不得在工具批次中混入 submit_report", "依赖该批次其他调用结果"}},
		{RoleDevPlanner, []string{"剩余请求预算", "合并同一修改范围的实现与测试为一个 Builder 任务", "仅为重复控制器已规定的 build/test 检查", "真实验收标准仍有未覆盖缺口", "不为保持活跃或重述验收而生成多余工作"}},
		{RoleReviewPlanner, []string{"首轮审查、可能的证据补充及 Skeptic 复核开销", "为后两者预留请求额度", "一到两个任务可覆盖彼此相关的多项义务", "不能把所有额度都排给首轮", "通过省略 Skeptic/义务制造虚假可行性"}},
		{RoleReviewer, []string{"直接相关的差异和源码", "不要重复全仓扫描", "workflow_state 等控制器事实", "产品源码行为必须由你独立取得的源码或执行证据支持", "可信 CI 结果只能证明记录的检查是否运行/通过"}},
		{RoleSkeptic, []string{"不得因预算紧张而省略已安排的 Skeptic 阶段", "减少强制义务"}},
	}
	for _, check := range checks {
		prompt, err := RenderPrompt(check.role, promptFixture())
		if err != nil {
			t.Fatal(err)
		}
		for _, phrase := range check.want {
			if !strings.Contains(prompt, phrase) {
				t.Errorf("role %s prompt missing budget/verification boundary %q", check.role, phrase)
			}
		}
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

func TestRunToolDescriptionExplainsShellAndPersistenceBoundaries(t *testing.T) {
	description, ok := ChineseToolDescription("run")
	if !ok {
		t.Fatal("run tool description is missing")
	}
	for _, required := range []string{"参数数组", "sh -ceu", "/tmp 不跨 run 保留", "同一次 run", "获授权的工作区路径"} {
		if !strings.Contains(description, required) {
			t.Errorf("run tool description must include %q: %s", required, description)
		}
	}
}
