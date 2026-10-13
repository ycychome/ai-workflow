package orchestration

import (
	"encoding/json"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJudgeRequiresActualNamedTestAndPackagePass(t *testing.T) {
	const pkg = "ai-workflow-trusted-judge"
	event := func(action, test string) string {
		b, _ := json.Marshal(map[string]string{"Action": action, "Test": test, "Package": pkg})
		return string(b) + "\n"
	}
	success := event("run", "TestHiddenT01Pagination") + event("pass", "TestHiddenT01Pagination") + event("pass", "")
	if !judgeTestPassed(sandbox.RunResult{Stdout: success}, "TestHiddenT01Pagination") {
		t.Fatal("真实目标测试成功未被识别")
	}
	for _, result := range []sandbox.RunResult{
		{}, {Stdout: event("pass", "")}, {Stdout: event("run", "TestHiddenT01Pagination") + event("skip", "TestHiddenT01Pagination") + event("pass", "")},
		{Stdout: success, ExitCode: 1}, {Stdout: success, TimedOut: true}, {Stdout: success + success}, {Stdout: strings.ReplaceAll(success, "TestHiddenT01Pagination", "TestOther")},
		{Stdout: event("output", "TestHiddenT01Pagination") + event("pass", "")},
	} {
		if judgeTestPassed(result, "TestHiddenT01Pagination") {
			t.Fatal("跳过、伪造、非零或缺失目标测试不能通过")
		}
	}
}
func TestJudgeManifestAndHiddenFileIntegrity(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "trusted", "harness"), 0700); err != nil {
		t.Fatal(err)
	}
	hidden := []byte("package issuedesk\n")
	issue := []byte("冻结题面")
	if err := os.WriteFile(filepath.Join(root, "trusted", "harness", "judge_test.go"), hidden, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "issue.md"), issue, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "trusted", "driver"), 0700); err != nil {
		t.Fatal(err)
	}
	trusted := map[string]string{"trusted/harness/judge_test.go": judgeSHA(hidden)}
	for name, body := range map[string][]byte{"trusted/harness/go.mod": []byte("module ai-workflow-trusted-judge\n"), "trusted/harness/go.sum": []byte("冻结依赖"), "trusted/driver/main.go.txt": []byte("package main\n")} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), body, 0600); err != nil {
			t.Fatal(err)
		}
		trusted[name] = judgeSHA(body)
	}
	manifest := map[string]any{"version": "v1", "baseline_files": map[string]string{"go.mod": strings.Repeat("a", 64), "go.sum": strings.Repeat("b", 64)}, "trusted_files": trusted, "tasks": []any{map[string]string{"id": "T01", "issue_path": "issue.md", "issue_sha256": judgeSHA(issue), "test_pattern": "^TestHiddenT01Pagination$"}}}
	body, _ := json.Marshal(manifest)
	path := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	evaluation := domain.EvaluationContext{TaskID: "T01", ManifestVersion: "v1", ManifestDigest: judgeSHA(body)}
	bundle, err := loadEvaluationJudgeBundle(path, evaluation)
	if err != nil || bundle.TestName != "TestHiddenT01Pagination" {
		t.Fatalf("冻结材料读取失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "trusted", "harness", "judge_test.go"), []byte("被篡改"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEvaluationJudgeBundle(path, evaluation); err == nil {
		t.Fatal("隐藏判定材料变动必须拒绝")
	}
	evaluation.ManifestDigest = strings.Repeat("0", 64)
	if _, err := loadEvaluationJudgeBundle(path, evaluation); err == nil {
		t.Fatal("清单变动必须拒绝")
	}
}
func TestJudgeRejectsUnfrozenOrActiveCandidate(t *testing.T) {
	sha := strings.Repeat("a", 40)
	w := domain.Workflow{Control: "ACTIVE", Phase: "EVALUATION_COMPLETE", HeadSHA: sha, BaseSHA: sha, TestedSHA: sha, TestedTreeSHA: sha, Spec: domain.Spec{Approved: true}, Evaluation: &domain.EvaluationContext{TrialID: "trial"}}
	if !judgeWorkflowReady(w) {
		t.Fatal("固定终态候选应允许判定")
	}
	for _, change := range []func(*domain.Workflow){func(x *domain.Workflow) { x.TestedSHA = "" }, func(x *domain.Workflow) { x.Control = "CANCELLED" }, func(x *domain.Workflow) { x.Phase = "REVIEWING" }, func(x *domain.Workflow) { x.LeaseUntil = time.Now().Add(time.Minute) }} {
		candidate := w
		change(&candidate)
		if judgeWorkflowReady(candidate) {
			t.Fatal("未冻结或活跃候选不得判定")
		}
	}
}

func TestJudgeExternalHarnessMustActuallyCompleteTarget(t *testing.T) {
	lines := `{"Action":"run","Test":"TestHiddenT01Pagination","Package":"ai-workflow-trusted-judge"}
{"Action":"fail","Test":"TestHiddenT01Pagination","Package":"ai-workflow-trusted-judge"}
{"Action":"fail","Package":"ai-workflow-trusted-judge"}
`
	if !judgeTestCompleted(sandbox.RunResult{ExitCode: 1, Stdout: lines}, "TestHiddenT01Pagination") {
		t.Fatal("真实隐藏断言失败必须可记录")
	}
	for _, result := range []sandbox.RunResult{{ExitCode: 1, Stdout: `{"Action":"fail","Package":"ai-workflow-trusted-judge"}`}, {ExitCode: 0}, {Stdout: strings.ReplaceAll(lines, `"fail"`, `"skip"`)}, {Stdout: lines, TimedOut: true}} {
		if judgeTestCompleted(result, "TestHiddenT01Pagination") {
			t.Fatal("harness编译/缺失/跳过/超时不能冒充候选失败")
		}
	}
}
