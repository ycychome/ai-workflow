package orchestration

import (
	"github.com/ycychome/ai-workflow/internal/domain"
	"testing"
)

func recoveryIntegrationFixture() domain.Workflow {
	return domain.Workflow{ID: "workflow", Generation: 2, HeadSHA: "head", Spec: domain.Spec{Digest: "spec"}, Tasks: []domain.Task{{ID: "task", Kind: "code", Status: "VALIDATED", AgentRunID: "run", PlanRevision: 3}}, SourceArtifacts: []domain.SourceArtifact{{ID: "artifact", TaskID: "task", AgentRunID: "run", Generation: 2, ParentSHA: "head", SpecDigest: "spec", PlanRevision: 3}}}
}
func TestRecoverValidatedBeforePreparing(t *testing.T) {
	w := recoveryIntegrationFixture()
	artifacts, err := unrecordedIntegrationArtifacts(w)
	if err != nil || len(artifacts) != 1 || artifacts[0].ID != "artifact" {
		t.Fatalf("崩溃窗口来源未被重建：%+v %v", artifacts, err)
	}
}
func TestRecoverNeverRepeatsRecordedArtifact(t *testing.T) {
	for _, status := range []string{"PREPARING", "VALIDATED", "PUBLISHED", "FAILED", "CLEANUP_PENDING", "CONFLICT"} {
		w := recoveryIntegrationFixture()
		w.IntegrationRecords = []domain.IntegrationRecord{{ID: "integration", Status: status, ArtifactIDs: []string{"artifact"}}}
		artifacts, err := unrecordedIntegrationArtifacts(w)
		if err != nil || len(artifacts) != 0 {
			t.Fatalf("已有 %s 记录的来源被重新发布：%v", status, err)
		}
	}
}
func TestRecoverRejectsAmbiguousOrStaleArtifact(t *testing.T) {
	for _, mutate := range []func(*domain.Workflow){
		func(w *domain.Workflow) { w.SourceArtifacts = nil },
		func(w *domain.Workflow) { w.SourceArtifacts = append(w.SourceArtifacts, w.SourceArtifacts[0]) },
		func(w *domain.Workflow) { w.SourceArtifacts[0].Generation-- },
		func(w *domain.Workflow) { w.SourceArtifacts[0].AgentRunID = "other" },
		func(w *domain.Workflow) { w.SourceArtifacts[0].ParentSHA = "old" },
	} {
		w := recoveryIntegrationFixture()
		mutate(&w)
		if _, err := unrecordedIntegrationArtifacts(w); err == nil {
			t.Fatal("来源身份不完整时不能猜测重建")
		}
	}
}
