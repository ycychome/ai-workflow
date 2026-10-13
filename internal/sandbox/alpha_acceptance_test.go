package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAcceptanceT12AttemptGitMetadataIsPrivate(t *testing.T) {
	ctx := quotaDockerContext(t)
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	makeWorkspace := func(id string) *Workspace {
		w, err := m.Create(ctx, id, map[string]File{"source.txt": {Data: []byte("original\n")}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := m.Destroy(ctx, w); err != nil {
				t.Error(err)
			}
		})
		return w
	}
	a := makeWorkspace(t.Name() + "-a")
	b := makeWorkspace(t.Name() + "-b")
	before, err := m.Run(ctx, b, []string{"git", "rev-parse", "HEAD"}, 10*time.Second)
	if err != nil || before.ExitCode != 0 {
		t.Fatalf("baseline: %+v %v", before, err)
	}
	changed, err := m.Run(ctx, a, []string{"sh", "-ceu", `git config user.name attacker; git config core.hooksPath /workspace/hooks; mkdir -p hooks; printf 'malicious' > hooks/post-commit; git update-ref refs/heads/injected HEAD; printf changed > source.txt`}, 10*time.Second)
	if err != nil || changed.ExitCode != 0 {
		t.Fatalf("fixture mutation: %+v %v", changed, err)
	}
	after, err := m.Run(ctx, b, []string{"sh", "-ceu", `git rev-parse HEAD; test "$(git config user.name)" = 'AI Workflow Snapshot'; test "$(git config core.hooksPath)" = /dev/null; ! git show-ref --verify refs/heads/injected; test "$(cat source.txt)" = original`}, 10*time.Second)
	if err != nil || after.ExitCode != 0 || !strings.HasPrefix(after.Stdout, strings.TrimSpace(before.Stdout)) {
		t.Fatalf("cross-attempt mutation: %+v %v", after, err)
	}
	changes, err := m.CollectChanges(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if hasGitComponent(change.Path) {
			t.Fatal("Git metadata exported as source")
		}
	}
	if _, err = m.ReadFile(ctx, a, ".git/config"); err == nil {
		t.Fatal("Git metadata exported via read API")
	}
}

func TestAcceptanceT24SecretsSocketAndServiceIsolation(t *testing.T) {
	ctx := quotaDockerContext(t)
	t.Setenv("ALPHA_CONTROL_SECRET", "private-control-marker")
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal(err)
	}
	w := serviceWorkspace(t, ctx, m, t.Name())
	result, err := m.Run(ctx, w, []string{"sh", "-ceu", `test ! -e /secrets; test ! -S /var/run/docker.sock; test -z "${ALPHA_CONTROL_SECRET:-}"; test "$(ls /sys/class/net)" = lo; test ! -e /var/lib/postgresql/data; test -S /run/aiwf-postgres/.s.PGSQL.5432; psql "$DATABASE_URL" -X -At -c 'SELECT inet_server_addr() IS NULL'`}, 20*time.Second)
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "t" {
		t.Fatalf("isolation boundary: %+v %v", result, err)
	}
	t.Run("other_attempt_database", TestPostgresAttemptUnixSocketAndIsolationDocker)
	t.Run("targeted_recovery", TestPostgresRecoveryTargetsOnlyExpiredRunDocker)
}
