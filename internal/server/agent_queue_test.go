package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/orchestration"
)

func TestAgentCapacityIncludesStoppingAndExpiredActiveLease(t *testing.T) {
	runs := []domain.AgentRun{
		{Status: domain.AgentActive, LeaseUntil: time.Now().Add(-time.Hour)},
		{Status: domain.AgentStopping}, {Status: domain.AgentQueued},
		{Status: domain.AgentFinished}, {Status: domain.AgentObsolete},
	}
	got := summarizeAgentQueue(runs, 4)
	if got.Active != 1 || got.Stopping != 1 || got.Occupied != 2 || got.Queued != 1 || got.Limit != 4 {
		t.Fatalf("incorrect capacity: %+v", got)
	}
}
func TestDynamicAgentLimitsExposedWithoutSecrets(t *testing.T) {
	cfg := config.Config{}
	cfg.Runner.AgentConcurrency = 9
	cfg.Runner.WorkflowAgentConcurrency = 5
	cfg.Runner.MaxTasksPerWorkflow = 100
	cfg.Runner.MaxEvidenceRequests = 80
	cfg.Model.TokenFile = "/private/secret-token"
	got := publicConfig(cfg)["runner"].(map[string]any)
	for name, want := range map[string]int{"agent_concurrency": 9, "workflow_agent_concurrency": 5, "max_tasks_per_workflow": 100, "max_evidence_requests": 80} {
		if got[name] != want {
			t.Errorf("%s=%v, want %d", name, got[name], want)
		}
	}
	encoded, err := json.Marshal(publicConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), cfg.Model.TokenFile) {
		t.Fatal("public config exposed token location")
	}
	defaults := runnerPublicConfig(config.Config{})
	if defaults["agent_concurrency"] != 4 || defaults["workflow_agent_concurrency"] != 2 {
		t.Fatalf("effective defaults missing: %v", defaults)
	}
}
func TestSettingsRejectInvalidExplicitAgentLimitsWithoutPersistence(t *testing.T) {
	for _, field := range []string{"agent_concurrency", "workflow_agent_concurrency", "max_tasks_per_workflow", "max_evidence_requests"} {
		for _, value := range []string{"0", "-1", "1.5"} {
			engine, err := orchestration.New(nil, config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			h := &Handlers{engine: engine, configPath: path}
			response := httptest.NewRecorder()
			h.updateSettings(response, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"runner":{"`+field+`":`+value+`}}`)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("%s=%s accepted: %d", field, value, response.Code)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("invalid limits were persisted")
			}
		}
	}
}
func TestWorkflowViewIncludesAgentAuditState(t *testing.T) {
	view := workflowStateView{Workflow: domain.Workflow{ID: "wf-1"}, AgentRuns: []domain.AgentRun{{ID: "run-1", Role: "analyst", Status: domain.AgentStopping, LeaseEpoch: 3, QueueReason: "等待名额", ContextHash: "digest"}}, AgentQueue: agentQueueSummary{Stopping: 1, Occupied: 1, Limit: 2}}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	runs := result["agent_runs"].([]any)
	run := runs[0].(map[string]any)
	if run["role"] != "analyst" || run["status"] != "STOPPING" || run["lease_epoch"] != float64(3) {
		t.Fatalf("audit state lost: %s", encoded)
	}
}
