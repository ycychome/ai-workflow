// Package sandbox manages isolated, disposable workspaces for untrusted code.
// It talks to Docker from the trusted control process; worker containers never
// receive the Docker socket, host paths, or the control process environment.
package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	managedLabel          = "ai-workflow.managed"
	workspaceLabel        = "ai-workflow.workspace"
	runLabel              = "ai-workflow.run"
	managedValue          = "1"
	workerUID             = "10001:10001"
	defaultImage          = "ai-workflow-runner:local"
	defaultCPUs           = 2.0
	defaultMemory         = int64(2 << 30)
	defaultPids           = 256
	defaultWorkspaceBytes = int64(512 << 20)
	maxCommandBytes       = 1 << 20
	maxFileBytes          = 64 << 20
	maxArchiveBytes       = 256 << 20
)

// File is a regular file in a source snapshot. Mode is normalized to Git's
// 100644 or 100755 semantics; special files and symlinks are not representable.
type File struct {
	Data []byte
	Mode fs.FileMode
}

// Options configures resource limits for the disposable worker containers.
// DockerBinary and RunnerImage default to "docker" and
// "ai-workflow-runner:local". A network mode is intentionally not configurable:
// workers run with Docker's isolated "none" network.
type Options struct {
	DockerBinary   string
	PostgresImage  string
	RunnerImage    string
	CPUQuota       float64
	MemoryBytes    int64
	PidsLimit      int
	WorkspaceBytes int64
}

// Manager owns the Docker volumes and containers it creates.
type Manager struct {
	docker          string
	image           string
	postgresImage   string
	cpus            float64
	memory          int64
	pids            int
	workspaceBytes  int64
	workspaceInodes int64

	mu         sync.Mutex
	workspaces map[*Workspace]struct{}
}

// Workspace is an opaque handle. Its volume is private to its Manager and is
// never a host path. A workspace is single-run-at-a-time.
type Workspace struct {
	ID             string
	readOnly       bool
	reviewScratch  *Workspace
	judgePeer      *Workspace
	judgeOwner     *Workspace
	judgeTransport *Workspace
	sharedServices bool

	manager           *Manager
	volume            string
	keeper            string
	keeperID          string
	keeperStartedAt   string
	filesystemID      string
	lifecycleMu       sync.RWMutex
	invalid           string
	mu                sync.Mutex
	runMu             sync.Mutex
	running           string
	baseline          map[string]snapshotMeta
	socketVolume      string
	postgresContainer string
	postgresID        string
	postgresStartedAt string
	databaseURL       string
}

// RunResult contains bounded stdout/stderr captured after the container exits.
type RunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
}

// Entry describes a path observed in the workspace snapshot.
type Entry struct {
	Path string
	Mode fs.FileMode
	Size int64
}

// Match describes a text search result.
type Match struct {
	Path string
	Line int
	Text string
}

// Change is the final regular-file state for one changed path. Deleted files
// have Deleted set and no Data. Git metadata itself is never returned.
type Change struct {
	Path    string
	Status  string
	Mode    fs.FileMode
	Data    []byte
	Deleted bool
}

// NewManager constructs a Docker-backed supervisor. It does not start services
// or create containers until Create or another operation is called.
func NewManager(options Options) (*Manager, error) {
	if options.DockerBinary == "" {
		options.DockerBinary = "docker"
	}
	if options.PostgresImage == "" {
		options.PostgresImage = "postgres:16-bookworm"
	}
	if options.RunnerImage == "" {
		options.RunnerImage = defaultImage
	}
	if options.CPUQuota == 0 {
		options.CPUQuota = defaultCPUs
	}
	if options.MemoryBytes == 0 {
		options.MemoryBytes = defaultMemory
	}
	if options.PidsLimit == 0 {
		options.PidsLimit = defaultPids
	}
	if options.WorkspaceBytes == 0 {
		options.WorkspaceBytes = defaultWorkspaceBytes
	}
	if options.CPUQuota <= 0 || options.MemoryBytes <= 0 || options.PidsLimit <= 0 || options.WorkspaceBytes <= 0 {
		return nil, errors.New("sandbox resource limits must be positive")
	}
	if strings.TrimSpace(options.RunnerImage) == "" {
		return nil, errors.New("runner image must not be empty")
	}
	return &Manager{
		docker:          options.DockerBinary,
		image:           options.RunnerImage,
		postgresImage:   options.PostgresImage,
		cpus:            options.CPUQuota,
		memory:          options.MemoryBytes,
		pids:            options.PidsLimit,
		workspaceBytes:  options.WorkspaceBytes,
		workspaceInodes: max(int64(1024), options.WorkspaceBytes/16384),
		workspaces:      make(map[*Workspace]struct{}),
	}, nil
}

// Create makes a fresh named volume, copies the provided snapshot into it,
// then initializes a private Git baseline with hooks/templates and global
// configuration disabled. Input paths are relative slash-separated names.
func (m *Manager) Create(ctx context.Context, id string, files map[string]File) (*Workspace, error) {
	if m == nil {
		return nil, errors.New("nil sandbox manager")
	}
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("workspace id must not be empty")
	}
	archive, err := snapshotTar(files)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("generate workspace name: %w", err)
	}
	idHash := sha256.Sum256([]byte(id))
	volume := "aiwf-ws-" + hex.EncodeToString(idHash[:8]) + "-" + hex.EncodeToString(nonce[:])
	w := &Workspace{ID: id, manager: m, volume: volume, keeper: volume + "-keeper"}
	cleanup := func(cause error) error {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return errors.Join(cause, m.removeWorkspaceStorage(cleanCtx, w))
	}
	if err := m.createWorkspaceStorage(ctx, w); err != nil {
		return nil, cleanup(err)
	}
	if err := m.initialize(ctx, w, archive); err != nil {
		return nil, cleanup(err)
	}
	if err := m.checkKeeper(ctx, w); err != nil {
		return nil, cleanup(err)
	}
	w.baseline = make(map[string]snapshotMeta, len(files))
	for name, file := range files {
		clean, _ := cleanRelativePath(name)
		mode, _ := NormalizeGitFileMode(file.Mode) // snapshotTar validated this mode.
		w.baseline[clean] = snapshotMeta{hash: sha256.Sum256(file.Data), mode: mode}
	}
	m.mu.Lock()
	m.workspaces[w] = struct{}{}
	m.mu.Unlock()
	return w, nil
}

// Recover refuses unscoped cleanup: another controller may own live resources.
// Use RecoverWorkspace with an expired, fenced AgentRun ID instead.
func (m *Manager) Recover(ctx context.Context) error {
	return errors.New("unscoped recovery is disabled; use RecoverWorkspace with a fenced run ID")
}

// Run executes one argument-vector command with no shell interpolation. A
// timeout or caller cancellation stops and removes the worker container.
func (m *Manager) Run(ctx context.Context, w *Workspace, args []string, timeout time.Duration) (result RunResult, runErr error) {
	finish, err := m.beginWorkspaceOperation(ctx, w)
	if err != nil {
		return RunResult{}, err
	}
	defer finish()
	if len(args) == 0 || args[0] == "" {
		return RunResult{}, errors.New("command must not be empty")
	}
	if timeout <= 0 {
		return RunResult{}, errors.New("timeout must be positive")
	}
	for _, arg := range args {
		if len(arg) > maxCommandBytes {
			return RunResult{}, errors.New("command argument exceeds size limit")
		}
	}
	w.runMu.Lock()
	defer w.runMu.Unlock()
	if err := m.checkKeeper(ctx, w); err != nil {
		return RunResult{}, err
	}
	name, err := randomName(w.ID, "run")
	if err != nil {
		return RunResult{}, err
	}
	if err := m.checkPostgres(ctx, w); err != nil {
		return RunResult{}, err
	}
	argsDocker := sealWorkerMounts(m.workerArgs(name, w.volume), w)
	argsDocker = append(argsDocker, "--label", runLabel+"="+runHash(w.ID))
	if w.socketVolume != "" {
		argsDocker = append(argsDocker, "--mount", "type=volume,src="+w.socketVolume+",dst=/run/aiwf-postgres,readonly,volume-nocopy", "--env", "DATABASE_URL="+w.databaseURL, "--env", "TEST_DATABASE_URL="+w.databaseURL, "--env", "AGENT_QUEUE_TEST_DATABASE_URL="+w.databaseURL)
	}
	argsDocker = append(argsDocker, "--entrypoint", "/bin/sh", m.image)
	command := append([]string{"timeout", "--signal=TERM", "--kill-after=2", timeoutSeconds(timeout)}, args...)
	argsDocker = append(argsDocker, workspaceCommand(w, command)[1:]...)
	w.mu.Lock()
	w.running = name
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = ""
		w.mu.Unlock()
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.removeContainerConfirmed(cleanCtx, name); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := m.dockerOutput(runCtx, argsDocker...); err != nil {
		if ctx.Err() != nil {
			return m.collectInterruptedRun(name, ctx.Err(), false)
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return m.collectInterruptedRun(name, nil, true)
		}
		return RunResult{}, fmt.Errorf("start worker: %w", err)
	}
	// Destroy may have revoked the workspace while Docker was starting the
	// container. In that race Cancel cannot yet find it, so stop here and let
	// the deferred forced removal reap it instead of waiting for its command.
	if err := m.checkKeeper(runCtx, w); err != nil {
		if ctx.Err() != nil {
			return m.collectInterruptedRun(name, ctx.Err(), false)
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return m.collectInterruptedRun(name, nil, true)
		}
		return RunResult{}, err
	}
	waitOut, waitErr := m.dockerOutput(runCtx, "wait", name)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	if waitErr != nil {
		if runCtx.Err() != nil {
			if ctx.Err() != nil {
				return m.collectInterruptedRun(name, ctx.Err(), false)
			}
			return m.collectInterruptedRun(name, runCtx.Err(), timedOut)
		} else {
			return RunResult{}, fmt.Errorf("wait for worker: %w", waitErr)
		}
	}
	logCtx, logCancel := context.WithTimeout(context.Background(), 5*time.Second)
	logsOut, logsErr, logsStderr := m.dockerOutputStreams(logCtx, "logs", name)
	logCancel()
	if logsErr != nil && !timedOut {
		return RunResult{}, fmt.Errorf("collect worker output: %w", logsErr)
	}
	code := 0
	if timedOut {
		code = 124
	} else {
		code, err = parseExitCode(waitOut)
		if err != nil {
			return RunResult{}, err
		}
	}
	result = RunResult{ExitCode: code, Stdout: string(logsOut), Stderr: string(logsStderr), TimedOut: timedOut}
	if err := m.checkKeeper(ctx, w); err != nil {
		return result, err
	}
	if err := m.checkPostgres(ctx, w); err != nil {
		return result, err
	}
	return result, nil
}

// ReadFile reads a regular file from the workspace. Symlinks are rejected.
func (m *Manager) ReadFile(ctx context.Context, w *Workspace, filePath string) ([]byte, error) {
	finish, err := m.beginWorkspaceOperation(ctx, w)
	if err != nil {
		return nil, err
	}
	defer finish()
	clean, err := cleanRelativePath(filePath)
	if err != nil {
		return nil, err
	}
	if hasGitComponent(clean) {
		return nil, errors.New("Git metadata is not part of the source workspace")
	}
	var out bytes.Buffer
	if err := m.archiveCommand(ctx, w, &out, workspaceTarArgs(clean)...); err != nil {
		return nil, err
	}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read workspace archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("workspace path %q is not a regular file", clean)
		}
		if h.Size > 64<<20 {
			return nil, fmt.Errorf("workspace file %q exceeds the 64 MiB read limit", clean)
		}
		return io.ReadAll(io.LimitReader(tr, 64<<20))
	}
	return nil, fs.ErrNotExist
}

// WriteFile atomically replaces a regular file through an isolated helper
// container. It never exposes the volume as a host filesystem path.
func (m *Manager) WriteFile(ctx context.Context, w *Workspace, filePath string, file File) error {
	finish, err := m.beginWorkspaceOperation(ctx, w)
	if err != nil {
		return err
	}
	defer finish()
	clean, err := cleanRelativePath(filePath)
	if err != nil {
		return err
	}
	if hasGitComponent(clean) {
		return errors.New("Git metadata is not part of the source workspace")
	}
	if w.readOnly && (w.reviewScratch == nil || !strings.HasPrefix(clean, "review_tests/")) {
		return errors.New("candidate is sealed read-only")
	}
	if len(file.Data) > maxFileBytes {
		return errors.New("workspace file exceeds the 64 MiB write limit")
	}
	archive, err := snapshotTar(map[string]File{clean: file})
	if err != nil {
		return err
	}
	return m.initializeFiles(ctx, w, archive)
}

// List returns files below path ("." for the full workspace). Git internals
// are excluded so consumers only see source snapshot paths.
func (m *Manager) List(ctx context.Context, w *Workspace, dir string) ([]Entry, error) {
	finish, err := m.beginWorkspaceOperation(ctx, w)
	if err != nil {
		return nil, err
	}
	defer finish()
	clean := "."
	if dir != "" && dir != "." {
		var err error
		clean, err = cleanRelativePath(dir)
		if err != nil {
			return nil, err
		}
		if hasGitComponent(clean) {
			return nil, errors.New("Git metadata is not part of the source workspace")
		}
	}
	out, err := m.archiveOutput(ctx, w, workspaceTarArgs(clean)...)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(bytes.NewReader(out))
	entries := make([]Entry, 0)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		p := strings.TrimPrefix(path.Clean(h.Name), "./")
		if p == "." || hasGitComponent(p) {
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA && h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeSymlink {
			continue
		}
		entries = append(entries, Entry{Path: p, Mode: fs.FileMode(h.Mode), Size: h.Size})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// Search performs fixed-string recursive text search. Empty queries are
// rejected, and the Git metadata directory is excluded.
func (m *Manager) Search(ctx context.Context, w *Workspace, query string) ([]Match, error) {
	finish, err := m.beginWorkspaceOperation(ctx, w)
	if err != nil {
		return nil, err
	}
	defer finish()
	if query == "" || strings.ContainsAny(query, "\x00\r\n") {
		return nil, errors.New("search query must not be empty")
	}
	out, err := m.archiveOutput(ctx, w, workspaceTarArgs(".")...)
	if err != nil {
		return nil, err
	}
	matches := make([]Match, 0)
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read workspace snapshot: %w", err)
		}
		p := strings.TrimPrefix(path.Clean(h.Name), "./")
		if p == "." || hasGitComponent(p) || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA) {
			continue
		}
		if strings.ContainsAny(p, "\x00\r\n") {
			continue
		}
		if h.Size > maxFileBytes {
			return nil, fmt.Errorf("workspace file %q exceeds the 64 MiB search limit", p)
		}
		data, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		for lineNo, line := range bytes.Split(data, []byte{'\n'}) {
			if bytes.Contains(line, []byte(query)) {
				matches = append(matches, Match{Path: p, Line: lineNo + 1, Text: string(line)})
			}
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Path != matches[j].Path {
			return matches[i].Path < matches[j].Path
		}
		return matches[i].Line < matches[j].Line
	})
	return matches, nil
}

// CollectChanges compares the workspace to its private baseline and returns
// all changed regular files, including untracked files and deletions.
func (m *Manager) CollectChanges(ctx context.Context, w *Workspace) ([]Change, error) {
	finish, err := m.beginWorkspaceOperation(ctx, w)
	if err != nil {
		return nil, err
	}
	defer finish()
	out, err := m.archiveOutput(ctx, w, workspaceTarArgs(".")...)
	if err != nil {
		return nil, err
	}
	current := make(map[string]Change)
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read workspace snapshot: %w", err)
		}
		p := strings.TrimPrefix(path.Clean(h.Name), "./")
		if p == "." || hasGitComponent(p) {
			continue
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("cannot export non-regular changed path %q", p)
		}
		clean, err := cleanRelativePath(p)
		if err != nil {
			return nil, err
		}
		if h.Size > maxFileBytes {
			return nil, fmt.Errorf("workspace file %q exceeds the 64 MiB collection limit", p)
		}
		data, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			return nil, err
		}
		mode, err := NormalizeGitFileMode(fs.FileMode(h.Mode))
		if err != nil {
			return nil, fmt.Errorf("cannot export changed path %q with unsupported mode: %w", clean, err)
		}
		current[clean] = Change{Path: clean, Mode: mode, Data: data}
	}
	changes := make([]Change, 0)
	for name, base := range w.baseline {
		if now, ok := current[name]; !ok {
			changes = append(changes, Change{Path: name, Status: "D", Deleted: true})
		} else if base.hash != sha256.Sum256(now.Data) || base.mode != now.Mode {
			now.Status = "M"
			changes = append(changes, now)
		}
	}
	for name, now := range current {
		if _, ok := w.baseline[name]; !ok {
			now.Status = "A"
			changes = append(changes, now)
		}
	}
	sortChanges(changes)
	return changes, nil
}

// Cancel revokes and destroys the entire attempt, including its database.
func (m *Manager) Cancel(ctx context.Context, w *Workspace) error {
	return m.Destroy(ctx, w)
}

// Destroy stops any active worker and removes the named volume.
func (m *Manager) Destroy(ctx context.Context, w *Workspace) error {
	if m == nil || w == nil || w.manager != m {
		return errors.New("workspace does not belong to this manager")
	}
	if err := m.checkWorkspace(w); err != nil {
		return m.confirmRunAbsent(ctx, "label="+workspaceLabel+"="+workspaceHashForVolume(w.volume))
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	w.mu.Lock()
	w.invalid = "workspace is being destroyed"
	w.mu.Unlock()
	// Kill this workspace's helpers as well as its worker before waiting for
	// operations to finish. A canceled Docker CLI may have left a helper alive.
	// Late starts are rejected by the filesystem guard and post-start check.
	initialCleanupErr := m.removeWorkspaceContainers(ctx, w)
	wait := time.NewTicker(10 * time.Millisecond)
	defer wait.Stop()
	for !w.lifecycleMu.TryLock() {
		select {
		case <-ctx.Done():
			return errors.Join(initialCleanupErr, fmt.Errorf("wait for workspace operations to stop: %w", ctx.Err()))
		case <-wait.C:
		}
	}
	defer w.lifecycleMu.Unlock()
	if err := errors.Join(initialCleanupErr, m.removeWorkspaceStorage(ctx, w)); err != nil {
		return err
	}
	if err := m.confirmRunAbsent(ctx, "label="+workspaceLabel+"="+workspaceHashForVolume(w.volume)); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.workspaces, w)
	m.mu.Unlock()
	return nil
}

func (m *Manager) initialize(ctx context.Context, w *Workspace, archive []byte) error {
	if err := m.initializeFiles(ctx, w, archive); err != nil {
		return err
	}
	script := "set -eu; git init --quiet /workspace; git -C /workspace config core.hooksPath /dev/null; git -C /workspace config init.templateDir /dev/null; git -C /workspace config user.name 'AI Workflow Snapshot'; git -C /workspace config user.email 'sandbox@invalid'; git -C /workspace add --all; git -C /workspace -c commit.gpgsign=false commit --quiet --allow-empty -m snapshot"
	if _, err := m.helperAs(ctx, w, nil, workerUID, "/bin/sh", "-ceu", script); err != nil {
		return fmt.Errorf("initialize isolated git baseline: %w", err)
	}
	return nil
}

func (m *Manager) initializeFiles(ctx context.Context, w *Workspace, archive []byte) error {
	if w.readOnly {
		_, err := m.helperAs(ctx, w, bytes.NewReader(archive), workerUID, "/bin/tar", "--no-same-owner", "-xf", "-", "-C", "/workspace")
		return err
	}
	script := "set -eu; mkdir -p /workspace; tar -xf - -C /workspace; chown -R 10001:10001 /workspace"
	if _, err := m.helperAs(ctx, w, bytes.NewReader(archive), "0:0", "/bin/sh", "-ceu", script); err != nil {
		return fmt.Errorf("write workspace snapshot: %w", err)
	}
	return nil
}

func (m *Manager) workerArgs(name, volume string) []string {
	args := []string{"run", "--detach", "--name", name,
		"--label", managedLabel + "=" + managedValue,
		"--label", workspaceLabel + "=" + workspaceHashForVolume(volume),
		"--network", "none", "--user", workerUID,
		"--workdir", "/workspace", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--read-only", "--pids-limit", fmt.Sprint(m.pids), "--cpus", fmt.Sprintf("%.3f", m.cpus),
		"--memory", fmt.Sprintf("%d", m.memory), "--memory-swap", fmt.Sprintf("%d", m.memory),
		"--log-driver", "json-file", "--log-opt", "max-size=10m", "--log-opt", "max-file=2",
		"--mount", "type=volume,src=" + volume + ",dst=/workspace,volume-nocopy"}
	args = append(args, "--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=512m,mode=1777",
		"--env", "HOME=/tmp", "--env", "GIT_CONFIG_NOSYSTEM=1", "--env", "GIT_CONFIG_GLOBAL=/dev/null",
		"--env", "GIT_TERMINAL_PROMPT=0", "--env", "GOTOOLCHAIN=local", "--env", "GOPROXY=off", "--env", "GOSUMDB=off")
	return args
}

func (m *Manager) helperAs(ctx context.Context, w *Workspace, input io.Reader, user string, command ...string) ([]byte, error) {
	if err := m.checkKeeper(ctx, w); err != nil {
		return nil, err
	}
	command = workspaceCommand(w, command)
	args := []string{"run", "--rm", "--log-driver", "none"}
	if input != nil {
		args = append(args, "-i")
	}
	args = append(args, "--label", managedLabel+"="+managedValue,
		"--label", workspaceLabel+"="+workspaceHashForVolume(w.volume),
		"--label", runLabel+"="+runHash(w.ID),
		"--network", "none", "--user", user,
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--pids-limit", fmt.Sprint(m.pids), "--cpus", fmt.Sprintf("%.3f", m.cpus),
		"--memory", fmt.Sprint(m.memory), "--memory-swap", fmt.Sprint(m.memory),
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=64m,mode=1777",
		"--env", "HOME=/tmp", "--env", "GIT_CONFIG_NOSYSTEM=1", "--env", "GIT_CONFIG_GLOBAL=/dev/null")
	args = append(args, workspaceMountArgs(w, w.readOnly, false)...)
	if user == "0:0" {
		args = append(args, "--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER")
	}
	args = append(args, "--workdir", "/workspace", "--entrypoint", command[0], m.image)
	args = append(args, command[1:]...)
	out, err := m.dockerInputOutput(ctx, input, args...)
	if err != nil {
		return out, err
	}
	return out, m.checkKeeper(ctx, w)
}

func (m *Manager) archiveCommand(ctx context.Context, w *Workspace, output io.Writer, command ...string) error {
	data, err := m.archiveOutput(ctx, w, command...)
	if err != nil {
		return err
	}
	_, err = output.Write(data)
	return err
}

func (m *Manager) archiveOutput(ctx context.Context, w *Workspace, command ...string) ([]byte, error) {
	if err := m.checkWorkspace(w); err != nil {
		return nil, err
	}
	if err := m.checkKeeper(ctx, w); err != nil {
		return nil, err
	}
	command = workspaceCommand(w, command)
	name, err := randomName(w.ID, "inspect")
	if err != nil {
		return nil, err
	}
	args := []string{"run", "--rm", "--log-driver", "none", "--label", managedLabel + "=" + managedValue,
		"--label", workspaceLabel + "=" + workspaceHashForVolume(w.volume),
		"--label", runLabel + "=" + runHash(w.ID),
		"--network", "none", "--user", workerUID,
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--pids-limit", "64", "--memory", "256m", "--name", name,
		"--workdir", "/workspace"}
	args = append(args, workspaceMountArgs(w, true, true)...)
	args = append(args, "--entrypoint", command[0], m.image)
	args = append(args, command[1:]...)
	out, err := m.dockerArchiveOutput(ctx, maxArchiveBytes, args...)
	if err != nil {
		return out, err
	}
	return out, m.checkKeeper(ctx, w)
}

func (m *Manager) dockerInputOutput(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, m.docker, args...)
	cmd.Stdin = input
	cmd.Env = dockerClientEnv()
	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = 32<<20, 1<<20
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args[:min(len(args), 3)], " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func (m *Manager) dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, m.docker, args...)
	cmd.Env = dockerClientEnv()
	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = maxArchiveBytes, 1<<20
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("docker %s: %w: %s", strings.Join(args[:min(len(args), 3)], " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// Archive output must be complete: tar accepts EOF at a member boundary,
// so silently truncating it could turn omitted source files into deletions.
func (m *Manager) dockerArchiveOutput(ctx context.Context, limit int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, m.docker, args...)
	cmd.Env = dockerClientEnv()
	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = limit, 1<<20
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if stdout.truncated {
		return nil, fmt.Errorf("workspace archive exceeds the %d byte export limit", limit)
	}
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args[:min(len(args), 3)], " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func (m *Manager) dockerOutputStreams(ctx context.Context, args ...string) ([]byte, error, []byte) {
	cmd := exec.CommandContext(ctx, m.docker, args...)
	cmd.Env = dockerClientEnv()
	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = 32<<20, 1<<20
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("docker %s: %w: %s", strings.Join(args[:min(len(args), 3)], " "), err, stderr.String()), stderr.Bytes()
	}
	return stdout.Bytes(), nil, stderr.Bytes()
}

func dockerClientEnv() []string {
	keys := []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (m *Manager) removeVolume(ctx context.Context, volume string) error {
	if _, err := m.dockerOutput(ctx, "volume", "rm", "--force", volume); err != nil {
		// A prior cleanup may have removed the volume while another cleanup
		// step failed. Preserve the handle for retries, and accept absence only
		// after a successful Docker listing confirms this exact name is gone.
		out, listErr := m.dockerOutput(ctx, "volume", "ls", "-q", "--filter", "name="+volume)
		if listErr == nil {
			exists := false
			for _, name := range strings.Fields(string(out)) {
				exists = exists || name == volume
			}
			if !exists {
				return nil
			}
		}
		return fmt.Errorf("remove workspace volume: %w", err)
	}
	return nil
}

func (m *Manager) checkWorkspace(w *Workspace) error {
	if m == nil || w == nil || w.manager != m {
		return errors.New("workspace does not belong to this manager")
	}
	m.mu.Lock()
	_, exists := m.workspaces[w]
	m.mu.Unlock()
	if !exists {
		return errors.New("workspace has been destroyed or is unknown")
	}
	return nil
}

func snapshotTar(files map[string]File) ([]byte, error) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	cleanFiles := make(map[string]File, len(files))
	dirs := make(map[string]struct{})
	for name, file := range files {
		if len(file.Data) > maxFileBytes {
			_ = tw.Close()
			return nil, fmt.Errorf("snapshot file %q exceeds the 64 MiB limit", name)
		}
		clean, err := cleanRelativePath(name)
		if err != nil {
			_ = tw.Close()
			return nil, err
		}
		if hasGitComponent(clean) {
			_ = tw.Close()
			return nil, fmt.Errorf("snapshot cannot contain Git metadata: %q", name)
		}
		mode, err := NormalizeGitFileMode(file.Mode)
		if err != nil {
			_ = tw.Close()
			return nil, fmt.Errorf("snapshot file %q has unsupported mode: %w", name, err)
		}
		file.Mode = mode
		cleanFiles[clean] = file
		for parent := path.Dir(clean); parent != "."; parent = path.Dir(parent) {
			dirs[parent] = struct{}{}
		}
	}
	for dir := range dirs {
		if _, fileCollision := cleanFiles[dir]; fileCollision {
			_ = tw.Close()
			return nil, fmt.Errorf("snapshot path is both a file and directory: %q", dir)
		}
	}
	dirNames := make([]string, 0, len(dirs))
	for dir := range dirs {
		dirNames = append(dirNames, dir)
	}
	sort.Strings(dirNames)
	for _, dir := range dirNames {
		if err := tw.WriteHeader(&tar.Header{Name: dir + "/", Mode: 0755, Typeflag: tar.TypeDir, Format: tar.FormatPAX}); err != nil {
			_ = tw.Close()
			return nil, err
		}
	}
	fileNames := make([]string, 0, len(cleanFiles))
	for name := range cleanFiles {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)
	for _, clean := range fileNames {
		file := cleanFiles[clean]
		header := &tar.Header{Name: clean, Mode: int64(file.Mode), Size: int64(len(file.Data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(header); err != nil {
			_ = tw.Close()
			return nil, err
		}
		if _, err := tw.Write(file.Data); err != nil {
			_ = tw.Close()
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func cleanRelativePath(input string) (string, error) {
	if input == "" || strings.ContainsAny(input, "\x00\r\n") || strings.Contains(input, "\\") {
		return "", fmt.Errorf("invalid workspace path %q", input)
	}
	clean := path.Clean(input)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean == "." || clean != input {
		return "", fmt.Errorf("workspace path must be a non-empty relative path: %q", input)
	}
	return clean, nil
}

func randomName(id, purpose string) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(id))
	return "aiwf-" + purpose + "-" + hex.EncodeToString(h[:6]) + "-" + hex.EncodeToString(nonce[:]), nil
}

func workspaceHashForVolume(volume string) string {
	h := sha256.Sum256([]byte(volume))
	return hex.EncodeToString(h[:])
}

func timeoutSeconds(timeout time.Duration) string {
	return strconv.FormatFloat(timeout.Seconds(), 'f', 3, 64)
}

func parseExitCode(output []byte) (int, error) {
	var code int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(output)), "%d", &code); err != nil || code < 0 {
		return 0, fmt.Errorf("invalid docker wait exit code %q", strings.TrimSpace(string(output)))
	}
	return code, nil
}

type snapshotMeta struct {
	hash [32]byte
	mode fs.FileMode
}

func sortChanges(changes []Change) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
}

// workspaceTarArgs excludes only actual .git directories and their contents.
// GNU tar's --exclude-vcs also filters normal source files such as
// .gitignore and .gitattributes. Include both leading-dot and non-leading-dot
// patterns because archives of "." name entries as "./path", while archives
// of a named directory use "path/...".
func workspaceTarArgs(target string) []string {
	return []string{
		"tar",
		"--exclude=.git", "--exclude=.git/", "--exclude=.git/*",
		"--exclude=./.git", "--exclude=./.git/", "--exclude=./.git/*",
		"--exclude=*/.git", "--exclude=*/.git/", "--exclude=*/.git/*",
		"-cf", "-", "-C", "/workspace", "--", target,
	}
}

func hasGitComponent(p string) bool {
	for _, component := range strings.Split(p, "/") {
		if component == ".git" {
			return true
		}
	}
	return false
}

type cappedBuffer struct {
	// Do not embed bytes.Buffer: its promoted ReadFrom lets io.Copy (including
	// os/exec stdout/stderr collection) bypass the bounded Write method.
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Len() int       { return b.buffer.Len() }
func (b *cappedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *cappedBuffer) String() string { return b.buffer.String() }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if n > remaining {
		b.truncated = true
	}
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buffer.Write(p)
	}
	return n, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
