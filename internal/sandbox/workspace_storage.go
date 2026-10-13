package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A Docker local tmpfs volume is unmounted when its last container releases
// it. The keeper keeps that mount alive across disposable tool containers.
// Merely recreating a container after the keeper dies would silently create
// an empty filesystem, so both the keeper incarnation and filesystem ID are
// bound to the Workspace and checked before executing any command.
func (m *Manager) createWorkspaceStorage(ctx context.Context, w *Workspace) error {
	options := fmt.Sprintf("size=%d,nr_inodes=%d,nosuid,nodev,mode=0755", m.workspaceBytes, m.workspaceInodes)
	if _, err := m.dockerOutput(ctx, "volume", "create", "--driver", "local",
		"--opt", "type=tmpfs", "--opt", "device=tmpfs", "--opt", "o="+options,
		"--label", managedLabel+"="+managedValue,
		"--label", workspaceLabel+"="+workspaceHashForVolume(w.volume), w.volume); err != nil {
		return fmt.Errorf("create bounded workspace volume: %w", err)
	}
	args := []string{"run", "--detach", "--name", w.keeper, "--restart", "no",
		"--label", managedLabel + "=" + managedValue,
		"--label", workspaceLabel + "=" + workspaceHashForVolume(w.volume),
		"--network", "none", "--user", workerUID, "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true", "--pids-limit", "16", "--cpus", "0.050",
		"--memory", "16m", "--memory-swap", "16m", "--log-driver", "none",
		"--mount", "type=volume,src=" + w.volume + ",dst=/workspace,volume-nocopy",
		"--entrypoint", "/bin/sleep", m.image, "infinity"}
	out, err := m.dockerOutput(ctx, args...)
	if err != nil {
		return fmt.Errorf("start workspace keeper: %w", err)
	}
	w.keeperID = strings.TrimSpace(string(out))
	if w.keeperID == "" {
		return errors.New("workspace keeper returned no container ID")
	}
	if err := m.checkKeeper(ctx, w); err != nil {
		return err
	}
	// statfs reads the mounted filesystem, rather than trusting Docker's
	// requested driver options. Unsupported options/backends must fail closed.
	out, err = m.dockerOutput(ctx, "exec", w.keeperID, "/usr/bin/stat", "-f", "-c", "%T %S %b %c %i", "/workspace")
	if err != nil {
		return fmt.Errorf("verify workspace filesystem: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 5 || fields[0] != "tmpfs" {
		return errors.New("workspace storage is not a verified tmpfs filesystem")
	}
	blockSize, blockErr := strconv.ParseInt(fields[1], 10, 64)
	blocks, blocksErr := strconv.ParseInt(fields[2], 10, 64)
	inodes, inodesErr := strconv.ParseInt(fields[3], 10, 64)
	if blockErr != nil || blocksErr != nil || inodesErr != nil || blockSize <= 0 || blocks <= 0 ||
		blocks > m.workspaceBytes/blockSize || inodes <= 0 || inodes > m.workspaceInodes {
		return errors.New("workspace filesystem lacks the requested hard byte or inode limit")
	}
	if fields[4] == "" || strings.Trim(fields[4], "0") == "" {
		return errors.New("workspace filesystem returned no stable identity")
	}
	w.filesystemID = fields[4]
	return m.checkKeeper(ctx, w)
}

type keeperInspection struct {
	ID           string `json:"Id"`
	RestartCount int
	State        struct {
		Running    bool
		Paused     bool
		Restarting bool
		StartedAt  string
	}
}

func (m *Manager) checkKeeper(ctx context.Context, w *Workspace) error {
	w.mu.Lock()
	invalid := w.invalid
	w.mu.Unlock()
	if invalid != "" {
		return errors.New(invalid)
	}
	out, err := m.dockerOutput(ctx, "inspect", "--type", "container", w.keeper)
	if err != nil {
		if ctx.Err() == nil {
			w.invalidate("workspace keeper is unavailable; create a new workspace")
		}
		return fmt.Errorf("check workspace keeper: %w", err)
	}
	var inspected []keeperInspection
	if err := json.Unmarshal(out, &inspected); err != nil || len(inspected) != 1 {
		w.invalidate("workspace keeper identity could not be verified")
		return errors.New("workspace keeper identity could not be verified")
	}
	k := inspected[0]
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.invalid != "" {
		return errors.New(w.invalid)
	}
	if k.ID != w.keeperID || !k.State.Running || k.State.Paused || k.State.Restarting || k.RestartCount != 0 ||
		k.State.StartedAt == "" || (w.keeperStartedAt != "" && k.State.StartedAt != w.keeperStartedAt) {
		w.invalid = "workspace keeper stopped or restarted; create a new workspace"
		return errors.New(w.invalid)
	}
	if w.keeperStartedAt == "" {
		w.keeperStartedAt = k.State.StartedAt
	}
	return nil
}

func (w *Workspace) invalidate(reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.invalid == "" {
		w.invalid = reason
	}
}

func (m *Manager) beginWorkspaceOperation(ctx context.Context, w *Workspace) (func(), error) {
	if err := m.checkWorkspace(w); err != nil {
		return nil, err
	}
	w.lifecycleMu.RLock()
	if err := m.checkWorkspace(w); err != nil {
		w.lifecycleMu.RUnlock()
		return nil, err
	}
	if err := m.checkKeeper(ctx, w); err != nil {
		w.lifecycleMu.RUnlock()
		return nil, err
	}
	return w.lifecycleMu.RUnlock, nil
}

// Arguments are passed as shell positional parameters, never interpolated.
// The guard closes the check/start race if Docker remounts an empty tmpfs
// after keeper loss. Project code cannot change a filesystem's kernel ID.
func workspaceCommand(w *Workspace, command []string) []string {
	const guard = `expected=$1; shift; actual=$(/usr/bin/stat -f -c %i /workspace); if [ "$actual" != "$expected" ]; then echo 'workspace filesystem changed; refusing to execute' >&2; exit 125; fi; exec "$@"`
	args := []string{"/bin/sh", "-ceu", guard, "workspace-guard", w.filesystemID}
	return append(args, command...)
}

// Cleanup only addresses this workspace's unguessable label. This also reaps
// a helper whose attached Docker CLI was interrupted before Docker removed it.
func (m *Manager) removeWorkspaceStorage(ctx context.Context, w *Workspace) error {
	containersErr := m.removeWorkspaceContainers(ctx, w)
	return errors.Join(containersErr, m.removeVolume(ctx, w.volume))
}

func (m *Manager) removeWorkspaceContainers(ctx context.Context, w *Workspace) error {
	out, err := m.dockerOutput(ctx, "ps", "-aq", "--filter", "label="+workspaceLabel+"="+workspaceHashForVolume(w.volume))
	if err != nil {
		return fmt.Errorf("list workspace containers for cleanup: %w", err)
	}
	var cleanupErrors []error
	for _, id := range strings.Fields(string(out)) {
		if _, err := m.dockerOutput(ctx, "rm", "-f", id); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove workspace container: %w", err))
		}
	}
	return errors.Join(cleanupErrors...)
}
