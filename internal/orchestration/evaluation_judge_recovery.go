package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// recoverEvaluationJudges 以独立进程互斥锁确认执行器已经退出，再回收专用沙箱。
func (e *Engine) recoverEvaluationJudges(ctx context.Context) error {
	paths, err := filepath.Glob(filepath.Join(e.Config().DataDir, "evaluation-judges", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var journal evaluationJudgeJournal
		if json.Unmarshal(body, &journal) != nil || journal.WorkflowID == "" || !strings.HasPrefix(journal.RunID, "evaluation-judge-") {
			return errors.New("隐藏判定恢复记录无效")
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
		if err = e.recoverEvaluationJudge(ctx, path, journal); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) recoverEvaluationJudge(ctx context.Context, path string, journal evaluationJudgeJournal) error {
	tx, err := e.Store.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, trustedExecutionLockKey(journal.WorkflowID)).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	// 取得互斥锁后重新读取，防止扫描期间新执行器替换了恢复记录。
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var current evaluationJudgeJournal
	if json.Unmarshal(body, &current) != nil || current.WorkflowID != journal.WorkflowID || !strings.HasPrefix(current.RunID, "evaluation-judge-") {
		return errors.New("隐藏判定恢复记录身份已改变")
	}
	journal = current
	manager := e.sandboxManager()
	if manager == nil {
		return errors.New("隐藏判定沙箱管理器不可用")
	}
	if err = manager.StopRun(ctx, journal.RunID); err != nil {
		return errors.New("隐藏判定遗留资源尚未停止确认")
	}
	journal.Status = "STOPPED"
	if journal.StoppedAt.IsZero() {
		journal.StoppedAt = time.Now().UTC()
	}
	if journal.StartedAt.IsZero() {
		return errors.New("隐藏判定恢复记录缺少开始时间")
	}
	if err = e.chargeTrustedRuntime(ctx, journal.WorkflowID, journal.RunID, int64((journal.StoppedAt.Sub(journal.StartedAt)+time.Second-1)/time.Second)); err != nil {
		return err
	}
	journal.UpdatedAt = time.Now().UTC()
	if err = writeCIExecutorJSON(path, journal); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
