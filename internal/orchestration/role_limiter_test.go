package orchestration

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRoleCapacitySharedAndCancelledWaiterDoesNotLeak(t *testing.T) {
	var limiter roleLimiter
	limit := func() int { return 2 }
	first, err := limiter.acquire(context.Background(), limit)
	if err != nil {
		t.Fatal(err)
	}
	second, err := limiter.acquire(context.Background(), limit)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = limiter.acquire(ctx, limit); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third workflow must wait at shared capacity: %v", err)
	}
	first()
	first() // Cleanup may be reached twice; it must not release another task's slot.
	third, err := limiter.acquire(context.Background(), limit)
	if err != nil {
		t.Fatal(err)
	}
	if limiter.active != 2 {
		t.Fatalf("slot count = %d", limiter.active)
	}
	third()
	second()
}

func TestLoweredRoleLimitLetsExistingTasksFinish(t *testing.T) {
	var limiter roleLimiter
	current := 2
	limit := func() int { return current }
	first, _ := limiter.acquire(context.Background(), limit)
	second, _ := limiter.acquire(context.Background(), limit)
	current = 1
	first()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limiter.acquire(ctx, limit); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lowered capacity should wait for existing task: %v", err)
	}
	second()
	next, err := limiter.acquire(context.Background(), limit)
	if err != nil {
		t.Fatal(err)
	}
	next()
}
