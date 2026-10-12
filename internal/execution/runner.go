// Package execution owns the only M0 Runner: a bounded Docker process adapter.
package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Capabilities struct {
	Cancel, StructuredEvents, Artifacts bool
	Resume                              bool
}
type Request struct {
	ID, Workspace, Command string
	Timeout                time.Duration
}
type Event struct {
	Kind     string
	ExitCode int
	Log      string
}
type Run struct {
	id     string
	events chan Event
	done   chan struct{}
	mu     sync.Mutex
	status string
	exit   int
}
type Runner interface {
	Capabilities() Capabilities
	Start(context.Context, Request) (*Run, error)
	Events(*Run) <-chan Event
	Status(*Run) string
	Cancel(context.Context, *Run) error
	CollectArtifacts(*Run) (Event, error)
}
type Docker struct{ Binary, Image, ConfigDir, Namespace string }

func NewDocker(binary, image string) (*Docker, error) {
	if os.Getuid() == 0 {
		return nil, errors.New("Runner Supervisor must run as a non-root user")
	}
	if !strings.Contains(image, "@sha256:") {
		return nil, errors.New("Runner image must be pinned by digest")
	}
	p, e := os.MkdirTemp("", "m0-docker-client-")
	if e != nil {
		return nil, e
	}
	if e = os.WriteFile(filepath.Join(p, "config.json"), []byte(`{}`), 0600); e != nil {
		return nil, e
	}
	return &Docker{Binary: binary, Image: image, ConfigDir: p, Namespace: "test"}, nil
}
func (d *Docker) Close()                     { os.RemoveAll(d.ConfigDir) }
func (d *Docker) Capabilities() Capabilities { return Capabilities{true, true, true, false} }
func (d *Docker) cmd(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"--config", d.ConfigDir, "--host=unix:///var/run/docker.sock"}, args...)
	cmd := exec.CommandContext(ctx, d.Binary, full...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	return cmd
}

type boundedLog struct {
	mu sync.Mutex
	b  []byte
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := 64*1024 - len(b.b)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.b = append(b.b, p...)
	}
	return n, nil
}
func (d *Docker) Start(ctx context.Context, req Request) (*Run, error) {
	if req.ID == "" || strings.ContainsAny(req.ID, " /\\\n") || req.Timeout <= 0 || req.Timeout > time.Minute {
		return nil, errors.New("invalid run contract")
	}
	workspace, e := filepath.Abs(req.Workspace)
	if e != nil {
		return nil, e
	}
	if strings.ContainsAny(workspace, ",\n") {
		return nil, errors.New("invalid workspace path")
	}
	run := &Run{id: "m0-" + req.ID, events: make(chan Event, 2), done: make(chan struct{}), status: "RUNNING"}
	// No host interfaces, credentials, shared Git metadata, inherited proxy configuration,
	// extra capabilities or network enter this container.
	args := []string{"create", "--pull=never", "--name", run.id, "--label", "ai-workflow.m0.namespace=" + d.Namespace, "--network=none", "--read-only", fmt.Sprintf("--user=%d:%d", os.Getuid(), os.Getgid()), "--cap-drop=ALL", "--security-opt=no-new-privileges:true", "--pids-limit=64", "--memory=128m", "--cpus=0.5", "--tmpfs=/tmp:rw,noexec,nosuid,size=16m", "--mount", "type=bind,src=" + workspace + ",dst=/workspace", "--workdir=/workspace", d.Image, "/bin/sh", "-eu", "-c", req.Command}
	if out, e := d.cmd(ctx, args...).CombinedOutput(); e != nil {
		return nil, fmt.Errorf("container create: %w: %s", e, out)
	}
	run.events <- Event{Kind: "started"}
	go func() {
		executionCtx, cancel := context.WithTimeout(ctx, req.Timeout)
		defer cancel()
		log := &boundedLog{}
		cmd := d.cmd(executionCtx, "start", "--attach", run.id)
		cmd.Stdout = log
		cmd.Stderr = log
		e := cmd.Run()
		code := 0
		if e != nil {
			code = 1
			if ex, ok := e.(*exec.ExitError); ok {
				code = ex.ExitCode()
			}
		}
		if executionCtx.Err() != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			d.Cancel(stopCtx, run)
			stopCancel()
			code = 124
		}
		run.mu.Lock()
		run.exit = code
		run.status = "EXITED"
		run.mu.Unlock()
		log.mu.Lock()
		output := string(log.b)
		log.mu.Unlock()
		run.events <- Event{Kind: "exited", ExitCode: code, Log: output}
		close(run.events)
		close(run.done)
	}()
	return run, nil
}
func (d *Docker) Events(r *Run) <-chan Event { return r.events }
func (d *Docker) Status(r *Run) string       { r.mu.Lock(); defer r.mu.Unlock(); return r.status }
func (d *Docker) Cancel(ctx context.Context, r *Run) error {
	// Killing the container terminates its entire PID namespace, including descendants.
	out, e := d.cmd(ctx, "kill", r.id).CombinedOutput()
	if e != nil && !strings.Contains(string(out), "is not running") && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("cancel container: %w", e)
	}
	return nil
}
func (d *Docker) CollectArtifacts(r *Run) (Event, error) {
	select {
	case <-r.done:
	default:
		return Event{}, errors.New("execution still running")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return Event{Kind: "exited", ExitCode: r.exit}, nil
}
func (d *Docker) Remove(ctx context.Context, r *Run) error {
	_, e := d.cmd(ctx, "rm", "--force", r.id).CombinedOutput()
	return e
}

// Recover must run under the repository supervisor lock before accepting events.
func (d *Docker) Recover(ctx context.Context) error {
	out, e := d.cmd(ctx, "ps", "-aq", "--filter", "label=ai-workflow.m0.namespace="+d.Namespace).CombinedOutput()
	if e != nil {
		return e
	}
	for _, id := range strings.Fields(string(out)) {
		if _, e = d.cmd(ctx, "rm", "--force", id).CombinedOutput(); e != nil {
			return e
		}
	}
	return nil
}
