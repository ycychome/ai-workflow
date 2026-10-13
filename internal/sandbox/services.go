package sandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CreateWithServices provisions trusted repository-selected dependencies. The
// model cannot choose images, credentials, mounts, network, or service options.
func (m *Manager) CreateWithServices(ctx context.Context, runID string, files map[string]File, services []string) (*Workspace, error) {
	postgres := false
	for _, service := range services {
		if service != "postgres" {
			return nil, fmt.Errorf("unsupported verification service %q", service)
		}
		postgres = true
	}
	w, err := m.Create(ctx, runID, files)
	if err != nil {
		return nil, err
	}
	if !postgres {
		return w, nil
	}
	// Destroy waits for setup too; no late-start sidecar may escape cancellation.
	w.lifecycleMu.RLock()
	err = m.createPostgres(ctx, w)
	w.lifecycleMu.RUnlock()
	if err != nil {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return nil, errors.Join(err, m.Destroy(cleanCtx, w))
	}
	return w, nil
}

func runHash(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) createPostgres(ctx context.Context, w *Workspace) error {
	w.socketVolume = w.volume + "-pg-socket"
	w.postgresContainer = w.volume + "-postgres"
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	password := hex.EncodeToString(nonce[:])
	w.databaseURL = "host=/run/aiwf-postgres port=5432 dbname=attempt user=attempt password=" + password + " sslmode=disable"
	if _, err := m.dockerOutput(ctx, "volume", "create", "--driver", "local", "--opt", "type=tmpfs", "--opt", "device=tmpfs",
		"--opt", "o=size=1048576,nr_inodes=128,nosuid,nodev,noexec,uid=999,gid=999,mode=0755",
		"--label", managedLabel+"="+managedValue, "--label", workspaceLabel+"="+workspaceHashForVolume(w.volume), "--label", runLabel+"="+runHash(w.ID), w.socketVolume); err != nil {
		return fmt.Errorf("create private postgres socket volume: %w", err)
	}
	args := []string{"run", "--detach", "--name", w.postgresContainer, "--restart", "no", "--pull", "never",
		"--label", managedLabel + "=" + managedValue, "--label", workspaceLabel + "=" + workspaceHashForVolume(w.volume), "--label", runLabel + "=" + runHash(w.ID),
		"--network", "none", "--user", "999:999", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--pids-limit", "64", "--cpus", "1.000", "--memory", "768m", "--memory-swap", "768m", "--shm-size", "32m",
		"--log-driver", "json-file", "--log-opt", "max-size=5m", "--log-opt", "max-file=2",
		"--tmpfs", "/var/lib/postgresql/data:rw,nosuid,nodev,noexec,size=512m,nr_inodes=32768,uid=999,gid=999,mode=0700",
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=64m,nr_inodes=4096,mode=1777",
		"--mount", "type=volume,src=" + w.socketVolume + ",dst=/var/run/postgresql,volume-nocopy",
		"--env", "POSTGRES_USER=attempt", "--env", "POSTGRES_DB=attempt", "--env", "POSTGRES_PASSWORD=" + password,
		"--env", "PGDATA=/var/lib/postgresql/data/pgdata",
		m.postgresImage, "postgres", "-c", "listen_addresses=", "-c", "unix_socket_directories=/var/run/postgresql",
		"-c", "unix_socket_permissions=0777", "-c", "shared_buffers=64MB", "-c", "max_connections=20",
		"-c", "temp_file_limit=65536", "-c", "work_mem=4MB", "-c", "maintenance_work_mem=32MB",
		"-c", "logging_collector=off", "-c", "log_statement=none"}
	out, err := m.dockerOutput(ctx, args...)
	if err != nil {
		return fmt.Errorf("start private postgres: %w", err)
	}
	w.postgresID = strings.TrimSpace(string(out))
	if w.postgresID == "" {
		return errors.New("postgres returned no container identity")
	}
	readyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err = m.checkKeeper(readyCtx, w); err != nil {
			return err
		}
		if err = m.checkPostgres(readyCtx, w); err != nil {
			return err
		}
		if _, err = m.dockerOutput(readyCtx, "exec", w.postgresID, "/bin/sh", "-ceu", `test "$(cat /proc/1/comm)" = postgres; exec pg_isready -h /var/run/postgresql -U attempt -d attempt`); err == nil {
			return m.verifyPostgresStorage(readyCtx, w)
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("postgres readiness: %w", readyCtx.Err())
		case <-ticker.C:
		}
	}
}

// The PostgreSQL incarnation is immutable, like the workspace keeper. A
// restart must never silently substitute a fresh empty database mid-attempt.
func (m *Manager) checkPostgres(ctx context.Context, w *Workspace) error {
	if w.postgresContainer == "" {
		return nil
	}
	out, err := m.dockerOutput(ctx, "inspect", "--type", "container", w.postgresContainer)
	if err != nil {
		return fmt.Errorf("check private postgres: %w", err)
	}
	var inspected []keeperInspection
	if json.Unmarshal(out, &inspected) != nil || len(inspected) != 1 {
		return errors.New("private postgres identity could not be verified")
	}
	p := inspected[0]
	if p.ID != w.postgresID || !p.State.Running || p.State.Paused || p.State.Restarting || p.RestartCount != 0 || p.State.StartedAt == "" || (w.postgresStartedAt != "" && p.State.StartedAt != w.postgresStartedAt) {
		return errors.New("private postgres stopped or restarted; create a new attempt")
	}
	if w.postgresStartedAt == "" {
		w.postgresStartedAt = p.State.StartedAt
	}
	return nil
}

// StopRun is for a fenced, expired AgentRun. The caller must revoke ownership
// before invoking it; it must not use a workflow ID shared by live sessions.
func (m *Manager) StopRun(ctx context.Context, runID string) error {
	return m.RecoverWorkspace(ctx, runID)
}

// RecoverWorkspace removes only resources labelled with this exact run ID.
// Successful completion is positive confirmation that no labelled containers
// or volumes remain, suitable for releasing the corresponding capacity slot.
func (m *Manager) RecoverWorkspace(ctx context.Context, runID string) error {
	if m == nil || strings.TrimSpace(runID) == "" {
		return errors.New("recovery requires a nonempty run ID")
	}
	// Stop local operations first, including helpers that might otherwise race
	// recovery by creating a container after the first Docker listing.
	m.mu.Lock()
	local := []*Workspace{}
	for w := range m.workspaces {
		if w.ID == runID {
			local = append(local, w)
		}
	}
	m.mu.Unlock()
	for _, w := range local {
		if err := m.Destroy(ctx, w); err != nil {
			return err
		}
	}
	filter := "label=" + runLabel + "=" + runHash(runID)
	containers, err := m.dockerOutput(ctx, "ps", "-aq", "--filter", "label="+managedLabel+"="+managedValue, "--filter", filter)
	if err != nil {
		return fmt.Errorf("list run containers: %w", err)
	}
	for _, id := range strings.Fields(string(containers)) {
		if err = m.removeContainerConfirmed(ctx, id); err != nil {
			return err
		}
	}
	volumes, err := m.dockerOutput(ctx, "volume", "ls", "-q", "--filter", "label="+managedLabel+"="+managedValue, "--filter", filter)
	if err != nil {
		return fmt.Errorf("list run volumes: %w", err)
	}
	for _, volume := range strings.Fields(string(volumes)) {
		if err = m.removeVolume(ctx, volume); err != nil {
			return err
		}
	}
	return m.confirmRunAbsent(ctx, filter)
}

func (m *Manager) confirmRunAbsent(ctx context.Context, filter string) error {
	for _, args := range [][]string{{"ps", "-aq", "--filter", "label=" + managedLabel + "=" + managedValue, "--filter", filter}, {"volume", "ls", "-q", "--filter", "label=" + managedLabel + "=" + managedValue, "--filter", filter}} {
		out, err := m.dockerOutput(ctx, args...)
		if err != nil {
			return fmt.Errorf("confirm run cleanup: %w", err)
		}
		if strings.TrimSpace(string(out)) != "" {
			return errors.New("run resources remain after cleanup")
		}
	}
	return nil
}

func (m *Manager) removeContainerConfirmed(ctx context.Context, name string) error {
	_, removeErr := m.dockerOutput(ctx, "rm", "-f", name)
	// Even after a CLI error, accept absence only if the daemon answers a fresh
	// successful listing. Ambiguous connectivity must retain the capacity slot.
	out, err := m.dockerOutput(ctx, "ps", "-a", "--no-trunc", "--format", "{{.ID}} {{.Names}}")
	if err != nil {
		return errors.Join(removeErr, fmt.Errorf("confirm container removal: %w", err))
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && (fields[0] == name || fields[1] == name) {
			return errors.Join(removeErr, errors.New("container remains after removal"))
		}
	}
	return nil
}

// Verify the mounted backend rather than trusting local-volume driver options.
func (m *Manager) verifyPostgresStorage(ctx context.Context, w *Workspace) error {
	for _, limit := range []struct {
		path          string
		bytes, inodes int64
	}{{"/var/run/postgresql", 1 << 20, 128}, {"/var/lib/postgresql/data", 512 << 20, 32768}, {"/tmp", 64 << 20, 4096}} {
		out, err := m.dockerOutput(ctx, "exec", w.postgresID, "stat", "-f", "-c", "%T %S %b %c", limit.path)
		if err != nil {
			return err
		}
		fields := strings.Fields(string(out))
		if len(fields) != 4 || fields[0] != "tmpfs" {
			return errors.New("postgres storage is not verified tmpfs")
		}
		blockSize, e1 := strconv.ParseInt(fields[1], 10, 64)
		blocks, e2 := strconv.ParseInt(fields[2], 10, 64)
		inodes, e3 := strconv.ParseInt(fields[3], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || blockSize <= 0 || blocks <= 0 || blocks > limit.bytes/blockSize || inodes <= 0 || inodes > limit.inodes {
			return errors.New("postgres storage lacks requested byte or inode bounds")
		}
	}
	return nil
}
