package orchestration

import (
	"context"
	"sync"
	"time"
)

// roleLimiter shares the configured worker capacity across independent workflows.
// Changing the limit affects new admissions; existing tasks finish normally.
type roleLimiter struct {
	mu      sync.Mutex
	active  int
	changed chan struct{}
}

func (l *roleLimiter) acquire(ctx context.Context, limit func() int) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l.mu.Lock()
		if l.changed == nil {
			l.changed = make(chan struct{})
		}
		if l.active < limit() {
			l.active++
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.mu.Lock()
					l.active--
					close(l.changed)
					l.changed = make(chan struct{})
					l.mu.Unlock()
				})
			}, nil
		}
		changed := l.changed
		l.mu.Unlock()
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
