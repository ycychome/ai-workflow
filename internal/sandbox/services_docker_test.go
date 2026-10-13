package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func serviceWorkspace(t *testing.T, ctx context.Context, m *Manager, id string) *Workspace {
	t.Helper()
	w, err := m.CreateWithServices(ctx, id, map[string]File{"source.txt": {Data: []byte("source\n")}}, []string{"postgres"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Destroy(ctx, w); err != nil {
			t.Error(err)
		}
	})
	return w
}
func serviceSQL(t *testing.T, ctx context.Context, m *Manager, w *Workspace, sql string) RunResult {
	t.Helper()
	r, err := m.Run(ctx, w, []string{"/bin/sh", "-ceu", `exec psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 -At -c "$1"`, "sql", sql}, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPostgresAttemptUnixSocketAndIsolationDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	t.Setenv("DATABASE_URL", "postgres://control-plane-must-never-be-forwarded.invalid/production")
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := serviceWorkspace(t, ctx, m, t.Name()+"-a")
	b := serviceWorkspace(t, ctx, m, t.Name()+"-b")
	r := serviceSQL(t, ctx, m, a, "CREATE TABLE only_a(value text); INSERT INTO only_a VALUES ('private'); SELECT inet_server_addr() IS NULL;")
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "t") {
		t.Fatalf("unix socket query failed: %+v", r)
	}
	r = serviceSQL(t, ctx, m, b, "SELECT to_regclass('public.only_a') IS NULL;")
	if r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "t" {
		t.Fatalf("attempt database leaked: %+v", r)
	}
	r, err = m.Run(ctx, a, []string{"/bin/sh", "-ceu", `test ! -e /var/lib/postgresql/data; test -S /run/aiwf-postgres/.s.PGSQL.5432; ! touch /run/aiwf-postgres/worker-write; case "$DATABASE_URL" in *control-plane*) exit 1;; esac; test "$GOPROXY" = off`}, 10*time.Second)
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("worker isolation: %+v %v", r, err)
	}
	out, err := m.dockerOutput(ctx, "inspect", a.postgresContainer)
	if err != nil {
		t.Fatal(err)
	}
	var inspected []struct {
		Config     struct{ Labels map[string]string }
		HostConfig struct {
			NetworkMode    string
			Memory         int64
			PidsLimit      int64
			ReadonlyRootfs bool
			Tmpfs          map[string]string
			LogConfig      struct {
				Type   string
				Config map[string]string
			}
		}
	}
	if err = json.Unmarshal(out, &inspected); err != nil || len(inspected) != 1 {
		t.Fatal("invalid service inspection")
	}
	p := inspected[0]
	if p.HostConfig.NetworkMode != "none" || p.HostConfig.Memory != 768<<20 || p.HostConfig.PidsLimit != 64 || !p.HostConfig.ReadonlyRootfs || !strings.Contains(p.HostConfig.Tmpfs["/var/lib/postgresql/data"], "size=512m") || p.HostConfig.LogConfig.Config["max-size"] != "5m" || p.Config.Labels[runLabel] != runHash(a.ID) {
		t.Fatal("service resource limits or identity missing")
	}
}

func TestPostgresRecoveryTargetsOnlyExpiredRunDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := serviceWorkspace(t, ctx, m, t.Name()+"-expired")
	b := serviceWorkspace(t, ctx, m, t.Name()+"-live")
	recovery, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = recovery.RecoverWorkspace(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err = recovery.confirmRunAbsent(ctx, "label="+runLabel+"="+runHash(a.ID)); err != nil {
		t.Fatal(err)
	}
	r := serviceSQL(t, ctx, m, b, "SELECT 42;")
	if r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "42" {
		t.Fatalf("live attempt affected by recovery: %+v", r)
	}
	if err = m.Cancel(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err = m.confirmRunAbsent(ctx, "label="+runLabel+"="+runHash(b.ID)); err != nil {
		t.Fatal(err)
	}
}
