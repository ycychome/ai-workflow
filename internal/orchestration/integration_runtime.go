package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ycychome/ai-workflow/internal/domain"
)

type integrationRuntimeJournal struct {
	ciExecutorJournal
	AccountingID string `json:"accounting_id"`
}

func (e *Engine) integrationRuntimePath(workflowID, recordID string) string {
	return filepath.Join(e.Config().DataDir, "integration-runtime", domain.Digest(workflowID), domain.Digest(recordID)+".json")
}

// 调用者已持有当前工作流租约或集成互斥锁；停止确认后才补记旧执行。
func (e *Engine) recoverIntegrationRuntime(ctx context.Context, workflowID, recordID string) error {
	manager := e.sandboxManager()
	if manager == nil {
		return errors.New("集成沙箱管理器不可用")
	}
	if err := manager.StopRun(ctx, recordID); err != nil {
		return err
	}
	path := e.integrationRuntimePath(workflowID, recordID)
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	} // PREPARING 落库后尚未开始实际执行。
	if err != nil {
		return err
	}
	var journal integrationRuntimeJournal
	if json.Unmarshal(body, &journal) != nil || journal.WorkflowID != workflowID || journal.RunID != recordID || !strings.HasPrefix(journal.AccountingID, "integration-runtime-") || journal.StartedAt.IsZero() {
		return errors.New("集成执行时间恢复记录身份无效")
	}
	if journal.StoppedAt.IsZero() {
		journal.StoppedAt = time.Now().UTC()
	}
	journal.Status = "STOPPED"
	journal.UpdatedAt = time.Now().UTC()
	if err = writeCIExecutorJSON(path, journal); err != nil {
		return err
	}
	return e.chargeTrustedRuntime(ctx, workflowID, journal.AccountingID, int64((journal.StoppedAt.Sub(journal.StartedAt)+time.Second-1)/time.Second))
}
