package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestEvidenceProposalValidationRequiresCurrentFindingAndAllowsUnlinkedRequest(t *testing.T) {
	w := domain.Workflow{Review: domain.Review{Key: "key-current", Findings: []domain.Finding{
		{ID: "F-current", VerificationKey: "key-current", Description: "当前候选发现"},
		{ID: "F-stale", VerificationKey: "key-old", Description: "旧候选发现"},
	}}}
	cases := []struct {
		name     string
		proposal evidenceProposal
		wantErr  string
	}{
		{name: "当前发现", proposal: evidenceProposal{Role: "reviewer", Question: "复查候选行为", FindingID: "F-current"}},
		{name: "不关联发现", proposal: evidenceProposal{Role: "verifier", Question: "运行指定检查"}},
		{name: "未知发现", proposal: evidenceProposal{Role: "reviewer", Question: "复查候选行为", FindingID: "F-unknown"}, wantErr: "未知或过期"},
		{name: "过期发现", proposal: evidenceProposal{Role: "reviewer", Question: "复查旧行为", FindingID: "F-stale"}, wantErr: "未知或过期"},
		{name: "非法角色", proposal: evidenceProposal{Role: "skeptic", Question: "复查候选行为"}, wantErr: "只能交由 verifier 或 reviewer"},
		{name: "空问题", proposal: evidenceProposal{Role: "reviewer", Question: "  "}, wantErr: "具体问题"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := domain.Digest(w)
			err := validateEvidenceProposals(w, []evidenceProposal{tc.proposal})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("有效补证请求被拒绝：%v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("错误=%v，期待包含 %q", err, tc.wantErr)
			}
			if domain.Digest(w) != before {
				t.Fatal("纯校验修改了发现账本")
			}
		})
	}
	noKey := domain.Workflow{Review: domain.Review{Findings: []domain.Finding{{ID: "F-unbound", VerificationKey: ""}}}}
	if err := validateEvidenceProposals(noKey, []evidenceProposal{{Role: "reviewer", Question: "复核", FindingID: "F-unbound"}}); err == nil {
		t.Fatal("空验证身份不能使未绑定发现可关联")
	}
}

func TestReviewPlanReportValidatesWholeEvidenceBatchBeforePlanCanBeAccepted(t *testing.T) {
	w := domain.Workflow{Review: domain.Review{Key: "key-current", Findings: []domain.Finding{{ID: "F-current", VerificationKey: "key-current"}}}}
	validate := func(output json.RawMessage) error {
		return validateReviewPlanReportForWorkflow(output, w)
	}
	invalidFirst := raw(map[string]any{
		"tasks": []domain.Task{{ID: "review-one", Kind: "verification", Description: "复核当前候选", Paths: []string{"**"}}},
		"evidence_requests": []evidenceProposal{
			{Role: "verifier", Question: "先执行有效请求", FindingID: "F-current"},
			{Role: "reviewer", Question: "再检查这个不存在的发现", FindingID: "F-missing"},
		},
	})
	if err := validate(invalidFirst); err == nil {
		t.Fatal("混有未知发现的完整批次未被拒绝")
	}
	corrected := raw(map[string]any{
		"tasks":             []domain.Task{{ID: "review-one", Kind: "verification", Description: "复核当前候选", Paths: []string{"**"}}},
		"evidence_requests": []evidenceProposal{{Role: "verifier", Question: "验证当前发现", FindingID: "F-current"}},
	})
	if err := validate(corrected); err != nil {
		t.Fatalf("修正后的完整计划未通过验证：%v", err)
	}
	if len(w.Review.Tasks) != 0 || len(w.EvidenceRequests) != 0 {
		t.Fatal("报告预检产生了计划或请求副作用")
	}
}

func TestReviewPlannerInvalidProposalGetsCorrectedBeforeAnyPlanMutation(t *testing.T) {
	w := domain.Workflow{Review: domain.Review{Key: "key-current", Findings: []domain.Finding{{ID: "F-current", VerificationKey: "key-current"}}}}
	before := domain.Digest(w)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		if calls == 1 {
			_, _ = fmt.Fprintln(response, `{"id":"resp-1","status":"completed","output":[{"type":"function_call","id":"fc-1","call_id":"submit-1","name":"submit_report","arguments":"{\"tasks\":[{\"id\":\"review-one\",\"kind\":\"verification\",\"description\":\"复核当前候选\",\"paths\":[\"**\"]}],\"evidence_requests\":[{\"role\":\"reviewer\",\"question\":\"检查不存在的发现\",\"finding_id\":\"F-missing\"}]}"}]}`)
			return
		}
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("读取修正请求失败：%v", err)
			return
		}
		encoded, _ := json.Marshal(body.Input)
		if !strings.Contains(string(encoded), "未知或过期") || !strings.Contains(string(encoded), "F-current") {
			t.Errorf("修正反馈未包含原因和有效 ID：%s", encoded)
		}
		_, _ = fmt.Fprintln(response, `{"id":"resp-2","status":"completed","output":[{"type":"function_call","id":"fc-2","call_id":"submit-2","name":"submit_report","arguments":"{\"tasks\":[{\"id\":\"review-one\",\"kind\":\"verification\",\"description\":\"复核当前候选\",\"paths\":[\"**\"]}],\"evidence_requests\":[{\"role\":\"reviewer\",\"question\":\"检查当前发现\",\"finding_id\":\"F-current\"}]}"}]}`)
	}))
	defer server.Close()
	runner, err := agent.NewRunner(agent.Config{BaseURL: server.URL, Token: "test-token", Model: "test-model", MaxOutputTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	validate := func(output json.RawMessage) error {
		return validateReviewPlanReportForWorkflow(output, w)
	}
	result, err := runner.Run(context.Background(), agent.Request{
		Input: json.RawMessage(`"plan review"`), MaxCalls: 0, MaxRequests: 2,
		ReportSchema: json.RawMessage(`{"type":"object"}`), ValidateReport: validate,
	})
	if err != nil {
		t.Fatalf("planner未能在共享请求预算内修正补证引用：%v", err)
	}
	if calls != 2 || result.Requests != 2 || strings.Contains(string(result.Report), "F-missing") || !strings.Contains(string(result.Report), "F-current") {
		t.Fatalf("Runner接受的不是修正后计划：calls=%d requests=%d report=%s", calls, result.Requests, result.Report)
	}
	if domain.Digest(w) != before || len(w.Review.Tasks) != 0 || len(w.EvidenceRequests) != 0 {
		t.Fatal("报告校验/纠正期间产生了计划或补证请求副作用")
	}
}

func TestReviewPlannerFindingAllowlistIsCurrentKeyOnlyAndDoesNotIncludePeerReports(t *testing.T) {
	w := domain.Workflow{Review: domain.Review{Key: "key-current", Findings: []domain.Finding{
		{ID: "F-current", VerificationKey: "key-current", Severity: "high", State: "CONFIRMED", Description: "当前候选发现"},
		{ID: "F-stale", VerificationKey: "key-old", Description: "不可关联的旧发现"},
	}, Tasks: []domain.Task{{ID: "peer", Report: raw(map[string]string{"summary": "peer-private-result"})}}}}
	options := evidenceFindingOptions(w)
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0].ID != "F-current" || strings.Contains(string(encoded), "peer-private-result") || strings.Contains(string(encoded), "F-stale") {
		t.Fatalf("规划器补证选项泄露了过期发现或同伴报告：%s", encoded)
	}
	input := map[string]any{"spec": w.Spec, "evidence_request_findings": options}
	if strings.Contains(string(raw(input)), "peer-private-result") {
		t.Fatal("规划器输入泄露同伴报告")
	}
}

func TestSkepticPreviewAllowsSameReportFindingRequestAndRejectsUnknownWithoutLedgerMutation(t *testing.T) {
	base := domain.Workflow{BaseSHA: "base", HeadSHA: "head", Review: domain.Review{Key: "key-current"}}
	evidence := []domain.Evidence{{ID: "ev-current", Kind: "SOURCE_ANALYSIS", BaseSHA: "base", HeadSHA: "head"}}
	finding := domain.Finding{ID: "F-new", Severity: "high", State: "CONFIRMED", EvidenceType: "SOURCE_ANALYSIS", Evidence: []string{"ev-current"}, Path: "pkg/example.go", Line: 8, Description: "新发现", Expected: "调用应拒绝无效输入", Actual: "调用继续执行", Preconditions: "传入无效输入"}
	makePreview := func(proposalID string) (domain.Workflow, error) {
		var preview domain.Workflow
		if err := json.Unmarshal(raw(base), &preview); err != nil {
			return preview, err
		}
		report := reviewReport{Summary: "独立复核", Findings: []domain.Finding{finding}, EvidenceRequests: []evidenceProposal{{Role: "verifier", Question: "补充复核新发现", FindingID: proposalID}}}
		if err := mergeReportWithMechanical(&preview, report, nil, evidence, true, nil); err != nil {
			return preview, err
		}
		if err := validateEvidenceProposals(preview, report.EvidenceRequests); err != nil {
			return preview, err
		}
		return preview, nil
	}
	valid, err := makePreview("F-new")
	if err != nil {
		t.Fatalf("Skeptic 同报告新发现的补证请求应可关联：%v", err)
	}
	if len(valid.Review.Findings) != 1 || valid.Review.Findings[0].VerificationKey != "key-current" {
		t.Fatalf("预览未将新发现绑定当前身份：%+v", valid.Review.Findings)
	}
	before := domain.Digest(base)
	if _, err = makePreview("F-not-in-report"); err == nil || !strings.Contains(err.Error(), "未知或过期") {
		t.Fatalf("Skeptic 未知发现未被预检拒绝：%v", err)
	}
	if domain.Digest(base) != before || len(base.Review.Findings) != 0 || len(base.EvidenceRequests) != 0 {
		t.Fatal("被拒绝的 Skeptic 预览修改了正式发现或请求账本")
	}
}
