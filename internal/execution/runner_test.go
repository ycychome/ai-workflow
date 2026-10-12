package execution

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testImage = "alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"

func testDocker(t *testing.T) *Docker {
	t.Helper()
	binary := os.Getenv("M0_TEST_DOCKER")
	if binary == "" {
		t.Skip("set M0_TEST_DOCKER to run container isolation tests")
	}
	d, e := NewDocker(binary, testImage)
	if e != nil {
		t.Fatal(e)
	}
	d.Namespace = fmt.Sprintf("test-%d", time.Now().UnixNano())
	t.Cleanup(d.Close)
	return d
}
func TestSandboxHasNoCredentialsHostInterfacesOrNetwork(t *testing.T) {
	d := testDocker(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello"), []byte("hello"), 0644)
	command := `test ! -e /var/run/docker.sock; test ! -e /run/secrets; test ! -e /root/.ssh; test -z "${GITHUB_TOKEN:-}"; test -z "${SSH_AUTH_SOCK:-}"; test -z "${HTTP_PROXY:-}"; test -z "${HTTPS_PROXY:-}"; test "$(id -u)" != 0; test "$(ls /sys/class/net)" = lo; test "$(cat hello)" = hello; printf ok > output; if touch /root/nope 2>/dev/null; then exit 1; fi; if touch /etc/nope 2>/dev/null; then exit 1; fi`
	run, e := d.Start(context.Background(), Request{ID: fmt.Sprintf("isolation-%d", time.Now().UnixNano()), Workspace: dir, Command: command, Timeout: 20 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer d.Remove(context.Background(), run)
	var exit Event
	for e := range d.Events(run) {
		if e.Kind == "exited" {
			exit = e
		}
	}
	if exit.ExitCode != 0 {
		t.Fatalf("sandbox verification failed: %d %s", exit.ExitCode, exit.Log)
	}
	if d.Status(run) != "EXITED" {
		t.Fatal("incorrect lifecycle")
	}
	if _, e = d.CollectArtifacts(run); e != nil {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "output")); string(b) != "ok" {
		t.Fatal("workspace not usable")
	}
}
func TestCancellationKillsChildProcesses(t *testing.T) {
	d := testDocker(t)
	dir := t.TempDir()
	run, e := d.Start(context.Background(), Request{ID: fmt.Sprintf("cancel-%d", time.Now().UnixNano()), Workspace: dir, Command: `sleep 30 & wait`, Timeout: 40 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer d.Remove(context.Background(), run)
	<-d.Events(run)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := d.cmd(context.Background(), "inspect", "-f", "{{.State.Running}}", run.id).Output()
		if string(out) == "true\n" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if e = d.Cancel(context.Background(), run); e != nil {
		t.Fatal(e)
	}
	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop descendants")
	}
	evi, e := d.CollectArtifacts(run)
	if e != nil || evi.ExitCode == 0 {
		t.Fatal("cancelled run appeared successful")
	}
}
func TestTimeoutStopsContainerAndRecoveryRemovesOrphans(t *testing.T) {
	d := testDocker(t)
	run, e := d.Start(context.Background(), Request{ID: fmt.Sprintf("timeout-%d", time.Now().UnixNano()), Workspace: t.TempDir(), Command: `sleep 30`, Timeout: 500 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	for range d.Events(run) {
	}
	out, e := d.CollectArtifacts(run)
	if e != nil || out.ExitCode != 124 {
		t.Fatalf("timeout exit: %+v %v", out, e)
	}
	if e = d.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = d.cmd(context.Background(), "inspect", run.id).Output(); e == nil {
		t.Fatal("orphan retained")
	}
}
