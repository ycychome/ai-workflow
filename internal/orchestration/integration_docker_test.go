package orchestration

import (
	"testing"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

func TestIntegrationDockerAcceptedEvidenceProducesExactTree(t *testing.T) {
	alphaRequireDocker(t)
	e, ctx := safetyRegressionDB(t)
	task := alphaCodeTask()
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", []domain.Task{task})
	run := regressionClaimRun(t, e, ctx, w, task, agent.RoleBuilder)
	task.AgentRunID = run.ID
	task.Status = "RUNNING"
	w.Tasks = []domain.Task{task}
	if err := e.Store.Save(ctx, &w); err != nil {
		t.Fatal(err)
	}
	changes := []sandbox.Change{{Path: "calculator.go", Status: "M", Data: []byte("package pilot\n\nfunc Add(a,b int) int { return a + b }\n"), Mode: 0644}}
	ev, err := e.artifact(ctx, w, "accepted-source", "SOURCE_ARTIFACT", sourceArtifactPayload{Generation: w.Generation, LeaseEpoch: task.LeaseEpoch, BaseSHA: w.HeadSHA, Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	ev.AgentRunID = run.ID
	if err = e.finishTaskAttempt(store.WithAgentFence(ctx, run), run.ID, w, task, "OUTPUT_READY", map[string]string{"summary": "合成已接受产物"}, []domain.Evidence{ev}, nil); err != nil {
		t.Fatal(err)
	}
	// 此合成 builder 从未启动进程，仅为真实集成器准备已接受的输入回执。
	if err = e.Store.MarkAgentStopping(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.FinishAgent(ctx, run.ID, run.Owner, run.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	task.Status = "VALIDATED"
	w.Tasks = []domain.Task{task}
	artifact := domain.SourceArtifact{ID: "source-" + domain.Digest([]any{run.ID, ev.Hash})[:24], TaskID: task.ID, AgentRunID: run.ID, Generation: w.Generation, PlanRevision: task.PlanRevision, SpecDigest: w.Spec.Digest, ParentSHA: w.HeadSHA, ChangesDigest: domain.Digest(changes), EvidenceID: ev.ID}
	w.SourceArtifacts = []domain.SourceArtifact{artifact}
	w.Evidence = []domain.Evidence{ev}
	inputs := []IntegrationInput{{Task: task, Artifact: artifact, Changes: changes}}
	w.IntegrationRecords = []domain.IntegrationRecord{PendingIntegration(w, inputs)}
	if err = e.Store.Save(ctx, &w); err != nil {
		t.Fatal(err)
	}
	result, err := e.IntegrateSourceArtifacts(ctx, w, inputs)
	if err != nil {
		t.Fatal(err)
	}
	expected := localFiles()
	expected["calculator.go"] = sandbox.File{Data: changes[0].Data, Mode: 0644}
	tree, err := GitTreeSHA(expected)
	if err != nil || result.Record.TreeSHA != tree || result.Record.Status != "VALIDATED" || len(result.Artifacts) != 1 || result.Artifacts[0].TreeSHA != tree {
		t.Fatalf("实际集成树不匹配：%+v %v", result, err)
	}
	used, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil || used < 1 {
		t.Fatalf("实际集成时间未计入预算：%d %v", used, err)
	}
}
