package server

import (
	"github.com/ycychome/ai-workflow/internal/domain"
	"testing"
)

func TestEvaluationTrialIDCannotEscapeRouteOrMarker(t *testing.T) {
	for _, id := range []string{"", "../T01", "T01/A", "T01 -->", "评估", "a?b"} {
		if evaluationID.MatchString(id) {
			t.Fatalf("不应接受试验ID %q", id)
		}
	}
	for _, id := range []string{"smoke-T01", "T01-A-1", "review-T10-defect"} {
		if !evaluationID.MatchString(id) {
			t.Fatalf("合法试验ID被拒绝 %q", id)
		}
	}
}
func TestEvaluationPRURLAbsentUntilRealPR(t *testing.T) {
	w := domain.Workflow{Repository: "ycychome/ai-workflow-pilot"}
	if evaluationPRURL(w) != "" {
		t.Fatal("没有真实PR时不能编造链接")
	}
	w.PR = 42
	if got := evaluationPRURL(w); got != "https://github.com/ycychome/ai-workflow-pilot/pull/42" {
		t.Fatal(got)
	}
}
func TestEvaluationSHAUsesRawFrozenBytes(t *testing.T) {
	if sha256Text([]byte("{}\n")) == sha256Text([]byte("{}")) {
		t.Fatal("冻结manifest必须绑定原始字节")
	}
}
