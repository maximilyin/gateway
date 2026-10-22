package ratelimit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T, clk clock.Clock, maxKeys int, idle time.Duration) *MemoryStore {
	t.Helper()
	s := NewMemoryStore(clk, maxKeys, idle, time.Hour)
	t.Cleanup(s.Close)
	return s
}

func TestMemoryStore_BurstAndRefill(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	s := newTestStore(t, clk, 10, time.Hour)
	lim := Limit{RPS: 1, Burst: 2}

	for i := 1; i <= 2; i++ {
		d, err := s.Allow(ctx, "a", lim)
		if err != nil || !d.Allowed {
			t.Fatalf("request %d: allowed=%v err=%v", i, d.Allowed, err)
		}
		if d.Remaining != float64(2-i) {
			t.Errorf("request %d remaining = %v, want %d", i, d.Remaining, 2-i)
		}
	}

	d, err := s.Allow(ctx, "a", lim)
	if err != nil || d.Allowed {
		t.Fatalf("over burst: allowed=%v err=%v", d.Allowed, err)
	}
	if d.RetryAfter != time.Second {
		t.Errorf("retry after = %s, want 1s", d.RetryAfter)
	}

	clk.Advance(time.Second)
	d, err = s.Allow(ctx, "a", lim)
	if err != nil || !d.Allowed {
		t.Fatalf("after refill: allowed=%v err=%v", d.Allowed, err)
	}
}

func TestMemoryStore_KeysAreIndependent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, clock.NewFake(t0), 10, time.Hour)
	lim := Limit{RPS: 1, Burst: 1}

	if d, err := s.Allow(ctx, "session-a", lim); err != nil || !d.Allowed {
		t.Fatalf("a: allowed=%v err=%v", d.Allowed, err)
	}
	if d, err := s.Allow(ctx, "session-b", lim); err != nil || !d.Allowed {
		t.Fatalf("b: allowed=%v err=%v", d.Allowed, err)
	}
	if d, _ := s.Allow(ctx, "session-a", lim); d.Allowed {
		t.Fatal("session-a second request was allowed")
	}
}

func TestMemoryStore_IdleBucketIsForgotten(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	s := newTestStore(t, clk, 10, time.Minute)
	lim := Limit{RPS: 0.001, Burst: 1}

	if _, err := s.Allow(ctx, "a", lim); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Allow(ctx, "a", lim); d.Allowed {
		t.Fatal("second request was allowed before idle expiry")
	}

	clk.Advance(time.Minute)
	if removed := s.sweep(); removed != 1 {
		t.Fatalf("sweep removed %d, want 1", removed)
	}
	// A forgotten empty bucket comes back full, so the client is allowed again
	// even though the refill rate would not have produced a token yet.
	d, err := s.Allow(ctx, "a", lim)
	if err != nil || !d.Allowed {
		t.Fatalf("after sweep: allowed=%v err=%v", d.Allowed, err)
	}
}

func TestMemoryStore_MaxKeys(t *testing.T) {
	s := newTestStore(t, clock.NewFake(t0), 1, time.Hour)
	lim := Limit{RPS: 1, Burst: 1}
	if _, err := s.Allow(context.Background(), "a", lim); err != nil {
		t.Fatal(err)
	}
	_, err := s.Allow(context.Background(), "b", lim)
	if !errors.Is(err, ErrStoreFull) {
		t.Fatalf("err = %v, want ErrStoreFull", err)
	}
}

func TestMemoryStore_ConcurrentBurst(t *testing.T) {
	s := newTestStore(t, clock.NewFake(t0), 10, time.Hour)
	const burst = 100
	lim := Limit{RPS: 1, Burst: burst}

	var wg sync.WaitGroup
	var allowed atomic.Int64
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := s.Allow(context.Background(), "k", lim)
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != burst {
		t.Fatalf("allowed = %d, want %d", got, burst)
	}
	if d, _ := s.Allow(context.Background(), "k", lim); d.Allowed {
		t.Fatal("request past the burst was allowed")
	}
}
