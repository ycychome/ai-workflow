package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// Snapshot exports the current regular source files without Git metadata.
// It supplies the credential-free integrator with an exact candidate snapshot.
func (m *Manager) Snapshot(ctx context.Context, w *Workspace) (map[string]File, error) {
	finish, err := m.beginWorkspaceOperation(ctx, w)
	if err != nil {
		return nil, err
	}
	defer finish()
	data, err := m.archiveOutput(ctx, w, workspaceTarArgs(".")...)
	if err != nil {
		return nil, err
	}
	out := map[string]File{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "." || name == "" || hasGitComponent(name) || h.Typeflag == tar.TypeDir {
			continue
		}
		clean, err := cleanRelativePath(name)
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("cannot export non-regular path %q", clean)
		}
		if h.Size > maxFileBytes {
			return nil, fmt.Errorf("snapshot file %q exceeds size limit", clean)
		}
		content, err := io.ReadAll(io.LimitReader(tr, maxFileBytes))
		if err != nil {
			return nil, err
		}
		mode, err := NormalizeGitFileMode(fs.FileMode(h.Mode))
		if err != nil {
			return nil, fmt.Errorf("cannot export snapshot path %q with unsupported mode: %w", clean, err)
		}
		out[clean] = File{Data: content, Mode: mode}
	}
	return out, nil
}
