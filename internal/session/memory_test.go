package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// newTestStore uses a long cleanup interval so the sweeper never fires on its own;
// tests call sweep directly.
func newTestStore(t *testing.T, clk clock.Clock, maxItems int) *MemoryStore {
	t.Helper()
	s := NewMemoryStore(clk, maxItems, time.Hour)
	t.Cleanup(s.Close)
	return s
}

func TestMemoryStore_CRUD(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	s := newTestStore(t, clk, 10)

	sess := Session{ID: "a", ClientIP: "1.2.3.4", CreatedAt: t0, ExpiresAt: t0.Add(time.Minute)}
	if err := s.Save(ctx, sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Get(ctx, "a")
	if err != nil || got != sess {
		t.Fatalf("Get = %+v, %v; want %+v", got, err, sess)
	}

	newExpiry := t0.Add(5 * time.Minute)
	if err := s.Extend(ctx, "a", newExpiry); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if got, _ := s.Get(ctx, "a"); !got.ExpiresAt.Equal(newExpiry) {
		t.Errorf("ExpiresAt after Extend = %s, want %s", got.ExpiresAt, newExpiry)
	}

	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "a"); err != nil {
		t.Errorf("second Delete: %v, want nil", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}

func TestMemoryStore_ExtendMissing(t *testing.T) {
	s := newTestStore(t, clock.NewFake(t0), 10)
	if err := s.Extend(context.Background(), "nope", t0.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Extend of missing session: err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_Expiry(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	s := newTestStore(t, clk, 10)

	_ = s.Save(ctx, Session{ID: "short", ExpiresAt: t0.Add(time.Minute)})
	_ = s.Save(ctx, Session{ID: "long", ExpiresAt: t0.Add(time.Hour)})

	clk.Advance(time.Minute) // "short" expires exactly now

	if _, err := s.Get(ctx, "short"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session: err = %v, want ErrNotFound", err)
	}
	if err := s.Extend(ctx, "short", t0.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("Extend of expired session: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, "long"); err != nil {
		t.Errorf("live session: err = %v", err)
	}

	// Expired entries still take memory until the sweeper runs.
	if s.Len() != 2 {
		t.Fatalf("Len before sweep = %d, want 2", s.Len())
	}
	if removed := s.sweep(); removed != 1 {
		t.Errorf("sweep removed %d, want 1", removed)
	}
	if s.Len() != 1 {
		t.Errorf("Len after sweep = %d, want 1", s.Len())
	}
}

func TestMemoryStore_MaxItems(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	s := newTestStore(t, clk, 2)
	exp := t0.Add(time.Minute)

	_ = s.Save(ctx, Session{ID: "a", ExpiresAt: exp})
	_ = s.Save(ctx, Session{ID: "b", ExpiresAt: exp})
	if err := s.Save(ctx, Session{ID: "c", ExpiresAt: exp}); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("Save over limit: err = %v, want ErrStoreFull", err)
	}
	// Overwriting an existing ID does not need a new slot.
	if err := s.Save(ctx, Session{ID: "a", ExpiresAt: exp}); err != nil {
		t.Fatalf("overwrite at limit: %v", err)
	}

	// Space is reclaimed by Delete and by the sweeper.
	_ = s.Delete(ctx, "a")
	if err := s.Save(ctx, Session{ID: "c", ExpiresAt: exp}); err != nil {
		t.Fatalf("Save after Delete: %v", err)
	}
	clk.Advance(time.Minute)
	s.sweep()
	if err := s.Save(ctx, Session{ID: "d", ExpiresAt: t0.Add(time.Hour)}); err != nil {
		t.Fatalf("Save after sweep: %v", err)
	}
}

func TestMemoryStore_Concurrent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, clock.NewFake(t0), 1_000_000)
	exp := t0.Add(time.Hour)

	const workers, perWorker = 16, 500
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				id := fmt.Sprintf("%d-%d", w, i)
				_ = s.Save(ctx, Session{ID: id, ExpiresAt: exp})
				if _, err := s.Get(ctx, id); err != nil {
					t.Errorf("Get(%s): %v", id, err)
					return
				}
				_ = s.Extend(ctx, id, exp.Add(time.Minute))
				if i%2 == 0 {
					_ = s.Delete(ctx, id)
				}
			}
		}()
	}
	wg.Wait()

	if want := workers * perWorker / 2; s.Len() != want {
		t.Errorf("Len = %d, want %d", s.Len(), want)
	}
}

func TestMemoryStore_SweeperRunsInBackground(t *testing.T) {
	clk := clock.NewFake(t0)
	s := NewMemoryStore(clk, 10, 5*time.Millisecond)
	defer s.Close()

	_ = s.Save(context.Background(), Session{ID: "a", ExpiresAt: t0.Add(time.Second)})
	clk.Advance(time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for s.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("background sweeper did not remove the expired session")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMemoryStore_CloseIsIdempotent(t *testing.T) {
	s := NewMemoryStore(clock.Real{}, 10, time.Hour)
	s.Close()
	s.Close()
}
