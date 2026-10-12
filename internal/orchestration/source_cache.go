package orchestration

import (
	"container/list"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
)

const maxCachedSourceSnapshots = 16

type snapshotEntry struct {
	key   string
	files map[string]sandbox.File
}

type snapshotFetch struct {
	done  chan struct{}
	files map[string]sandbox.File
	err   error
}

// sourceSnapshotCache is independent of Engine.mu: archive HTTP requests never
// hold the engine configuration lock. Concurrent requests for the same exact
// repository/SHA share one fetch, while each caller receives its own byte copy.
type sourceSnapshotCache struct {
	mu       sync.Mutex
	entries  map[string]*list.Element
	lru      *list.List
	inflight map[string]*snapshotFetch
}

func (c *sourceSnapshotCache) get(ctx context.Context, key string, load func() (map[string]sandbox.File, error)) (map[string]sandbox.File, error) {
	c.mu.Lock()
	c.initialize()
	if element := c.entries[key]; element != nil {
		c.lru.MoveToFront(element)
		files := cloneSnapshot(element.Value.(*snapshotEntry).files)
		c.mu.Unlock()
		return files, nil
	}
	if fetch := c.inflight[key]; fetch != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-fetch.done:
			if fetch.err != nil {
				return nil, fetch.err
			}
			return cloneSnapshot(fetch.files), nil
		}
	}
	fetch := &snapshotFetch{done: make(chan struct{})}
	c.inflight[key] = fetch
	c.mu.Unlock()

	files, err := load()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	c.mu.Lock()
	if err == nil {
		fetch.files = cloneSnapshot(files)
		c.insert(key, fetch.files)
	}
	fetch.err = err
	delete(c.inflight, key)
	close(fetch.done)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return cloneSnapshot(files), nil
}

func (c *sourceSnapshotCache) initialize() {
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	if c.lru == nil {
		c.lru = list.New()
	}
	if c.inflight == nil {
		c.inflight = make(map[string]*snapshotFetch)
	}
}

func (c *sourceSnapshotCache) insert(key string, files map[string]sandbox.File) {
	if element := c.entries[key]; element != nil {
		element.Value.(*snapshotEntry).files = files
		c.lru.MoveToFront(element)
		return
	}
	element := c.lru.PushFront(&snapshotEntry{key: key, files: files})
	c.entries[key] = element
	if c.lru.Len() > maxCachedSourceSnapshots {
		oldest := c.lru.Back()
		entry := oldest.Value.(*snapshotEntry)
		delete(c.entries, entry.key)
		c.lru.Remove(oldest)
	}
}

func cloneSnapshot(files map[string]sandbox.File) map[string]sandbox.File {
	copyOfFiles := make(map[string]sandbox.File, len(files))
	for path, file := range files {
		file.Data = append([]byte(nil), file.Data...)
		copyOfFiles[path] = file
	}
	return copyOfFiles
}

func retryableArchiveRead(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var apiErr *gh.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusBadGateway || apiErr.StatusCode == http.StatusServiceUnavailable || apiErr.StatusCode == http.StatusGatewayTimeout
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func waitArchiveRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
