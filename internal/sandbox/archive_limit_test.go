package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveOutputRejectsTruncationAtTarMemberBoundary(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, name := range []string{"first.txt", "must-not-become-a-deletion.txt"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	// This tiny boundary models the real 256 MiB cap. Go's tar reader treats
	// the truncated archive as one complete member, with no malformed-tar error.
	const limit = 1024
	tr := tar.NewReader(bytes.NewReader(archive.Bytes()[:limit]))
	if h, err := tr.Next(); err != nil || h.Name != "first.txt" {
		t.Fatalf("first member: header=%v err=%v", h, err)
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected silent member-boundary EOF, got %v", err)
	}
	path := filepath.Join(t.TempDir(), "archive.tar")
	if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	// Cat exercises the subprocess-output path without a Docker daemon.
	m := &Manager{docker: "/bin/cat"}
	got, err := m.dockerArchiveOutput(context.Background(), limit, path)
	if err == nil || got != nil || !strings.Contains(err.Error(), "export limit") {
		t.Fatalf("partial archive accepted: length=%d err=%v", len(got), err)
	}
	got, err = m.dockerArchiveOutput(context.Background(), archive.Len(), path)
	if err != nil || !bytes.Equal(got, archive.Bytes()) {
		t.Fatalf("complete archive at exact limit rejected: length=%d err=%v", len(got), err)
	}
}

func TestCappedBufferRecordsOverflowAcrossWrites(t *testing.T) {
	b := cappedBuffer{limit: 3}
	if n, err := b.Write([]byte("abc")); n != 3 || err != nil || b.truncated {
		t.Fatalf("exact limit: n=%d err=%v truncated=%v", n, err, b.truncated)
	}
	if n, err := b.Write([]byte("def")); n != 3 || err != nil || !b.truncated || b.String() != "abc" {
		t.Fatalf("overflow: n=%d err=%v truncated=%v retained=%q", n, err, b.truncated, b.String())
	}
}

func TestCappedBufferCannotBypassLimitThroughCopy(t *testing.T) {
	b := cappedBuffer{limit: 3}
	// Hide the source's WriterTo so io.Copy would select a promoted ReadFrom
	// if cappedBuffer ever embeds bytes.Buffer again.
	n, err := io.Copy(&b, struct{ io.Reader }{strings.NewReader("abcdefgh")})
	if err != nil || n != 8 || b.String() != "abc" || !b.truncated {
		t.Fatalf("io.Copy bypassed cap: n=%d err=%v retained=%q truncated=%v", n, err, b.String(), b.truncated)
	}
}
