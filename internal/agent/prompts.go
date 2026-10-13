package agent

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"
	"unicode"
)

// PromptContext contains controller-authored state only. Source text, reports,
// task summaries and other untrusted bodies belong in Request.Input, never here.
// Empty slices mean explicitly no capabilities/dependencies/other tasks; nil
// slices are rejected to prevent an omitted boundary becoming an implicit grant.
type PromptContext struct {
	Repository        string            `json:"repository"`
	RepositoryID      int64             `json:"repository_id"`
	Issue             int               `json:"issue"`
	PR                int               `json:"pr"`
	AgentRunID        string            `json:"agent_run_id"`
	LeaseEpoch        int64             `json:"lease_epoch"`
	StartedAt         *time.Time        `json:"started_at"`
	Deadline          *time.Time        `json:"deadline"`
	Workspace         string            `json:"workspace"`
	SpecDigest        string            `json:"spec_digest"`
	BaseSHA           string            `json:"base_sha"`
	HeadSHA           string            `json:"head_sha"`
	TestedCommitSHA   string            `json:"tested_commit_sha"`
	TestedTreeSHA     string            `json:"tested_tree_sha"`
	EvaluationProfile string            `json:"evaluation_profile,omitempty"`
	VerificationKey   string            `json:"verification_key"`
	AllowedPaths      []string          `json:"allowed_paths"`
	ProtectedPaths    []string          `json:"protected_paths"`
	Checkpoint        string            `json:"checkpoint"`
	Concurrency       PromptConcurrency `json:"concurrency"`

	WorkflowID   string             `json:"workflow_id"`
	TaskID       string             `json:"task_id"`
	Attempt      int                `json:"attempt"`
	Generation   int                `json:"generation"`
	PlanRevision int                `json:"plan_revision"`
	QueueReason  string             `json:"queue_reason"`
	QueuedAt     *time.Time         `json:"queued_at"`
	StartReason  string             `json:"start_reason"`
	Versions     PromptVersions     `json:"versions"`
	Budget       PromptBudget       `json:"budget"`
	Capabilities []string           `json:"capabilities"`
	Dependencies []string           `json:"dependencies"`
	OtherTasks   []TaskStateSummary `json:"other_tasks"`
}
type PromptConcurrency struct {
	Global   int `json:"global"`
	Workflow int `json:"workflow"`
}
type PromptVersions struct {
	Prompt   string `json:"prompt"`
	Policy   string `json:"policy"`
	Snapshot string `json:"snapshot"`
}

// MaxCalls and MaxRequests are the remaining limits for this run, not workflow totals.
type PromptBudget struct {
	MaxCalls                  int    `json:"max_calls"`
	MaxRequests               int    `json:"max_requests"`
	RemainingTokens           int64  `json:"remaining_tokens"`
	TokenBudgetKind           string `json:"token_budget_kind,omitempty"`
	DailyRemainingRequests    int    `json:"daily_remaining_requests"`
	IssueRemainingSeconds     int64  `json:"issue_remaining_seconds"`
	RemainingTasks            int    `json:"remaining_tasks"`
	RemainingEvidenceRequests int    `json:"remaining_evidence_requests"`
}

// TaskStateSummary deliberately excludes agent-authored prose and reports.
type TaskStateSummary struct {
	TaskID string `json:"task_id"`
	Role   RoleID `json:"role"`
	Status string `json:"status"`
}

//go:embed templates/system.tmpl
var systemTemplate string

func (c PromptContext) Validate() error {
	for _, field := range []struct{ name, value string }{{"workflow_id", c.WorkflowID}, {"task_id", c.TaskID}, {"start_reason", c.StartReason}, {"versions.prompt", c.Versions.Prompt}, {"versions.policy", c.Versions.Policy}, {"versions.snapshot", c.Versions.Snapshot}} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("受信运行状态缺少 %s", field.name)
		}
	}
	if c.Versions.Prompt != PromptVersion {
		return fmt.Errorf("提示词版本不匹配：%s", c.Versions.Prompt)
	}
	if c.Attempt < 1 || c.Generation < 1 || c.PlanRevision < 1 {
		return fmt.Errorf("attempt、generation 和 plan_revision 必须为正数")
	}
	if c.Budget.MaxCalls < 0 || c.Budget.MaxRequests < 1 || c.Budget.RemainingTokens < 0 {
		return fmt.Errorf("运行预算无效")
	}
	if (strings.TrimSpace(c.QueueReason) == "") != (c.QueuedAt == nil) {
		return fmt.Errorf("queue_reason 与 queued_at 必须同时提供或同时为空")
	}
	if c.QueuedAt != nil && c.QueuedAt.IsZero() {
		return fmt.Errorf("queued_at 不能为零时间")
	}
	if c.Capabilities == nil || c.Dependencies == nil || c.OtherTasks == nil {
		return fmt.Errorf("capabilities、dependencies 和 other_tasks 必须显式提供，允许空数组")
	}
	for _, value := range append(append([]string{}, c.Capabilities...), c.Dependencies...) {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("能力和依赖 ID 不能为空")
		}
	}
	for _, task := range c.OtherTasks {
		if strings.TrimSpace(task.TaskID) == "" || strings.TrimSpace(task.Status) == "" {
			return fmt.Errorf("其他任务状态缺少 ID 或状态")
		}
		if _, err := LookupRole(task.Role); err != nil {
			return err
		}
	}
	return nil
}

func RenderPrompt(id RoleID, ctx PromptContext) (string, error) {
	role, err := LookupRole(id)
	if err != nil {
		return "", err
	}
	if err = ctx.Validate(); err != nil {
		return "", err
	}
	state, err := json.MarshalIndent(ctx, "", "  ")
	if err != nil {
		return "", err
	}
	tmpl, err := template.New("system").Option("missingkey=error").Parse(systemTemplate)
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	err = tmpl.Execute(&output, struct {
		Rules   string
		Catalog []RoleDefinition
		Role    RoleDefinition
		State   string
	}{CommonRules, RoleCatalog(), role, string(state)})
	return output.String(), err
}

// ChineseToolDescription is a closed catalog; unknown tools are not silently
// described as having privileges they do not possess.
func ChineseToolDescription(name string) (string, bool) {
	descriptions := map[string]string{
		"submit_preflight": "提交当前启动检查，必须引用本次读取的 workflow_state 和项目证据；排队、重试后均须重新检查，接受前不能写入、运行命令或完成报告。",
		"workflow_state":   "读取独立记录的控制状态、开发产物与任务完成状态、人工合并策略。此证据用于流程约束，不证明源码正确。",
		"read_file":        "读取源码文件并获得不可变证据 ID。",
		"list_files":       "列出工作区文件，path 可为 .。",
		"search":           "搜索源码文本并获得证据 ID。",
		"write_file":       "写入分配范围内的源码；审查者仅可在 review_tests/ 写补充测试。",
		"run":              "在隔离、无凭据且禁用网络的容器内执行。优先直接传参数数组，不经 shell；确需 shell 时使用 sh -ceu '命令'，不要使用 login shell。每次 run 都启动新的临时容器，/tmp 不跨 run 保留；临时脚本应在同一次 run 内创建并执行。只有获授权的工作区路径在不同 run 间持久。返回受信退出状态及日志证据。",
		"read_evidence":    "按 ID 读取已接受同伴报告中分配且哈希验证的证据，返回脱敏 JSON 预览并标记截断。正文缺失时执行针对性读取或检查；控制器路径不能通过 read_file 访问。引用原始 evidence_id，读取不构成新的独立证明，也不授予任意文件访问权。",
		"read_base_file":   "读取固定目标分支基线快照中的文件。",
		"candidate_diff":   "读取独立计算的基线到候选版本的源码差异。",
		"submit_report":    "提交本角色的最终结构化报告。此工具必须独占当前响应；校验失败后可在后续响应修正并重新提交。",
	}
	description, ok := descriptions[name]
	return description, ok
}

// ValidateChineseProse checks only explicitly selected prose paths. Paths use
// dot-separated JSON keys and * for array elements, e.g. findings.*.description.
// It never rewrites JSON, scans evidence fields implicitly or bans ASCII: code,
// URLs, commands, proper nouns and quoted evidence remain byte-for-byte intact.
// Han presence is a lightweight language guard, not a claim of perfect language
// identification or a traditional-to-simplified conversion.
func ValidateChineseProse(report json.RawMessage, paths ...string) error {
	var root any
	if err := json.Unmarshal(report, &root); err != nil {
		return err
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("自然语言字段路径不能为空")
		}
		if err := validateProsePath(root, strings.Split(path, "."), path); err != nil {
			return err
		}
	}
	return nil
}
func validateProsePath(value any, parts []string, path string) error {
	if len(parts) == 0 {
		prose, ok := value.(string)
		if !ok {
			return fmt.Errorf("自然语言字段 %s 必须是字符串", path)
		}
		if strings.TrimSpace(prose) == "" {
			return nil
		}
		for _, r := range prose {
			if unicode.Is(unicode.Han, r) {
				return nil
			}
		}
		return fmt.Errorf("自然语言字段 %s 应使用简体中文；保留代码、命令和原始引文", path)
	}
	if parts[0] == "*" {
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("自然语言字段路径 %s 预期数组", path)
		}
		for _, item := range items {
			if err := validateProsePath(item, parts[1:], path); err != nil {
				return err
			}
		}
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("自然语言字段路径 %s 预期对象", path)
	}
	next, ok := object[parts[0]]
	if !ok {
		return fmt.Errorf("自然语言字段 %s 缺失", path)
	}
	return validateProsePath(next, parts[1:], path)
}
