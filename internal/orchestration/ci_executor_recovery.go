package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// recoverCIExecutors 只读取控制平面 journal；活跃签发器持有互斥锁时跳过。
func (e *Engine) recoverCIExecutors(ctx context.Context) error {
	paths, err := filepath.Glob(filepath.Join(e.Config().DataDir, "ci-executors", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return errors.New("读取可信签发器恢复记录失败")
		}
		var journal ciExecutorJournal
		if json.Unmarshal(body, &journal) != nil || journal.WorkflowID == "" {
			return errors.New("可信签发器恢复记录格式无效")
		}
		if journal.Status == "STOPPED" {
			var charged bool
			if err = e.Store.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM processed_commands WHERE command_id=$1 AND workflow_id=$2)`, "runtime:"+journal.RunID, journal.WorkflowID).Scan(&charged); err != nil {
				return err
			}
			if charged {
				continue
			}
		}
		if err = e.RecoverTrustedCIExecutor(ctx, journal.WorkflowID); err != nil && !errors.Is(err, ErrCIExecutorBusy) {
			return err
		}
	}
	return e.recoverEvaluationJudges(ctx)
}
