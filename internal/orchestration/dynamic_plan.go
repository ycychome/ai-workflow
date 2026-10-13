package orchestration

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
)

type developmentPlanReport struct {
	ExpectedRevision int           `json:"expected_revision"`
	Reason           string        `json:"reason"`
	Tasks            []domain.Task `json:"tasks"`
	Supersede        []string      `json:"supersede"`
}

// Only durable facts wake the Planner. A no-op plan records this snapshot and
// releases its slot; the scheduler does not repeatedly call it while waiting.
func developmentStateDigest(w domain.Workflow) string {
	states := []any{}
	for _, task := range w.Tasks {
		states = append(states, []any{task.ID, task.Status, task.LeaseEpoch, task.IntegratedSHA, task.Report})
	}
	return domain.Digest([]any{w.Generation, w.Spec.Digest, w.HeadSHA, states, w.Review.Findings, w.IntegrationRecords})
}

func cloneWorkflow(w domain.Workflow) (domain.Workflow, error) {
	var snapshot domain.Workflow
	data, err := json.Marshal(w)
	if err != nil {
		return snapshot, err
	}
	err = json.Unmarshal(data, &snapshot)
	return snapshot, err
}

func applyDevelopmentPlan(w *domain.Workflow, report developmentPlanReport, maximum int) error {
	if report.ExpectedRevision != w.PlanRevision {
		return errors.New("开发计划修订已过期，请重新读取项目状态")
	}
	if !w.Spec.Approved || report.Reason == "" {
		return errors.New("计划需要已批准的需求和调整理由")
	}
	if err := validatePlannerTaskIDs(report.Tasks); err != nil {
		return err
	}
	created := max(w.TasksCreated, len(w.Tasks)+len(w.Review.Tasks))
	if created+len(report.Tasks) > maximum {
		return fmt.Errorf("累计任务数超出上限 %d，重规划不会清零", maximum)
	}
	tasks := append([]domain.Task{}, w.Tasks...)
	index := map[string]int{}
	for i, task := range tasks {
		index[task.ID] = i
	}
	for _, id := range report.Supersede {
		i, ok := index[id]
		if !ok {
			return fmt.Errorf("待替代任务 %s 不存在", id)
		}
		if tasks[i].Status == "INTEGRATED" || tasks[i].Status == "ACCEPTED" || tasks[i].Status == "RUNNING" || tasks[i].Status == "VALIDATED" {
			return fmt.Errorf("不能替代已接受或正在执行的任务 %s", id)
		}
		tasks[i].Status = "SUPERSEDED"
	}
	for _, task := range report.Tasks {
		if _, exists := index[task.ID]; exists {
			return fmt.Errorf("任务 ID %s 已存在；重规划应使用新的稳定 ID", task.ID)
		}
		for _, id := range task.FindingIDs {
			found := false
			for _, finding := range w.Review.Findings {
				if finding.ID == id {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("任务引用不存在的 Finding %s", id)
			}
		}
		if w.Phase == "CHANGES_REQUESTED" && len(w.Review.Findings) > 0 && len(task.FindingIDs) == 0 {
			return fmt.Errorf("修复任务 %s 必须关联具体 Finding", task.ID)
		}
		task.Status = "QUEUED"
		task.Attempts = 0
		task.LeaseEpoch = 0
		task.Report = nil
		task.IntegratedSHA = ""
		task.AgentRunID = ""
		task.Preflight = nil
		task.PlanRevision = w.PlanRevision + 1
		task.Role = string(taskRole(task, false))
		index[task.ID] = len(tasks)
		tasks = append(tasks, task)
	}
	if err := validatePlan(tasks); err != nil {
		return err
	}
	for _, task := range tasks {
		if task.Status != "SUPERSEDED" {
			for _, dep := range task.DependsOn {
				if tasks[index[dep]].Status == "SUPERSEDED" {
					return fmt.Errorf("有效任务 %s 依赖已替代任务 %s", task.ID, dep)
				}
			}
		}
	}
	w.PlanRevision++
	delta := domain.PlanDelta{Team: "dev", Generation: w.Generation, SpecDigest: w.Spec.Digest, ExpectedRevision: report.ExpectedRevision, Revision: w.PlanRevision, Reason: report.Reason, Add: report.Tasks, Supersede: append([]string{}, report.Supersede...), CreatedAt: time.Now().UTC()}
	delta.ID = domain.Digest(delta)
	w.PlanHistory = append(w.PlanHistory, delta)
	w.Tasks = tasks
	w.TasksCreated = created + len(report.Tasks)
	return nil
}

// First-pass review can see earlier-generation findings requiring verification,
// but not reports or findings authored by its current independent peers.
func independentPriorFindings(w domain.Workflow) []domain.Finding {
	findings := []domain.Finding{}
	for _, finding := range w.Review.Findings {
		if finding.State == "FIX_CANDIDATE" || finding.VerificationKey != w.Review.Key {
			findings = append(findings, finding)
		}
	}
	return findings
}

func validateDevelopmentDelta(output json.RawMessage) error {
	var report developmentPlanReport
	if err := json.Unmarshal(output, &report); err != nil {
		return err
	}
	if report.ExpectedRevision < 0 || report.Reason == "" {
		return errors.New("计划必须包含 expected_revision 和中文 reason")
	}
	if err := validatePlannerTaskIDs(report.Tasks); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, task := range report.Tasks {
		if task.ID == "" || seen[task.ID] {
			return errors.New("新增任务 ID 缺失或重复")
		}
		seen[task.ID] = true
	}
	return nil
}
