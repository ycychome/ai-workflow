package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func quotaDockerContext(t *testing.T) context.Context {
	t.Helper()
	if os.Getenv("AI_WORKFLOW_FORCE_DOCKER") != "1" {
		t.Skip("set AI_WORKFLOW_FORCE_DOCKER=1 with the runner image available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func quotaWorkspace(t *testing.T, ctx context.Context) (*Manager, *Workspace) {
	t.Helper()
	m, err := NewManager(Options{WorkspaceBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Create(ctx, t.Name(), map[string]File{"source.txt": {Data: []byte("original\n")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := m.Destroy(cleanup, w); err != nil {
			t.Errorf("destroy bounded workspace: %v", err)
		}
		if _, err := m.dockerOutput(cleanup, "volume", "inspect", w.volume); err == nil {
			t.Error("workspace volume remains after Destroy")
		}
		if _, err := m.dockerOutput(cleanup, "inspect", w.keeper); err == nil {
			t.Error("workspace keeper remains after Destroy")
		}
	})
	return m, w
}

func quotaRun(t *testing.T, ctx context.Context, m *Manager, w *Workspace, script string) RunResult {
	t.Helper()
	r, err := m.Run(ctx, w, []string{"/bin/sh", "-c", script}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestWorkspaceQuotaPersistsAndRejectsAggregateWritesDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, w := quotaWorkspace(t, ctx)
	// Each file is individually below the old 64 MiB export limit. The sum
	// exceeds only this disposable 16 MiB filesystem, never the daemon disk.
	r := quotaRun(t, ctx, m, w, "dd if=/dev/zero of=first.bin bs=1M count=10 2>/dev/null")
	if r.ExitCode != 0 {
		t.Fatalf("first bounded write failed: %+v", r)
	}
	r = quotaRun(t, ctx, m, w, "test $(wc -c < first.bin) -eq 10485760 && dd if=/dev/zero of=second.bin bs=1M count=10")
	if r.ExitCode == 0 || !strings.Contains(r.Stderr, "No space left on device") {
		t.Fatalf("aggregate quota was not enforced: %+v", r)
	}
	if got, err := m.ReadFile(ctx, w, "source.txt"); err != nil || string(got) != "original\n" {
		t.Fatalf("source lost after command exits/quota failure: %q %v", got, err)
	}
	r = quotaRun(t, ctx, m, w, "rm first.bin second.bin")
	if r.ExitCode != 0 {
		t.Fatalf("cannot recover space: %+v", r)
	}
	if err := m.WriteFile(ctx, w, "source.txt", File{Data: []byte("changed\n")}); err != nil {
		t.Fatal(err)
	}
	r = quotaRun(t, ctx, m, w, "test \"$(cat source.txt)\" = changed && printf persisted > next.txt")
	if r.ExitCode != 0 {
		t.Fatalf("cross-command persistence failed: %+v", r)
	}
	changes, err := m.CollectChanges(ctx, w)
	if err != nil || fmt.Sprint(pathsOf(changes)) != "[next.txt source.txt]" {
		t.Fatalf("export after quota failure/recovery: paths=%v err=%v", pathsOf(changes), err)
	}
}

func TestWorkspaceQuotaBoundsInodesDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, w := quotaWorkspace(t, ctx)
	// At most 2048 empty files are attempted on the small test volume.
	r := quotaRun(t, ctx, m, w, "mkdir small; i=0; while [ $i -lt 2048 ]; do : > small/$i || exit 42; i=$((i+1)); done")
	if r.ExitCode == 0 || !strings.Contains(r.Stderr, "No space left on device") {
		t.Fatalf("inode quota was not enforced: %+v", r)
	}
}

func TestWorkspaceKeeperLossFailsClosedDocker(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			ctx := quotaDockerContext(t)
			m, w := quotaWorkspace(t, ctx)
			if _, err := m.dockerOutput(ctx, "stop", "--time", "1", w.keeper); err != nil {
				t.Fatal(err)
			}
			if restart {
				if _, err := m.dockerOutput(ctx, "start", w.keeper); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.Run(ctx, w, []string{"true"}, time.Second); err == nil {
				t.Fatal("command accepted a lost/restarted keeper")
			}
			if _, err := m.ReadFile(ctx, w, "source.txt"); err == nil {
				t.Fatal("source read accepted a lost/restarted keeper")
			}
			if err := m.WriteFile(ctx, w, "new.txt", File{Data: []byte("not allowed")}); err == nil {
				t.Fatal("source write accepted a lost/restarted keeper")
			}
		})
	}
}

func TestWorkspaceInitializationQuotaFailureCleansResourcesDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, err := NewManager(Options{WorkspaceBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("quota-init-failure-%d", time.Now().UnixNano())
	// Oversize input is bounded to 17 MiB and only touches our small tmpfs.
	if w, err := m.Create(ctx, id, map[string]File{"large": {Data: make([]byte, 17<<20)}}); err == nil || w != nil || !strings.Contains(err.Error(), "write workspace snapshot") || !(strings.Contains(err.Error(), "No space left on device") || strings.Contains(err.Error(), "Wrote only")) {
		t.Fatalf("oversized initialization accepted: workspace=%v err=%v", w, err)
	}
	// Inspect names instead of using Recover, which could touch a running service.
	hash := sha256.Sum256([]byte(id))
	prefix := "aiwf-ws-" + hex.EncodeToString(hash[:8]) + "-"
	volumes, err := m.dockerOutput(ctx, "volume", "ls", "--format", "{{.Name}}", "--filter", "label="+managedLabel+"="+managedValue)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(volumes), prefix) {
		t.Fatal("initialization volume leaked")
	}
	containers, err := m.dockerOutput(ctx, "ps", "-a", "--format", "{{.Names}}", "--filter", "label="+managedLabel+"="+managedValue)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(containers), prefix) {
		t.Fatal("initialization keeper leaked")
	}
}

func TestWorkspaceContainerRejectsReplacedFilesystemDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, w := quotaWorkspace(t, ctx)
	// The keeper itself is still healthy. Simulate the filesystem identity
	// changing between the supervisor's check and a container mounting it.
	w.filesystemID = "not-the-original-filesystem"
	r, err := m.Run(ctx, w, []string{"/bin/sh", "-c", "echo command-was-executed"}, 10*time.Second)
	if err == nil && (r.ExitCode == 0 || strings.Contains(r.Stdout, "command-was-executed")) {
		t.Fatalf("worker executed on the wrong filesystem: %+v", r)
	}
	if err := m.WriteFile(ctx, w, "new.txt", File{Data: []byte("not allowed")}); err == nil {
		t.Fatal("write helper accepted the wrong filesystem")
	}
	if _, err := m.CollectChanges(ctx, w); err == nil {
		t.Fatal("archive helper accepted the wrong filesystem")
	}
}

func TestWorkspaceCommandTimeoutPreservesBoundedStorageDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, w := quotaWorkspace(t, ctx)
	r, err := m.Run(ctx, w, []string{"/bin/sh", "-c", "printf checkpoint > checkpoint.txt; sleep 30"}, 2*time.Second)
	if err != nil || !r.TimedOut || r.ExitCode != 124 {
		t.Fatalf("command timeout: result=%+v err=%v", r, err)
	}
	if got, err := m.ReadFile(ctx, w, "checkpoint.txt"); err != nil || string(got) != "checkpoint" {
		t.Fatalf("timeout lost workspace state: %q %v", got, err)
	}
	if r := quotaRun(t, ctx, m, w, "test \"$(cat checkpoint.txt)\" = checkpoint"); r.ExitCode != 0 {
		t.Fatalf("workspace was unusable after timeout: %+v", r)
	}
}
