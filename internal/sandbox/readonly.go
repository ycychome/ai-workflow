package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SealReadOnly permanently removes command and helper write access to the
// candidate. Trusted initialization (including hidden tests) must precede it.
// Optional review tests live in a separate tmpfs capped at min(64 MiB, the
// configured workspace limit), additional to the candidate's existing quota.
// A candidate-owned review_tests path is rejected rather than hidden by a mount.
func (m *Manager) SealReadOnly(ctx context.Context, w *Workspace, allowReviewTests bool) error {
	if err := m.checkWorkspace(w); err != nil {
		return err
	}
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	if err := m.checkKeeper(ctx, w); err != nil {
		return err
	}
	if w.readOnly {
		if (w.reviewScratch != nil) != allowReviewTests {
			return errors.New("sealed workspace mode cannot change")
		}
		return nil
	}
	if allowReviewTests {
		// Inspect actual storage too: trusted initialization may have written files
		// since the initial baseline was recorded. A symlink is also a collision.
		if _, err := m.helperAs(ctx, w, nil, workerUID, "/bin/sh", "-ceu", `test ! -e /workspace/review_tests && test ! -L /workspace/review_tests`); err != nil {
			return errors.New("candidate owns reserved review_tests path; refusing to hide it")
		}
		scratchBytes := m.workspaceBytes
		if scratchBytes > 64<<20 {
			scratchBytes = 64 << 20
		}
		scratchManager, err := NewManager(Options{DockerBinary: m.docker, RunnerImage: m.image, PostgresImage: m.postgresImage, CPUQuota: m.cpus, MemoryBytes: m.memory, PidsLimit: m.pids, WorkspaceBytes: scratchBytes})
		if err != nil {
			return err
		}
		scratch, err := scratchManager.Create(ctx, w.ID, map[string]File{})
		if err != nil {
			return fmt.Errorf("create bounded review scratch: %w", err)
		}
		if _, err = m.helperAs(ctx, w, nil, workerUID, "/bin/mkdir", "/workspace/review_tests"); err != nil {
			cleanupErr := scratchManager.Destroy(context.WithoutCancel(ctx), scratch)
			return errors.Join(err, cleanupErr)
		}
		w.reviewScratch = scratch
	}
	w.readOnly = true
	return nil
}

func workspaceMountArgs(w *Workspace, sourceReadOnly, scratchReadOnly bool) []string {
	mount := "type=volume,src=" + w.volume + ",dst=/workspace,volume-nocopy"
	if sourceReadOnly {
		mount += ",readonly"
	}
	args := []string{"--mount", mount}
	if w.reviewScratch != nil {
		mount = "type=volume,src=" + w.reviewScratch.volume + ",dst=/workspace/review_tests,volume-nocopy"
		if scratchReadOnly {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
	}
	return append(args, judgeMountArgs(w)...)
}

func sealWorkerMounts(args []string, w *Workspace) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--mount" && strings.Contains(args[i+1], "dst=/workspace,") {
			args = append(args[:i], args[i+2:]...)
			break
		}
	}
	return append(args, workspaceMountArgs(w, w.readOnly, false)...)
}
