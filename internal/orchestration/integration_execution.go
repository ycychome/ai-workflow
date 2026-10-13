package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/store"
)

func replaceIntegration(w *domain.Workflow, record domain.IntegrationRecord) {
	for i, prior := range w.IntegrationRecords {
		if prior.ID == record.ID {
			w.IntegrationRecords[i] = record
			return
		}
	}
	w.IntegrationRecords = append(w.IntegrationRecords, record)
}

func (e *Engine) integrateAndPublish(ctx context.Context, w *domain.Workflow, inputs []IntegrationInput, taskIDs []string) error {
	record := PendingIntegration(*w, inputs)
	for _, prior := range w.IntegrationRecords {
		if prior.ID == record.ID {
			record.CreatedAt = prior.CreatedAt
		}
	}
	replaceIntegration(w, record)
	if err := e.Store.Save(ctx, w, statusAction(*w)); err != nil {
		return err
	}

	result, err := e.IntegrateSourceArtifacts(ctx, *w, inputs)
	if err != nil {
		if result.Record.ID == "" {
			result.Record = record
		}
		var conflict *IntegrationConflict
		if errors.As(err, &conflict) {
			result.Record.Status = "CONFLICT"
			result.Record.Reason = conflict.Error()
			w.LastError = conflict.Error()
			w.Phase = "PLANNING"
			// Keep source artifacts for audit. The planner must author new intents
			// using the actual conflicting scope and cannot overwrite accepted work.
			for i, t := range w.Tasks {
				for _, id := range taskIDs {
					if t.ID == id && t.Status == "VALIDATED" {
						w.Tasks[i].Status = "RETRY_WAIT"
					}
				}
			}
			replaceIntegration(w, result.Record)
			return e.Store.Save(ctx, w, statusAction(*w))
		}
		if result.Record.Status != "CLEANUP_PENDING" {
			result.Record.Status = "FAILED"
			result.Record.Reason = err.Error()
		}
		replaceIntegration(w, result.Record)
		if saveErr := e.Store.Save(context.WithoutCancel(ctx), w, statusAction(*w)); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		return err
	}
	if result.Record.Status != "VALIDATED" || result.Record.TreeSHA == "" {
		return errors.New("集成结果缺少已验证树身份")
	}
	for _, artifact := range result.Artifacts {
		for i, prior := range w.SourceArtifacts {
			if prior.ID == artifact.ID {
				w.SourceArtifacts[i] = artifact
			}
		}
	}
	replaceIntegration(w, result.Record)
	changes := make([]gh.FileChange, 0, len(result.Changes))
	for _, change := range result.Changes {
		mode := "100644"
		if change.Mode&0111 != 0 {
			mode = "100755"
		}
		changes = append(changes, gh.FileChange{Path: change.Path, Content: change.Data, Mode: mode, Delete: change.Deleted})
	}
	payload := publishActionData{Changes: changes, TaskIDs: taskIDs, Message: fmt.Sprintf("实现 Issue #%d 的任务 %s\n\nai-workflow action %s/%d", w.Issue, strings.Join(taskIDs, ","), w.ID, w.Generation), ExpectedParent: w.HeadSHA, BaseBranch: w.BaseBranch, BaseSHA: w.BaseSHA, Branch: w.Branch, IntegrationID: result.Record.ID, ExpectedTree: result.Record.TreeSHA}
	return e.Store.Save(ctx, w, store.NewAction(*w, "publish", payload), statusAction(*w))
}

// resumeIntegration runs only under the newly claimed workflow lease. Old
// PREPARING resources are stopped by exact run label before any replacement.
func (e *Engine) resumeIntegration(ctx context.Context, w *domain.Workflow) (bool, error) {
	for _, record := range w.IntegrationRecords {
		if record.Status != "PREPARING" && record.Status != "CLEANUP_PENDING" && record.Status != "FAILED" {
			continue
		}
		if err := e.recoverIntegrationRuntime(ctx, w.ID, IntegrationRecordRunID(record)); err != nil {
			return true, err
		}
		if record.Generation != w.Generation || record.ParentSHA != w.HeadSHA || record.SpecDigest != w.Spec.Digest {
			record.Status = "OBSOLETE"
			replaceIntegration(w, record)
			return true, e.Store.Save(ctx, w)
		}
		record.Status = "PREPARING"
		replaceIntegration(w, record)
		if err := e.Store.Save(ctx, w); err != nil {
			return true, err
		}
		inputs, err := e.LoadIntegrationInputs(ctx, *w, record)
		if err != nil {
			return true, err
		}
		ids := []string{}
		for _, input := range inputs {
			ids = append(ids, input.Task.ID)
		}
		return true, e.integrateAndPublish(ctx, w, inputs, ids)
	}
	// VALIDATED 与来源证据可能已提交，而 PREPARING 尚未落库；从受信证据重建。
	artifacts, err := unrecordedIntegrationArtifacts(*w)
	if err != nil {
		return true, err
	}
	if len(artifacts) == 0 {
		return false, nil
	}
	var pendingPublish bool
	if err = e.Store.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE workflow_id=$1 AND generation=$2 AND kind='publish' AND state IN('pending','running','unknown','retry_wait'))`, w.ID, w.Generation).Scan(&pendingPublish); err != nil {
		return true, err
	}
	if pendingPublish {
		return true, nil
	}
	seeds := make([]IntegrationInput, 0, len(artifacts))
	for _, artifact := range artifacts {
		task, _ := integrationTask(*w, artifact.TaskID)
		seeds = append(seeds, IntegrationInput{Task: task, Artifact: artifact})
	}
	record := PendingIntegration(*w, seeds)
	inputs, err := e.LoadIntegrationInputs(ctx, *w, record)
	if err != nil {
		return true, err
	}
	ids := make([]string, 0, len(inputs))
	for _, input := range inputs {
		ids = append(ids, input.Task.ID)
	}
	return true, e.integrateAndPublish(ctx, w, inputs, ids)
}

// unrecordedIntegrationArtifacts 只恢复当前已接受任务的唯一来源，已有任意集成记录的产物不重发。
func unrecordedIntegrationArtifacts(w domain.Workflow) ([]domain.SourceArtifact, error) {
	recorded := map[string]bool{}
	for _, record := range w.IntegrationRecords {
		for _, id := range record.ArtifactIDs {
			recorded[id] = true
		}
	}
	var out []domain.SourceArtifact
	for _, task := range w.Tasks {
		if task.Status != "VALIDATED" || taskKind(task) != "code" {
			continue
		}
		var found []domain.SourceArtifact
		for _, artifact := range w.SourceArtifacts {
			if artifact.TaskID == task.ID && artifact.AgentRunID == task.AgentRunID && artifact.Generation == w.Generation && artifact.ParentSHA == w.HeadSHA && artifact.SpecDigest == w.Spec.Digest && artifact.PlanRevision == task.PlanRevision {
				found = append(found, artifact)
			}
		}
		if len(found) != 1 {
			return nil, fmt.Errorf("已接受任务 %s 缺少唯一且匹配的来源产物，不能猜测重建", task.ID)
		}
		if !recorded[found[0].ID] {
			out = append(out, found[0])
		}
	}
	return out, nil
}

func validatePublishIntegration(w domain.Workflow, d publishActionData) error {
	if d.IntegrationID == "" || d.ExpectedTree == "" {
		return errors.New("发布请求缺少受信集成记录或候选树身份")
	}
	for _, record := range w.IntegrationRecords {
		if record.ID == d.IntegrationID {
			if (record.Status != "VALIDATED" && record.Status != "PUBLISHED") || record.TreeSHA != d.ExpectedTree || record.ParentSHA != d.ExpectedParent || record.Generation != w.Generation || record.SpecDigest != w.Spec.Digest {
				return errors.New("发布请求与集成记录身份不一致")
			}
			return nil
		}
	}
	return errors.New("发布请求引用了不存在的集成记录")
}

func (e *Engine) completeIntegrationReceipt(ctx context.Context, w *domain.Workflow, d publishActionData, sha, tree string) error {
	if err := validatePublishIntegration(*w, d); err != nil {
		return err
	}
	if tree != d.ExpectedTree || sha == "" {
		return errors.New("远端提交未匹配独立集成树")
	}
	for i := range w.IntegrationRecords {
		if w.IntegrationRecords[i].ID == d.IntegrationID {
			w.IntegrationRecords[i].Status = "PUBLISHED"
			w.IntegrationRecords[i].CommitSHA = sha
		}
	}
	bindPublishedResolutions(w, d.TaskIDs, sha, e.key(ctx, *w))
	return nil
}
