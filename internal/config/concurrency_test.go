package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkflowConcurrencyDefaultsAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want int
		ok   bool
	}{
		{name: "default", yaml: "database_url: postgres://unused\n", want: 2, ok: true},
		{name: "minimum", yaml: "database_url: postgres://unused\nrunner:\n  workflow_concurrency: 1\n", want: 1, ok: true},
		{name: "maximum", yaml: "database_url: postgres://unused\nrunner:\n  workflow_concurrency: 4\n", want: 4, ok: true},
		{name: "too low", yaml: "database_url: postgres://unused\nrunner:\n  workflow_concurrency: 0\n", ok: false},
		{name: "too high", yaml: "database_url: postgres://unused\nrunner:\n  workflow_concurrency: 5\n", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err == nil) != tc.ok {
				t.Fatalf("Load error = %v, want success %v", err, tc.ok)
			}
			if err == nil && cfg.Runner.WorkflowConcurrency != tc.want {
				t.Fatalf("workflow concurrency = %d, want %d", cfg.Runner.WorkflowConcurrency, tc.want)
			}
		})
	}
}
