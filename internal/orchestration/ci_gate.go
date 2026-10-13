package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
)

// trustedCIGate consumes an independently signed executor receipt from the
// controller evidence directory. Candidate workspaces cannot write this path.
// API status, exact attempt and source are re-read even for a stored receipt.
func (e *Engine) trustedCIGate(ctx context.Context, w domain.Workflow) (*domain.Evidence, error) {
	configured := e.repository(w).TrustedCI
	if configured == nil {
		return nil, nil
	}
	policy := TrustedCIPolicy{RepositoryID: configured.RepositoryID, Repository: configured.Repository, WorkflowID: configured.WorkflowID, WorkflowPath: configured.WorkflowPath, SourceCommitSHA: configured.SourceCommitSHA, SourceBlobSHA: configured.SourceBlobSHA, ReceiptPublicKey: configured.ReceiptPublicKey, MaxRecordAgeSeconds: configured.MaxRecordAgeSeconds}
	if policy.RepositoryID != w.RepositoryID || policy.Repository != w.Repository {
		return nil, errors.New("可信 CI 策略仓库与工作流不一致")
	}
	key := e.key(ctx, w)
	path := filepath.Join(e.Config().DataDir, "ci-receipts", w.ID, key+".json")
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &TrustedCIPendingError{Reason: "等待独立可信执行器签发当前候选的 CI 回执"}
		}
		return nil, fmt.Errorf("可信 CI 回执不可读取：%w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("可信 CI 回执文件类型或大小无效")
	}
	var receipt SignedCIReceipt
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&receipt); err != nil {
		return nil, err
	}
	candidate := CICandidateIdentity{RepositoryID: w.RepositoryID, PullRequest: w.PR, Generation: w.Generation, VerificationKey: key, HeadSHA: w.HeadSHA, BaseSHA: w.BaseSHA, TestMergeSHA: w.TestedSHA, TreeSHA: w.TestedTreeSHA, RunID: receipt.Payload.Candidate.RunID, RunAttempt: receipt.Payload.Candidate.RunAttempt, NotBefore: w.CreatedAt}
	client, _, _, err := e.client(ctx, w)
	if err != nil {
		return nil, err
	}
	record, err := FetchTrustedCIRecord(ctx, client, policy, candidate, receipt, time.Now().UTC())
	if errors.Is(err, ErrCIReceiptSuperseded) {
		if sourceErr := validateWaitingCISource(ctx, client, policy, candidate); sourceErr != nil {
			return nil, sourceErr
		}
		return nil, &TrustedCIPendingError{Reason: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	if record.Verdict == "PENDING" {
		return nil, &TrustedCIPendingError{Reason: record.Reason}
	}
	if !record.Current(policy, candidate, time.Now().UTC()) {
		return nil, fmt.Errorf("可信 CI 尚未通过：%s（%s）", record.Reason, record.Verdict)
	}
	ev, err := e.artifact(ctx, w, "ci:"+domain.Digest(record), "TRUSTED_CI", record)
	if err != nil {
		return nil, err
	}
	ev.TestedCommitSHA = w.TestedSHA
	ev.TestedTreeSHA = w.TestedTreeSHA
	return &ev, nil
}
