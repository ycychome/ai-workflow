package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceRejectsUnverifiedStorageBackend(t *testing.T) {
	for _, tc := range []struct{ name, stat string }{
		{"disk filesystem", "ext2/ext3 4096 4096 1024 abc"},
		{"unbounded blocks", "tmpfs 4096 999999 1024 abc"},
		{"unbounded inodes", "tmpfs 4096 4096 999999 abc"},
		{"missing identity", "tmpfs 4096 4096 1024 0"},
		{"unreadable metadata", "not statfs metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := filepath.Join(t.TempDir(), "docker")
			script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> "$0.calls"
case "$1" in
  run) echo keeper-id ;;
  inspect) echo '[{"Id":"keeper-id","RestartCount":0,"State":{"Running":true,"StartedAt":"original"}}]' ;;
  exec) printf '%%s\n' '%s' ;;
  ps) echo keeper-id ;;
esac
`, tc.stat)
			if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			m, err := NewManager(Options{DockerBinary: fake, WorkspaceBytes: 16 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if w, err := m.Create(context.Background(), "reject-unbounded", map[string]File{"source": {Data: []byte("source")}}); err == nil || w != nil {
				t.Fatalf("unverified backend accepted: workspace=%v err=%v", w, err)
			}
			calls, err := os.ReadFile(fake + ".calls")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(calls), "rm -f keeper-id") || !strings.Contains(string(calls), "volume rm --force aiwf-ws-") {
				t.Fatalf("failed storage setup was not cleaned: %s", calls)
			}
		})
	}
}

func TestDestroyDeadlinePreservesHandleForCleanupRetry(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{DockerBinary: fake})
	if err != nil {
		t.Fatal(err)
	}
	w := &Workspace{ID: "cleanup-deadline", manager: m, volume: "test-only-fake-volume"}
	m.workspaces[w] = struct{}{}
	w.lifecycleMu.RLock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = m.Destroy(ctx, w)
	w.lifecycleMu.RUnlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Destroy did not honor its deadline: %v", err)
	}
	if err := m.checkWorkspace(w); err != nil {
		t.Fatalf("failed cleanup discarded its retry handle: %v", err)
	}
	if err := m.Destroy(context.Background(), w); err != nil {
		t.Fatalf("cleanup retry failed: %v", err)
	}
	if err := m.checkWorkspace(w); err == nil {
		t.Fatal("successful cleanup retained its handle")
	}
}

func TestRecoverRemovesKeepersAndVolumesWithManagedLabels(t *testing.T) {
	// This is a fake Docker binary: never call real Recover alongside the
	// user's running supervisor, whose workspaces carry the same labels.
	fake := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$0.calls"
case "$*" in
  'ps -aq --filter label=ai-workflow.managed=1') printf 'worker-id\nkeeper-id\n' ;;
  'volume ls -q --filter label=ai-workflow.managed=1') echo bounded-volume ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{DockerBinary: fake})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(fake + ".calls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rm -f worker-id", "rm -f keeper-id", "ps -aq --filter volume=bounded-volume", "volume rm bounded-volume"} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("missing recovery step %q: %s", want, calls)
		}
	}
}
