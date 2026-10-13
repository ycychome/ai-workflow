package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/orchestration"
	"github.com/ycychome/ai-workflow/internal/store"
)

func TestSettingsExposeEffectiveWorkspaceQuota(t *testing.T) {
	cfg := config.Config{}
	public := publicConfig(cfg)
	if public["runner"].(map[string]any)["workspace_bytes"] != config.DefaultWorkspaceBytes {
		t.Fatal("public config omitted bounded effective default")
	}
	cfg.Runner.WorkspaceBytes = 64 << 20
	engine, err := orchestration.New(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{engine: engine}
	response := httptest.NewRecorder()
	h.settings(response, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var body struct {
		Runner struct {
			WorkspaceBytes int64 `json:"workspace_bytes"`
		} `json:"runner"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK || body.Runner.WorkspaceBytes != 64<<20 {
		t.Fatalf("settings quota=%d status=%d err=%v", body.Runner.WorkspaceBytes, response.Code, err)
	}
}
func TestSettingsPersistencePreservesWorkspaceQuota(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("database_url: postgres://unused\nrunner:\n  workspace_bytes: 67108864\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.Dev = "unrelated-model-setting"
	h := &Handlers{configPath: path}
	if err := h.persistConfig(cfg); err != nil {
		t.Fatal(err)
	}
	again, err := config.Load(path)
	if err != nil || again.Runner.WorkspaceBytes != 64<<20 || again.Model.Dev != cfg.Model.Dev {
		t.Fatalf("settings save lost quota: quota=%d err=%v", again.Runner.WorkspaceBytes, err)
	}
}
func TestSettingsRejectInvalidExplicitWorkspaceQuota(t *testing.T) {
	for _, value := range []string{"0", "-1", "8589934593", "3.5"} {
		cfg := config.Config{}
		engine, err := orchestration.New(nil, cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "must-not-save.yaml")
		h := &Handlers{engine: engine, configPath: path}
		response := httptest.NewRecorder()
		h.updateSettings(response, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(fmt.Sprintf(`{"runner":{"workspace_bytes":%s}}`, value))))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid quota %s returned %d: %s", value, response.Code, response.Body.String())
		}
		if engine.Config().Runner.WorkspaceBytes != config.DefaultWorkspaceBytes {
			t.Fatal("rejected setting changed quota")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("rejected setting persisted config: %v", err)
		}
	}
}

func TestSettingsPostPreservesAndUpdatesWorkspaceQuotaPostgresIntegration(t *testing.T) {
	if os.Getenv("AI_WORKFLOW_FORCE_INTEGRATION") != "1" {
		t.Skip("opt-in isolated PostgreSQL regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load(os.Getenv("CONFIG_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("settings_quota_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	u, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	s, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.Runner.Mode = "fake"
	cfg.Runner.WorkspaceBytes = 64 << 20
	cfg.DataDir = t.TempDir()
	e, err := orchestration.New(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{engine: e, configPath: filepath.Join(t.TempDir(), "config.yaml")}
	if err := h.persistConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body string
		want int64
	}{{`{"runner":{"builders":1}}`, 64 << 20}, {`{"runner":{"workspace_bytes":33554432}}`, 32 << 20}} {
		response := httptest.NewRecorder()
		h.updateSettings(response, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(tc.body)).WithContext(ctx))
		if response.Code != http.StatusOK {
			t.Fatalf("settings save status=%d body=%s", response.Code, response.Body.String())
		}
		saved, err := config.Load(h.configPath)
		if err != nil || saved.Runner.WorkspaceBytes != tc.want || e.Config().Runner.WorkspaceBytes != tc.want {
			t.Fatalf("settings quota mismatch: saved=%d engine=%d err=%v", saved.Runner.WorkspaceBytes, e.Config().Runner.WorkspaceBytes, err)
		}
	}
}
