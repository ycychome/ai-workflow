package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func fixture() map[string]string {
	return map[string]string{"greeting.txt": "hello\n", "README.md": "trusted\n"}
}
func TestIndependentGitMetadataAndObjects(t *testing.T) {
	ctx := context.Background()
	a, e := New(ctx, t.TempDir(), fixture())
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := New(ctx, t.TempDir(), fixture())
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	original, e := os.ReadFile(filepath.Join(a.Source, ".git", "config"))
	if e != nil {
		t.Fatal(e)
	}
	Git(ctx, a.Work, "config", "remote.origin.url", "attacker")
	Git(ctx, a.Work, "update-ref", "refs/heads/evil", "HEAD")
	now, _ := os.ReadFile(filepath.Join(a.Source, ".git", "config"))
	if string(now) != string(original) {
		t.Fatal("source configuration corrupted")
	}
	if _, e = Git(ctx, b.Work, "rev-parse", "--verify", "refs/heads/evil"); e == nil {
		t.Fatal("refs shared between attempts")
	}
	// Object files must not be writable hardlinks back into the trusted source.
	filepath.WalkDir(filepath.Join(a.Work, ".git", "objects"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(a.Work, ".git", "objects"), path)
		src := filepath.Join(a.Source, ".git", "objects", rel)
		x, _ := os.Stat(path)
		y, e := os.Stat(src)
		if e == nil && os.SameFile(x, y) {
			t.Error("shared object inode")
		}
		return nil
	})
}
func TestCollectUsesRealBytesAndRejectsInvalidArtifacts(t *testing.T) {
	for _, kind := range []string{"empty", "outside scope", "symlink", "mode", "deletion", "forged git config", "valid"} {
		t.Run(kind, func(t *testing.T) {
			w, e := New(context.Background(), t.TempDir(), fixture())
			if e != nil {
				t.Fatal(e)
			}
			defer w.Close()
			os.WriteFile(filepath.Join(w.Work, "greeting.txt"), []byte("hello M0\n"), 0644)
			switch kind {
			case "empty":
				os.WriteFile(filepath.Join(w.Work, "greeting.txt"), []byte("hello\n"), 0644)
			case "outside scope":
				os.WriteFile(filepath.Join(w.Work, "new.txt"), []byte("x"), 0644)
			case "symlink":
				os.Remove(filepath.Join(w.Work, "greeting.txt"))
				os.Symlink("/etc/passwd", filepath.Join(w.Work, "greeting.txt"))
			case "mode":
				os.Chmod(filepath.Join(w.Work, "greeting.txt"), 0755)
			case "deletion":
				os.Remove(filepath.Join(w.Work, "README.md"))
			case "forged git config":
				os.WriteFile(filepath.Join(w.Work, ".git", "config"), []byte("[core]\nfsmonitor = touch /tmp/m0-should-never-run\n"), 0600)
			}
			candidate, e := w.Collect("base")
			valid := kind == "valid" || kind == "forged git config"
			if (e == nil) != valid {
				t.Fatalf("artifact acceptance %v", e)
			}
			if valid {
				if _, e = w.Integrate(context.Background(), candidate); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
func TestBuilderCannotChangeGreetingServiceSourceOrTests(t *testing.T) {
	for _, name := range []string{"app.py", "test_app.py"} {
		t.Run(name, func(t *testing.T) {
			files := fixture()
			files["app.py"] = "print('trusted')\n"
			files["test_app.py"] = "import unittest\n"
			w, e := New(t.Context(), t.TempDir(), files)
			if e != nil {
				t.Fatal(e)
			}
			defer w.Close()
			os.WriteFile(filepath.Join(w.Work, "greeting.txt"), []byte("hello M0\n"), 0644)
			os.WriteFile(filepath.Join(w.Work, name), []byte("print('changed')\n"), 0644)
			if _, e = w.Collect("base"); e == nil {
				t.Fatal("Builder changed immutable service source")
			}
		})
	}
}
