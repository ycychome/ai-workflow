// Package artifacts treats the worker filesystem and its Git configuration as untrusted.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	c "github.com/ycychome/ai-workflow/internal/contracts"
	"github.com/ycychome/ai-workflow/internal/policies"
)

type Workspace struct {
	Root, Source, Work string
	Original           map[string]string
}

func Git(ctx context.Context, dir string, args ...string) (string, error) {
	fixed := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "commit.gpgsign=false", "-c", "user.name=M0", "-c", "user.email=m0@example.invalid"}
	cmd := exec.CommandContext(ctx, "git", append(fixed, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	out, e := cmd.CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], e, out)
	}
	return strings.TrimSpace(string(out)), nil
}
func New(ctx context.Context, parent string, files map[string]string) (*Workspace, error) {
	root, e := os.MkdirTemp(parent, "attempt-")
	if e != nil {
		return nil, e
	}
	w := &Workspace{root, filepath.Join(root, "source"), filepath.Join(root, "work"), files}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(root)
		}
	}()
	if e = os.Mkdir(w.Source, 0700); e != nil {
		return nil, e
	}
	for name, value := range files {
		if !policies.SnapshotPath(name) {
			return nil, errors.New("M0 snapshot scope violation")
		}
		if e = os.WriteFile(filepath.Join(w.Source, name), []byte(value), 0644); e != nil {
			return nil, e
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "--", "."}, {"commit", "-qm", "trusted snapshot"}} {
		if _, e = Git(ctx, w.Source, args...); e != nil {
			return nil, e
		}
	}
	if _, e = Git(ctx, root, "clone", "--no-local", "--no-hardlinks", "--", w.Source, w.Work); e != nil {
		return nil, e
	}
	// Normalize the trusted starting modes independently of the supervisor's umask.
	for name := range files {
		if e = os.Chmod(filepath.Join(w.Work, name), 0644); e != nil {
			return nil, e
		}
	}
	ok = true
	return w, nil
}
func (w *Workspace) Close() { os.RemoveAll(w.Root) }
func ReadRegular(dir, name string) (string, error) {
	info, e := os.Lstat(filepath.Join(dir, name))
	if e != nil {
		return "", e
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return "", errors.New("invalid candidate file")
	}
	b, e := os.ReadFile(filepath.Join(dir, name))
	return string(b), e
}
func (w *Workspace) Collect(baseSHA string) (c.Candidate, error) {
	changed := map[string]string{}
	// Do not invoke Git on Worker-controlled .git/config, attributes or hooks.
	e := filepath.WalkDir(w.Work, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, e := filepath.Rel(w.Work, path)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		if rel == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected directory or symbolic link")
		}
		if _, ok := w.Original[rel]; !ok {
			return errors.New("out-of-scope artifact: " + rel)
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0644 || info.Size() > 65536 {
			return errors.New("unsupported artifact mode or size")
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if string(b) != w.Original[rel] {
			if rel != "greeting.txt" {
				return errors.New("file is outside the patch scope: " + rel)
			}
			changed[rel] = string(b)
		}
		return nil
	})
	if e != nil {
		return c.Candidate{}, e
	}
	for name := range w.Original {
		if _, e = os.Lstat(filepath.Join(w.Work, name)); e != nil {
			return c.Candidate{}, errors.New("deleted artifact")
		}
	}
	if len(changed) != 1 || changed["greeting.txt"] != "hello M0\n" || w.Original["greeting.txt"] != "hello\n" {
		return c.Candidate{}, errors.New("missing or invalid fixed patch")
	}
	return c.Candidate{BaseSHA: baseSHA, Files: changed, ArtifactDigest: c.Digest(changed)}, nil
}

// Integrate applies the collected bytes to a clean, credential-free trusted copy.
func (w *Workspace) Integrate(ctx context.Context, candidate c.Candidate) (string, error) {
	if candidate.ArtifactDigest != c.Digest(candidate.Files) {
		return "", errors.New("artifact digest mismatch")
	}
	for name, value := range candidate.Files {
		if name != "greeting.txt" {
			return "", errors.New("integration scope violation")
		}
		if e := os.WriteFile(filepath.Join(w.Source, name), []byte(value), 0644); e != nil {
			return "", e
		}
	}
	if _, e := Git(ctx, w.Source, "add", "--", "greeting.txt"); e != nil {
		return "", e
	}
	if _, e := Git(ctx, w.Source, "commit", "-qm", "integrated fixed patch"); e != nil {
		return "", e
	}
	return Git(ctx, w.Source, "rev-parse", "HEAD")
}
