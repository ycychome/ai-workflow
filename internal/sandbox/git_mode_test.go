package sandbox

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"testing"
)

func TestNormalizeGitFileModeUsesOnlyOwnerExecuteBit(t *testing.T) {
	for _, tc := range []struct {
		mode fs.FileMode
		want fs.FileMode
	}{
		{0644, 0644},
		{0444, 0644},
		{0600, 0644},
		{0640, 0644},
		{0654, 0644}, // group execute is not the Git executable bit
		{0645, 0644}, // other execute is not the Git executable bit
		{0744, 0755},
		{0755, 0755},
	} {
		got, err := NormalizeGitFileMode(tc.mode)
		if err != nil || got != tc.want {
			t.Errorf("mode %#o normalized to %#o, want %#o (err=%v)", tc.mode, got, tc.want, err)
		}
	}
}

func TestNormalizeGitFileModeRejectsTypeAndSpecialBits(t *testing.T) {
	for _, mode := range []fs.FileMode{
		fs.ModeDir | 0755,
		fs.ModeSymlink | 0777,
		fs.ModeSetuid | 0644,
		fs.ModeSetgid | 0644,
		fs.ModeSticky | 0644,
		fs.ModeNamedPipe | 0644,
	} {
		if got, err := NormalizeGitFileMode(mode); err == nil {
			t.Errorf("accepted unsupported mode %#o as %#o", mode, got)
		}
	}
}

func TestSnapshotTarNormalizesPermissionOnlyModes(t *testing.T) {
	archive, err := snapshotTar(map[string]File{
		"readonly": {Data: []byte("read-only"), Mode: 0444},
		"private":  {Data: []byte("private"), Mode: 0600},
		"owner-x":  {Data: []byte("executable"), Mode: 0744},
		"group-x":  {Data: []byte("not executable in Git"), Mode: 0654},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"readonly": 0644, "private": 0644, "owner-x": 0755, "group-x": 0644}
	reader := tar.NewReader(bytes.NewReader(archive))
	for len(want) > 0 {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("read normalized archive: %v", err)
		}
		mode, ok := want[header.Name]
		if !ok || header.Mode != mode {
			t.Errorf("archive entry %s mode=%#o, want %#o", header.Name, header.Mode, mode)
		}
		delete(want, header.Name)
	}
}

func TestSnapshotTarRejectsSetuidMode(t *testing.T) {
	if _, err := snapshotTar(map[string]File{"setuid": {Data: []byte("file"), Mode: fs.ModeSetuid | 0644}}); err == nil {
		t.Fatal("accepted a special setuid mode")
	}
}
