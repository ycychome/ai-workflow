package orchestration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ycychome/ai-workflow/internal/sandbox"
)

func TestSourceSnapshotCacheDeduplicatesAndReturnsCopies(t *testing.T) {
	var cache sourceSnapshotCache
	var loads atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	load := func() (map[string]sandbox.File, error) {
		loads.Add(1)
		close(started)
		<-release
		return map[string]sandbox.File{"a.go": {Data: []byte("source"), Mode: 0644}}, nil
	}
	type result struct {
		files map[string]sandbox.File
		err   error
	}
	first := make(chan result, 1)
	go func() {
		files, err := cache.get(context.Background(), "repo\x00sha", load)
		first <- result{files, err}
	}()
	<-started

	second := make(chan result, 1)
	go func() {
		files, err := cache.get(context.Background(), "repo\x00sha", func() (map[string]sandbox.File, error) {
			loads.Add(1)
			return nil, errors.New("duplicate fetch")
		})
		second <- result{files, err}
	}()
	close(release)

	a, b := <-first, <-second
	if a.err != nil || b.err != nil || loads.Load() != 1 {
		t.Fatalf("first err=%v second err=%v loads=%d", a.err, b.err, loads.Load())
	}
	a.files["a.go"].Data[0] = 'X'
	cached, err := cache.get(context.Background(), "repo\x00sha", func() (map[string]sandbox.File, error) {
		loads.Add(1)
		return nil, errors.New("cache miss")
	})
	if err != nil || string(cached["a.go"].Data) != "source" || loads.Load() != 1 {
		t.Fatalf("cache did not return an isolated copy: data=%q err=%v loads=%d", cached["a.go"].Data, err, loads.Load())
	}
}

func TestSourceSnapshotCacheWaitCanBeCanceled(t *testing.T) {
	var cache sourceSnapshotCache
	started := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := cache.get(context.Background(), "repo\x00sha", func() (map[string]sandbox.File, error) {
			close(started)
			<-release
			return map[string]sandbox.File{}, nil
		})
		first <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.get(ctx, "repo\x00sha", func() (map[string]sandbox.File, error) {
		return nil, errors.New("unexpected second load")
	}); !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("canceled waiter error = %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}
