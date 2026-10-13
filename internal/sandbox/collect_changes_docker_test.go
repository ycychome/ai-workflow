package sandbox

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"
)

// This opt-in regression uses the built runner image and a real named Docker
// volume. It verifies GNU tar does not make ordinary VCS config files vanish
// from the candidate diff while the private .git directory remains excluded.
func TestCollectChangesIncludesVCSConfigFilesDocker(t *testing.T) {
	if os.Getenv("AI_WORKFLOW_FORCE_DOCKER") != "1" {
		t.Skip("set AI_WORKFLOW_FORCE_DOCKER=1 with the runner image available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	m, err := NewManager(Options{})
	if err != nil {
		t.Fatal("create sandbox manager")
	}
	w, err := m.Create(ctx, "collect-vcs-config-regression", map[string]File{
		".gitignore":     {Data: []byte("ignored.txt\n")},
		".gitattributes": {Data: []byte("*.go text\n")},
		"main.go":        {Data: []byte("package main\n")},
	})
	if err != nil {
		t.Fatal("create Docker workspace")
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := m.Destroy(cleanup, w); err != nil {
			t.Errorf("destroy Docker workspace: %v", err)
		}
	}()

	changes, err := m.CollectChanges(ctx, w)
	if err != nil {
		t.Fatal("collect unchanged Docker workspace")
	}
	if len(changes) != 0 {
		t.Fatalf("unchanged Docker workspace returned changes: %#v", pathsOf(changes))
	}

	// Create deliberately large root and nested Git metadata. These directories
	// must be excluded by tar itself, before its output reaches the supervisor.
	result, err := m.Run(ctx, w, []string{"/bin/sh", "-c", "mkdir -p .git/objects/pack nested/module/.git/objects/pack && truncate -s 300M .git/objects/pack/large.pack && truncate -s 300M nested/module/.git/objects/pack/large.pack"}, 15*time.Second)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("create large Git metadata: result=%#v err=%v", result, err)
	}

	for filePath, want := range map[string]string{
		".gitignore":     "ignored.txt\n",
		".gitattributes": "*.go text\n",
	} {
		got, err := m.ReadFile(ctx, w, filePath)
		if err != nil || string(got) != want {
			t.Errorf("ReadFile(%q) = %q, %v; want %q", filePath, got, err, want)
		}
	}

	entries, err := m.List(ctx, w, ".")
	if err != nil {
		t.Fatal("list Docker workspace with Git metadata")
	}
	listed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		listed[entry.Path] = true
		if entry.Path == ".git" || entry.Path == "nested/module/.git" || entry.Path == "nested/module/.git/objects/pack/large.pack" {
			t.Errorf("List exposed Git metadata path %q", entry.Path)
		}
	}
	if !listed[".gitignore"] || !listed[".gitattributes"] {
		t.Errorf("List omitted ordinary VCS config files: %#v", listed)
	}

	for _, query := range []string{"ignored.txt", "*.go text"} {
		matches, err := m.Search(ctx, w, query)
		if err != nil {
			t.Fatalf("search %q in Docker workspace", query)
		}
		if len(matches) != 1 {
			t.Errorf("Search(%q) returned %d matches, want 1: %#v", query, len(matches), matches)
		}
	}

	for path, content := range map[string]string{
		".gitignore":     "ignored.txt\n*.tmp\n",
		".gitattributes": "*.go text eol=lf\n*.md text\n",
		"new/.gitignore": "cache/\n",
	} {
		if err := m.WriteFile(ctx, w, path, File{Data: []byte(content), Mode: 0644}); err != nil {
			t.Fatalf("write %s in Docker workspace", path)
		}
	}

	changes, err = m.CollectChanges(ctx, w)
	if err != nil {
		t.Fatal("collect modified Docker workspace")
	}
	want := []string{".gitattributes", ".gitignore", "new/.gitignore"}
	got := pathsOf(changes)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collected paths = %#v, want %#v", got, want)
	}
	wantStatus := map[string]string{".gitattributes": "M", ".gitignore": "M", "new/.gitignore": "A"}
	for _, change := range changes {
		if change.Status != wantStatus[change.Path] {
			t.Errorf("change %s has status %q, want %s", change.Path, change.Status, wantStatus[change.Path])
		}
	}
}

func pathsOf(changes []Change) []string {
	paths := make([]string, len(changes))
	for i := range changes {
		paths[i] = changes[i].Path
	}
	return paths
}
