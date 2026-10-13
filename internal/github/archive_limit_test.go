package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetArchiveRejectsOversizedDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/pilot/zipball/commit" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Length", fmt.Sprint(maxBinaryResponseBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := &Client{baseURL: server.URL, http: server.Client(), now: time.Now, tokens: map[int64]tokenEntry{42: {value: "test", expires: time.Now().Add(time.Hour)}}}
	got, err := c.ForInstallation(context.Background(), 42).GetArchive(context.Background(), "acme", "pilot", "commit")
	if !errors.Is(err, ErrResponseTooLarge) || got != nil {
		t.Fatalf("oversized archive: bytes=%d err=%v", len(got), err)
	}
}

type countingArchiveReader struct{ reads int }

func (r *countingArchiveReader) Read(p []byte) (int, error) {
	clear(p)
	r.reads += len(p)
	return len(p), nil
}

func TestArchiveStreamLimitDoesNotRequireContentLength(t *testing.T) {
	r := &countingArchiveReader{}
	got, err := readBoundedBytes(r, 32)
	if !errors.Is(err, ErrResponseTooLarge) || got != nil || r.reads != 33 {
		t.Fatalf("unbounded stream: bytes=%d reads=%d err=%v", len(got), r.reads, err)
	}
	for _, size := range []int{0, 31, 32} {
		got, err := readBoundedBytes(strings.NewReader(strings.Repeat("a", size)), 32)
		if err != nil || len(got) != size {
			t.Fatalf("valid %d-byte body: bytes=%d err=%v", size, len(got), err)
		}
	}
	_, err = readBoundedBytes(errorArchiveReader{}, 32)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error lost: %v", err)
	}
}

type errorArchiveReader struct{}

func (errorArchiveReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
