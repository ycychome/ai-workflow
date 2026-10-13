package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestReadOnlyCandidateRejectsTransientMutationDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Create(ctx, t.Name(), map[string]File{"source.txt": {Data: []byte("original\n")}, "go.mod": {Data: []byte("module readonly.test\n\ngo 1.24.0\n")}, "main_test.go": {Data: []byte("package readonly\nimport \"testing\"\nfunc TestImmutable(t *testing.T) {}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Destroy(c, w); err != nil {
			t.Error(err)
		}
	})
	if err = m.SealReadOnly(ctx, w, true); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{`cp source.txt /tmp/original; printf forged > source.txt; cp /tmp/original source.txt`, `chmod 777 source.txt`, `rm source.txt`, `printf injected > .git/config`, `ln -s /workspace/source.txt review_tests/source; printf forged > review_tests/source`} {
		result, err := m.Run(ctx, w, []string{"sh", "-ceu", script}, 10*time.Second)
		if err != nil || result.ExitCode == 0 {
			t.Fatalf("candidate write unexpectedly succeeded: %+v %v", result, err)
		}
	}
	if err = m.WriteFile(ctx, w, "source.txt", File{Data: []byte("forged")}); err == nil {
		t.Fatal("helper changed sealed candidate")
	}
	if err = m.WriteFile(ctx, w, "review_tests/probe.txt", File{Data: []byte("independent proof")}); err != nil {
		t.Fatal(err)
	}
	got, err := m.ReadFile(ctx, w, "review_tests/probe.txt")
	if err != nil || string(got) != "independent proof" {
		t.Fatalf("scratch read=%q %v", got, err)
	}
	result, err := m.Run(ctx, w, []string{"sh", "-ceu", `test "$(cat review_tests/probe.txt)" = 'independent proof'; printf persisted > review_tests/from-shell; go test ./...`}, 45*time.Second)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("readonly tests/cache failed: %+v %v", result, err)
	}
	got, err = m.ReadFile(ctx, w, "review_tests/from-shell")
	if err != nil || string(got) != "persisted" {
		t.Fatalf("scratch not persistent: %q %v", got, err)
	}
	got, err = m.ReadFile(ctx, w, "source.txt")
	if err != nil || string(got) != "original\n" {
		t.Fatalf("source changed: %q %v", got, err)
	}
	// Symbolic links created by untrusted commands must still be refused.
	_, err = m.ReadFile(ctx, w, "review_tests/source")
	if err == nil {
		t.Fatal("scratch symlink read bypassed snapshot rules")
	}
	if err = m.SealReadOnly(ctx, w, false); err == nil {
		t.Fatal("seal mode changed")
	}
}

func TestReadOnlySealRejectsCandidateScratchCollisionDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Create(ctx, t.Name(), map[string]File{"review_tests/existing.txt": {Data: []byte("candidate-owned")}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Destroy(context.Background(), w)
	if err = m.SealReadOnly(ctx, w, true); err == nil || !strings.Contains(err.Error(), "reserved review_tests") {
		t.Fatalf("candidate scratch silently hidden: %v", err)
	}
	if err = m.SealReadOnly(ctx, w, false); err != nil {
		t.Fatal(err)
	}
	result, err := m.Run(ctx, w, []string{"sh", "-ceu", `printf forged > review_tests/existing.txt`}, 10*time.Second)
	if err != nil || result.ExitCode == 0 {
		t.Fatalf("read-only existing test changed: %+v %v", result, err)
	}
}
