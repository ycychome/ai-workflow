package orchestration

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestPlannerReportsRejectControllerTaskIDs(t *testing.T) {
	for _, id := range []string{"skeptic", "trusted-verification", "evidence-ER-", "evidence-ER-request-1"} {
		for _, planner := range []struct {
			name     string
			kind     string
			validate func(json.RawMessage) error
		}{
			{"开发完整计划", "code", validateDevPlanReport},
			{"开发动态计划", "code", validateDevelopmentDelta},
			{"审查计划", "verification", validateReviewPlanReport},
		} {
			t.Run(planner.name+"/"+id, func(t *testing.T) {
				report := raw(map[string]any{
					"expected_revision": 0, "reason": "安排完整验收任务",
					"tasks": []domain.Task{{ID: id, Kind: planner.kind, Description: "验证当前需求", Paths: []string{"calculator.go"}, ObligationIDs: []string{"AC-1"}}},
				})
				if err := planner.validate(report); err == nil || !strings.Contains(err.Error(), "控制器保留") {
					t.Fatalf("保留任务 ID 未在规划报告校验时拒绝：%v", err)
				}
			})
		}
	}
}

func TestDevelopmentPlanRejectsReservedIDsBeforeMutation(t *testing.T) {
	for _, id := range []string{"skeptic", "trusted-verification", "evidence-ER-request-1"} {
		t.Run(id, func(t *testing.T) {
			w := domain.Workflow{Generation: 1, PlanRevision: 1, TasksCreated: 1, Spec: domain.Spec{Approved: true, Digest: "approved"}, Tasks: []domain.Task{{ID: "repair-add-r1", Kind: "code", Description: "原有修复", Paths: []string{"calculator.go"}, Status: "QUEUED"}}}
			before := domain.Digest(w)
			err := applyDevelopmentPlan(&w, developmentPlanReport{ExpectedRevision: 1, Reason: "替代原有任务", Supersede: []string{"repair-add-r1"}, Tasks: []domain.Task{{ID: id, Kind: "code", Description: "新的修复", Paths: []string{"calculator.go"}}}}, 64)
			if err == nil || !strings.Contains(err.Error(), "控制器保留") {
				t.Fatalf("计划应用接受了保留任务 ID：%v", err)
			}
			if domain.Digest(w) != before {
				t.Fatal("拒绝的计划修改了任务、修订或累计额度")
			}
		})
	}
}

func TestPlannerTaskIDBoundaryPreservesOrdinaryAndInternalTasks(t *testing.T) {
	for _, task := range []domain.Task{
		{ID: "repair-add-r1", Kind: "code", Description: "修复加法", Paths: []string{"calculator.go"}},
		{ID: "review-1", Kind: "verification", Description: "独立验证加法", Paths: []string{"**"}},
		{ID: "skeptic-tests", Kind: "verification", Description: "验证边界情况", Paths: []string{"**"}},
	} {
		if err := validatePlannerTaskIDs([]domain.Task{task}); err != nil {
			t.Fatalf("普通规划任务 %s 被拒绝：%v", task.ID, err)
		}
	}
	// The common DAG validator still permits controller-created tasks. Only
	// planner report/application boundaries reject their internal namespace.
	internal := []domain.Task{
		{ID: "skeptic", Kind: "verification", Description: "控制器复核", Paths: []string{"**"}},
		{ID: "trusted-verification", Kind: "verification", Description: "受信验证", Paths: []string{"**"}},
		{ID: "evidence-ER-request-1", Kind: "verification", Description: "执行补证请求", Paths: []string{"**"}},
	}
	if err := validatePlan(internal); err != nil {
		t.Fatalf("控制器内部任务不应受规划 ID 限制：%v", err)
	}
	if err := validateDevelopmentDelta(raw(developmentPlanReport{ExpectedRevision: 1, Reason: "当前计划仍充分"})); err != nil {
		t.Fatalf("空增量规划应继续释放执行槽：%v", err)
	}
}
