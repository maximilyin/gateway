package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
)

func newTestManager(t *testing.T, ttl, maxLifetime time.Duration) (*Manager, *clock.Fake, *MemoryStore) {
	t.Helper()
	clk := clock.NewFake(t0)
	store := newTestStore(t, clk, 100)
	return NewManager(store, clk, Config{TTL: ttl, MaxLifetime: maxLifetime}), clk, store
}

func TestManager_IssueAndAuthenticate(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newTestManager(t, 10*time.Minute, time.Hour)

	token, s, err := m.Issue(ctx, "203.0.113.7")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(token) != tokenLen {
		t.Errorf("token length = %d, want %d", len(token), tokenLen)
	}
	if s.ID == token || strings.Contains(s.ID, token) {
		t.Error("session ID must be a hash of the token, not the token itself")
	}
	if s.ClientIP != "203.0.113.7" || !s.CreatedAt.Equal(t0) || !s.ExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("unexpected session %+v", s)
	}

	got, err := m.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != s.ID {
		t.Errorf("Authenticate returned session %q, want %q", got.ID, s.ID)
	}
}

func TestManager_TokensAreUnique(t *testing.T) {
	m, _, _ := newTestManager(t, time.Minute, time.Hour)
	seen := make(map[string]bool)
	for range 50 {
		token, _, err := m.Issue(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatalf("duplicate token %q", token)
		}
		seen[token] = true
	}
}

func TestManager_RejectsBadTokens(t *testing.T) {
	m, _, _ := newTestManager(t, time.Minute, time.Hour)
	unknown := strings.Repeat("A", tokenLen)

	for name, token := range map[string]string{
		"empty":         "",
		"too short":     "abc",
		"too long":      strings.Repeat("A", tokenLen+1),
		"bad character": strings.Repeat("A", tokenLen-1) + "=",
		"unknown":       unknown,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Authenticate(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestManager_SlidingExpiration(t *testing.T) {
	ctx := context.Background()
	m, clk, store := newTestManager(t, 10*time.Minute, time.Hour)
	token, s, _ := m.Issue(ctx, "")

	// With more than half of the TTL left, nothing is written.
	clk.Advance(4 * time.Minute)
	got, err := m.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Errorf("expiry changed too early: %s, want %s", got.ExpiresAt, s.ExpiresAt)
	}

	// Past the half-way mark the expiry slides to now+TTL.
	clk.Advance(2 * time.Minute) // t0+6m, 4m left
	got, err = m.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	want := t0.Add(16 * time.Minute)
	if !got.ExpiresAt.Equal(want) {
		t.Errorf("expiry after refresh = %s, want %s", got.ExpiresAt, want)
	}
	if stored, _ := store.Get(ctx, s.ID); !stored.ExpiresAt.Equal(want) {
		t.Errorf("stored expiry = %s, want %s", stored.ExpiresAt, want)
	}

	// The session outlives its original TTL thanks to the refresh.
	clk.Advance(9 * time.Minute) // t0+15m
	if _, err := m.Authenticate(ctx, token); err != nil {
		t.Errorf("session expired despite activity: %v", err)
	}
}

func TestManager_IdleSessionExpires(t *testing.T) {
	ctx := context.Background()
	m, clk, _ := newTestManager(t, 10*time.Minute, time.Hour)
	token, _, _ := m.Issue(ctx, "")

	clk.Advance(10 * time.Minute)
	if _, err := m.Authenticate(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("idle session: err = %v, want ErrInvalidToken", err)
	}
}

func TestManager_MaxLifetime(t *testing.T) {
	ctx := context.Background()
	m, clk, _ := newTestManager(t, 10*time.Minute, 15*time.Minute)
	token, _, _ := m.Issue(ctx, "")

	// Keep the session busy; refreshes may not push it past CreatedAt+15m.
	for range 4 {
		clk.Advance(3 * time.Minute)
		s, err := m.Authenticate(ctx, token)
		if err != nil {
			t.Fatalf("at %s: %v", clk.Now().Sub(t0), err)
		}
		if limit := t0.Add(15 * time.Minute); s.ExpiresAt.After(limit) {
			t.Fatalf("expiry %s is past max lifetime %s", s.ExpiresAt, limit)
		}
	}

	clk.Advance(3 * time.Minute) // t0+15m
	if _, err := m.Authenticate(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("after max lifetime: err = %v, want ErrInvalidToken", err)
	}
}

func TestManager_Revoke(t *testing.T) {
	ctx := context.Background()
	m, _, store := newTestManager(t, time.Minute, time.Hour)
	token, _, _ := m.Issue(ctx, "")

	if err := m.Revoke(ctx, token); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := m.Authenticate(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("after revoke: err = %v, want ErrInvalidToken", err)
	}
	if store.Len() != 0 {
		t.Errorf("store Len = %d, want 0", store.Len())
	}
	if err := m.Revoke(ctx, "garbage"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Revoke(garbage): err = %v, want ErrInvalidToken", err)
	}
}

func TestManager_StoreFull(t *testing.T) {
	clk := clock.NewFake(t0)
	store := newTestStore(t, clk, 1)
	m := NewManager(store, clk, Config{TTL: time.Minute, MaxLifetime: time.Hour})

	if _, _, err := m.Issue(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Issue(context.Background(), ""); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("err = %v, want ErrStoreFull", err)
	}
}
