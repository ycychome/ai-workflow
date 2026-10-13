package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentConcurrencyConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, yaml       string
		global, workflow int
		invalid          bool
	}{
		{"defaults", "", 4, 2, false},
		{"legacy", "builders: 5\n  reviewers: 3", 8, 5, false},
		{"explicit", "agent_concurrency: 9\n  workflow_agent_concurrency: 4", 9, 4, false},
		{"zero", "agent_concurrency: 0", 0, 0, true},
		{"fraction", "agent_concurrency: 3.5", 0, 0, true},
		{"fraction tasks", "max_tasks_per_workflow: 3.5", 0, 0, true},
		{"larger workflow", "agent_concurrency: 2\n  workflow_agent_concurrency: 3", 0, 0, true},
		{"zero tasks", "max_tasks_per_workflow: 0", 0, 0, true},
		{"zero evidence", "max_evidence_requests: 0", 0, 0, true},
		{"workflow ceiling unchanged", "workflow_concurrency: 5", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte("database_url: postgres://localhost/test\nrunner:\n  "+tc.yaml+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(p)
			if tc.invalid {
				if err == nil {
					t.Fatal("expected invalid configuration")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Runner.AgentConcurrency != tc.global || c.Runner.WorkflowAgentConcurrency != tc.workflow {
				t.Fatalf("capacities = %d/%d", c.Runner.AgentConcurrency, c.Runner.WorkflowAgentConcurrency)
			}
			if c.Runner.MaxTasksPerWorkflow != 64 || c.Runner.MaxEvidenceRequests != 64 {
				t.Fatal("missing safety defaults")
			}
		})
	}
}
