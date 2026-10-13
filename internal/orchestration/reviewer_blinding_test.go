package orchestration

import (
	"context"
	"encoding/json"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDependentReviewerCannotInheritPeerObligationResults(t *testing.T) {
	w := domain.Workflow{Review: domain.Review{Obligations: []domain.Obligation{{ID: "O1", CriterionID: "AC1", Description: "独立验证行为", Result: "satisfied", Evidence: []string{"peer-proof"}}}}}
	task := domain.Task{ID: "review-b", DependsOn: []string{"review-a"}, ObligationIDs: []string{"O1"}}
	got := reviewTaskObligations(w, task, false)
	if len(got) != 1 || got[0].Result != "unverified" || len(got[0].Evidence) != 0 || got[0].Description != w.Review.Obligations[0].Description {
		t.Fatalf("盲审泄露同伴结论或丢失义务定义：%+v", got)
	}
	if strings.Contains(string(raw(got)), "peer-proof") {
		t.Fatal("序列化输入泄露同伴证据")
	}
	skeptic := reviewTaskObligations(w, task, true)
	if skeptic[0].Result != "satisfied" || skeptic[0].Evidence[0] != "peer-proof" {
		t.Fatal("质疑复核丢失同伴证据")
	}
	skeptic[0].Evidence[0] = "changed"
	if w.Review.Obligations[0].Evidence[0] != "peer-proof" {
		t.Fatal("提示词投影修改持久化义务")
	}
}

func TestIndependentReviewerCannotReadPeerEvidenceThroughTools(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := domain.Workflow{ID: "blind-review-tool", RepositoryID: 998766, Repository: "local/pilot", Issue: 91, Generation: 1, Phase: "REVIEWING", Control: "ACTIVE", Review: domain.Review{Tasks: []domain.Task{{ID: "review-a", Status: "ACCEPTED", Report: raw(map[string]string{"summary": "peer-secret-summary"})}}, Obligations: []domain.Obligation{{ID: "O1", Result: "satisfied", Evidence: []string{"peer-secret-proof"}}}}, Evidence: []domain.Evidence{{ID: "peer-secret-proof", Kind: "SOURCE_ANALYSIS"}}}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	session := toolSession{e: e, w: w, review: true, workspace: &sandbox.Workspace{ID: "blind-review"}}
	for _, tool := range session.tools() {
		if tool.Name == "read_evidence" {
			t.Fatal("独立审查者不应获得同伴证据工具")
		}
	}
	if _, err := session.handler(ctx, "read_evidence", raw(map[string]string{"evidence_id": "peer-secret-proof"})); err == nil {
		t.Fatal("未暴露工具的直接调用仍必须拒绝未分配证据")
	}
	state, err := session.handler(ctx, "workflow_state", raw(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state, "peer-secret") || strings.Contains(state, "satisfied") {
		t.Fatalf("workflow_state泄露同伴结论：%s", state)
	}
}

// This regression exercises the actual reviewer tool, not only write_file policy.
func TestReviewerRunCannotForgeFrozenCandidateEvidence(t *testing.T) {
	if os.Getenv("AI_WORKFLOW_FORCE_DOCKER") != "1" {
		t.Skip("requires isolated Docker acceptance environment")
	}
	e, ctx := safetyRegressionDB(t)
	manager, err := sandbox.NewManager(sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(ctx, t.Name(), map[string]sandbox.File{"source.txt": {Data: []byte("original\n"), Mode: 0644}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := manager.Destroy(cleanup, workspace); err != nil {
			t.Error(err)
		}
	}()
	if err := manager.SealReadOnly(ctx, workspace, true); err != nil {
		t.Fatal(err)
	}
	w := domain.Workflow{ID: "review-frozen-source", RepositoryID: 998766, Repository: "local/pilot", Issue: 92, Generation: 1, Phase: "REVIEWING", Control: "ACTIVE", HeadSHA: "candidate", TestedSHA: "merge", TestedTreeSHA: "tree"}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	session := toolSession{e: e, w: w, manager: manager, workspace: workspace, review: true, snapshotSHA: w.TestedSHA, snapshotTreeSHA: w.TestedTreeSHA}
	output, err := session.handler(ctx, "run", raw(map[string]any{"args": []string{"sh", "-ceu", `cp source.txt /tmp/original; printf 'forged\n' > source.txt; test "$(cat source.txt)" = forged; cp /tmp/original source.txt`}, "timeout_seconds": 10}))
	if err != nil {
		t.Fatalf("审查命令基础设施失败，不能作为禁止写入的证明：%v", err)
	}
	var body struct {
		Exit int `json:"exit_code"`
	}
	if err = json.Unmarshal([]byte(output), &body); err != nil {
		t.Fatal(err)
	}
	changes, err := manager.CollectChanges(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if body.Exit == 0 || len(changes) != 0 {
		t.Fatalf("review run modified frozen source; changes=%+v evidence=%s", changes, output)
	}
	if _, err := session.handler(ctx, "write_file", raw(map[string]string{"path": "review_tests/check.txt", "content": "补充测试"})); err != nil {
		t.Fatal(err)
	}
	check, err := manager.Run(ctx, workspace, []string{"sh", "-ceu", `test "$(cat source.txt)" = original; test -f review_tests/check.txt`}, 10*time.Second)
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("只读源码或独立补充目录不可用：%+v %v", check, err)
	}
}

func TestReviewerMechanicalEvidenceRequiresExactFrozenIdentity(t *testing.T) {
	w := domain.Workflow{HeadSHA: "head", BaseSHA: "base", TestedSHA: "merge", TestedTreeSHA: "tree"}
	good := domain.Evidence{ID: "controller", Hash: "hash", Kind: "TRUSTED_CHECK", HeadSHA: w.HeadSHA, BaseSHA: w.BaseSHA, TestedCommitSHA: w.TestedSHA, TestedTreeSHA: w.TestedTreeSHA, ProfileDigest: "profile"}
	w.Evidence = append(w.Evidence, good)
	for _, field := range []string{"kind", "head", "base", "commit", "tree", "profile", "hash"} {
		wrong := good
		wrong.ID = field
		switch field {
		case "kind":
			wrong.Kind = "TOOL_VERIFIED"
		case "head":
			wrong.HeadSHA = "old"
		case "base":
			wrong.BaseSHA = "old"
		case "commit":
			wrong.TestedCommitSHA = "old"
		case "tree":
			wrong.TestedTreeSHA = "old"
		case "profile":
			wrong.ProfileDigest = "old"
		case "hash":
			wrong.Hash = ""
		}
		w.Evidence = append(w.Evidence, wrong)
	}
	failed := good
	failed.ID = "failed-check"
	failed.ExitCode = 1
	w.Evidence = append(w.Evidence, failed)
	got := currentReviewerMechanicalEvidence(w, "profile")
	if len(got) != 2 || got[0].ID != "controller" || got[1].ID != "failed-check" {
		t.Fatalf("受信机械证据筛选错误：%+v", got)
	}
	// Failed checks remain readable, but never establish a satisfied obligation.
	w.Review.Obligations = []domain.Obligation{{ID: "O1"}}
	report := reviewReport{Obligations: []domain.Obligation{{ID: "O1", Result: "satisfied", Evidence: []string{"controller"}}}}
	if err := mergeReportWithMechanical(&w, report, []string{"O1"}, nil, false, got); err != nil {
		t.Fatal(err)
	}
	report.Obligations[0].Evidence = []string{"failed-check"}
	if err := mergeReportWithMechanical(&w, report, []string{"O1"}, nil, false, got); err == nil {
		t.Fatal("失败机械证据不能证明义务通过")
	}
	finding := domain.Finding{ID: "F1", Severity: "high", State: "REFUTED", EvidenceType: "TOOL_VERIFIED", Evidence: []string{"controller"}, Description: "反证", Expected: "预期", Actual: "实际", Preconditions: "前提"}
	report.Obligations = nil
	report.Findings = []domain.Finding{finding}
	if err := mergeReportWithMechanical(&w, report, nil, nil, true, got); err == nil {
		t.Fatal("共享机械证据不能冒充本次独立反证")
	}
}

func TestReviewerCanReadControllerProofWithoutPeerProof(t *testing.T) {
	e, ctx := safetyRegressionDB(t)
	w := domain.Workflow{ID: "review-controller-proof", RepositoryID: 998766, Repository: "local/pilot", Issue: 93, Generation: 1, Phase: "REVIEWING", Control: "ACTIVE", BaseSHA: "base", HeadSHA: "head", TestedSHA: "merge", TestedTreeSHA: "tree"}
	if err := e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	proof, err := e.artifact(ctx, w, "controller-check", "TRUSTED_CHECK", map[string]string{"stdout": "可信验证记录"})
	if err != nil {
		t.Fatal(err)
	}
	proof.TestedCommitSHA = w.TestedSHA
	proof.TestedTreeSHA = w.TestedTreeSHA
	peer := proof
	peer.ID = "peer-proof"
	peer.Kind = "TOOL_VERIFIED"
	w.Evidence = []domain.Evidence{proof, peer}
	session := toolSession{e: e, w: w, review: true, workspace: &sandbox.Workspace{ID: "controller-reader"}, readableEvidence: e.reviewerMechanicalEvidence(w)}
	output, err := session.handler(ctx, "read_evidence", raw(map[string]string{"evidence_id": proof.ID}))
	if err != nil || !strings.Contains(output, "可信验证记录") {
		t.Fatalf("不能读取当前机械验证记录：%s %v", output, err)
	}
	if _, err := session.handler(ctx, "read_evidence", raw(map[string]string{"evidence_id": peer.ID})); err == nil {
		t.Fatal("获得机械证据工具不应授予同伴证据访问权")
	}
	if len(session.listEvidence()) != 0 {
		t.Fatal("读取已有证明不得洗成新的独立证据")
	}
}
