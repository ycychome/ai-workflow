package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CreateJudgePeer creates a separate trusted filesystem and PID namespace for
// an external HTTP/DB harness. It never imports or mounts candidate source.
// The candidate must already be sealed and have a private PostgreSQL sidecar.
// Run the candidate service with Run in a goroutine, then run the peer harness;
// their stdout streams are independent. StopRun(candidate.ID) stops both.
func (m *Manager) CreateJudgePeer(ctx context.Context, candidate *Workspace, files map[string]File) (*Workspace, error) {
	if err := m.checkWorkspace(candidate); err != nil {
		return nil, err
	}
	candidate.lifecycleMu.Lock()
	defer candidate.lifecycleMu.Unlock()
	if err := m.checkKeeper(ctx, candidate); err != nil {
		return nil, err
	}
	if !candidate.readOnly || candidate.reviewScratch != nil || candidate.postgresContainer == "" || candidate.judgeOwner != nil || candidate.judgePeer != nil {
		return nil, errors.New("judge peer requires one sealed candidate with private PostgreSQL")
	}
	if err := m.checkPostgres(ctx, candidate); err != nil {
		return nil, err
	}
	transportManager, err := NewManager(Options{DockerBinary: m.docker, RunnerImage: m.image, PostgresImage: m.postgresImage, CPUQuota: m.cpus, MemoryBytes: m.memory, PidsLimit: m.pids, WorkspaceBytes: 1 << 20})
	if err != nil {
		return nil, err
	}
	transport, err := transportManager.Create(ctx, candidate.ID, map[string]File{})
	if err != nil {
		return nil, fmt.Errorf("create bounded judge transport: %w", err)
	}
	peer, err := m.Create(ctx, candidate.ID, files)
	cleanup := func(cause error) (*Workspace, error) {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if peer != nil {
			cause = errors.Join(cause, m.Destroy(c, peer))
		}
		return nil, errors.Join(cause, transportManager.Destroy(c, transport))
	}
	if err != nil {
		return cleanup(err)
	}
	if err = m.SealReadOnly(ctx, peer, false); err != nil {
		return cleanup(err)
	}
	if err = m.checkKeeper(ctx, candidate); err != nil {
		return cleanup(err)
	}
	peer.socketVolume = candidate.socketVolume
	peer.postgresContainer = candidate.postgresContainer
	peer.postgresID = candidate.postgresID
	peer.postgresStartedAt = candidate.postgresStartedAt
	peer.databaseURL = candidate.databaseURL
	peer.sharedServices = true
	peer.judgeOwner = candidate
	peer.judgeTransport = transport
	candidate.judgeTransport = transport
	candidate.judgePeer = peer
	return peer, nil
}

func judgeMountArgs(w *Workspace) []string {
	if w.judgeTransport == nil {
		return nil
	}
	mount := "type=volume,src=" + w.judgeTransport.volume + ",dst=/run/aiwf-judge,volume-nocopy"
	if w.judgeOwner != nil {
		mount += ",readonly"
	}
	return []string{"--mount", mount, "--env", "AI_WORKFLOW_JUDGE_SOCKET=/run/aiwf-judge/candidate.sock"}
}
