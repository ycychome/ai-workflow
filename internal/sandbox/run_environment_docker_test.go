package sandbox

import (
	"strings"
	"testing"
	"time"
)

func TestRunEnvironmentToolsAndTmpAreScopedPerRunDocker(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, w := quotaWorkspace(t, ctx)
	if err := m.WriteFile(ctx, w, "main.go", File{Data: []byte("package main\n\nfunc main() {}\n"), Mode: 0644}); err != nil {
		t.Fatal(err)
	}

	run := func(label string, args []string) RunResult {
		t.Helper()
		result, err := m.Run(ctx, w, args, 60*time.Second)
		if err != nil {
			t.Fatalf("%s: run: %v", label, err)
		}
		if result.ExitCode != 0 || result.TimedOut {
			t.Fatalf("%s: exit=%d timed_out=%v stdout=%q stderr=%q", label, result.ExitCode, result.TimedOut, result.Stdout, result.Stderr)
		}
		return result
	}

	version := run("direct go version", []string{"go", "version"})
	if !strings.Contains(version.Stdout, "go version go1.24.") {
		t.Fatalf("unexpected pinned toolchain: %q", version.Stdout)
	}
	run("direct gofmt", []string{"gofmt", "-d", "main.go"})

	buildScript := `test -x /usr/local/bin/go
test -x /usr/local/bin/gofmt
command -v go
command -v gofmt
go version
gofmt -d main.go
go build -o /tmp/aiwf-run-env-probe main.go
go build -o /tmp/aiwf-run-env-probe main.go
/tmp/aiwf-run-env-probe
printf normal-run > /tmp/aiwf-normal-run-marker`
	normal := run("sh -ceu keeps Go tools and cached build", []string{"sh", "-ceu", buildScript})
	if !strings.Contains(normal.Stdout, "go version go1.24.") {
		t.Fatalf("normal shell did not run the pinned Go tools: %q", normal.Stdout)
	}

	loginScript := `set -eu
test -x /usr/local/bin/go
test -x /usr/local/bin/gofmt
command -v go
command -v gofmt
go version
gofmt -d main.go
go build -o /tmp/aiwf-run-env-probe main.go
go build -o /tmp/aiwf-run-env-probe main.go
/tmp/aiwf-run-env-probe
printf login-run > /tmp/aiwf-login-run-marker`
	login := run("login shell keeps Go tools and cached build", []string{"sh", "-l", "-c", loginScript})
	if !strings.Contains(login.Stdout, "/usr/local/bin/go\n/usr/local/bin/gofmt") {
		t.Fatalf("login shell did not find the /usr/local/bin tool links: %q", login.Stdout)
	}
	failed, err := m.Run(ctx, w, []string{"sh", "-ceu", "false\nprintf unexpected-success"}, 10*time.Second)
	if err != nil {
		t.Fatalf("sh -ceu failure propagation: %v", err)
	}
	if failed.ExitCode == 0 || strings.Contains(failed.Stdout, "unexpected-success") {
		t.Fatalf("sh -ceu must stop at and propagate a failed command: %+v", failed)
	}

	isolated := run("tmp is fresh on the next run", []string{"sh", "-ceu", `test ! -e /tmp/aiwf-normal-run-marker
test ! -e /tmp/aiwf-login-run-marker
test -f /workspace/main.go
printf tmp-isolated-workspace-persistent`})
	if !strings.Contains(isolated.Stdout, "tmp-isolated-workspace-persistent") {
		t.Fatalf("authorized workspace did not persist between runs: %q", isolated.Stdout)
	}
}
