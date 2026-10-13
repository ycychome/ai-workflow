package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUntrustedServiceSelectionRejectedBeforeDocker(t *testing.T) {
	m, err := NewManager(Options{DockerBinary: "must-not-run"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.CreateWithServices(context.Background(), "run", nil, []string{"postgres://production"}); err == nil {
		t.Fatal("arbitrary service accepted")
	}
}
func TestCleanupCannotConfirmExitWhenDockerIsUnavailable(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{DockerBinary: fake})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.RecoverWorkspace(context.Background(), "expired"); err == nil {
		t.Fatal("ambiguous cleanup reported success")
	}
	if err = m.removeContainerConfirmed(context.Background(), "worker"); err == nil {
		t.Fatal("ambiguous process exit reported success")
	}
}
