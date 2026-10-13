package orchestration

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestIntegrationRuntimeDockerRecoveryChargesExactlyOnce(t *testing.T) {
	alphaRequireDocker(t)
	e, ctx := safetyRegressionDB(t)
	w := alphaClaimWorkflow(t, e, ctx, "DEVELOPING", nil)
	recordID := "integration-runtime-recovery-test-" + e.owner
	path := e.integrationRuntimePath(w.ID, recordID)
	start := time.Now().Add(-time.Minute)
	journal := integrationRuntimeJournal{ciExecutorJournal: ciExecutorJournal{RunID: recordID, WorkflowID: w.ID, Status: "PREPARING", StartedAt: start, StoppedAt: start.Add(4100 * time.Millisecond)}, AccountingID: "integration-runtime-first"}
	if err := writeCIExecutorJSON(path, journal); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := e.recoverIntegrationRuntime(ctx, w.ID, recordID); err != nil {
			t.Fatal(err)
		}
	}
	used, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil || used != 5 {
		t.Fatalf("崩溃恢复漏计或重复扣减：%d %v", used, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(body, &journal); err != nil || journal.Status != "STOPPED" {
		t.Fatalf("停止确认未持久化：%+v %v", journal, err)
	}
	journal.AccountingID = "integration-runtime-second"
	journal.StartedAt = start
	journal.StoppedAt = start.Add(time.Second)
	if err = writeCIExecutorJSON(path, journal); err != nil {
		t.Fatal(err)
	}
	if err = e.recoverIntegrationRuntime(ctx, w.ID, recordID); err != nil {
		t.Fatal(err)
	}
	used, err = e.Store.AgentRuntimeSeconds(ctx, w.ID)
	if err != nil || used != 6 {
		t.Fatalf("同一集成的第二次尝试未独立计时：%d %v", used, err)
	}
}
