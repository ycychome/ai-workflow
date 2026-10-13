package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceQuotaDefaultsAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        int64
		valid       bool
	}{
		{"omitted", "", DefaultWorkspaceBytes, true},
		{"one byte", "1", 1, true},
		{"custom", "67108864", 64 << 20, true},
		{"maximum", "8589934592", MaxWorkspaceBytes, true},
		{"zero", "0", 0, false},
		{"negative", "-1", 0, false},
		{"over maximum", "8589934593", 0, false},
		{"overflow", "9223372036854775808", 0, false},
		{"fractional", "3.5", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := "database_url: postgres://unused\n"
			if tc.value != "" {
				data += fmt.Sprintf("runner:\n  workspace_bytes: %s\n", tc.value)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("Load valid=%v err=%v", tc.valid, err)
			}
			if err == nil && (cfg.Runner.WorkspaceBytes != tc.want || cfg.EffectiveWorkspaceBytes() != tc.want) {
				t.Fatalf("wrong quota: raw=%d effective=%d want=%d", cfg.Runner.WorkspaceBytes, cfg.EffectiveWorkspaceBytes(), tc.want)
			}
		})
	}
}
func TestWorkspaceQuotaProgrammaticDefault(t *testing.T) {
	cfg := Config{}
	if cfg.EffectiveWorkspaceBytes() != DefaultWorkspaceBytes {
		t.Fatal("zero-value config lost bounded default")
	}
	cfg.Runner.WorkspaceBytes = -1
	if ValidWorkspaceBytes(cfg.EffectiveWorkspaceBytes()) {
		t.Fatal("negative quota silently defaulted")
	}
}
func TestWorkspaceQuotaYAMLRoundTripPreservesCustomLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("database_url: postgres://unused\nrunner:\n  workspace_bytes: 67108864\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.Dev = "changed-model"
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "workspace_bytes: 67108864") {
		t.Fatal("unrelated config save dropped quota")
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil || again.Runner.WorkspaceBytes != 64<<20 {
		t.Fatalf("quota lost on reload: %d %v", again.Runner.WorkspaceBytes, err)
	}
}
