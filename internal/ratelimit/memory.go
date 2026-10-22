package ratelimit

import (
	"context"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
)

const numShards = 64

type entry struct {
	tokens  float64
	updated time.Time
}

type shard struct {
	mu    sync.Mutex
	items map[string]entry
}

// MemoryStore is a sharded token-bucket store. Idle buckets are deleted by a
// background sweeper so one-off clients do not stay in memory forever.
// Deleting a bucket forgets its deficit: the next request starts from a full bucket.
type MemoryStore struct {
	shards [numShards]shard
	seed   maphash.Seed
	clock  clock.Clock

	maxKeys int64
	idle    time.Duration
	count   atomic.Int64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewMemoryStore starts the sweeper, which runs every cleanupInterval until Close.
func NewMemoryStore(clk clock.Clock, maxKeys int, idle, cleanupInterval time.Duration) *MemoryStore {
	s := &MemoryStore{
		seed:    maphash.MakeSeed(),
		clock:   clk,
		maxKeys: int64(maxKeys),
		idle:    idle,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for i := range s.shards {
		s.shards[i].items = make(map[string]entry)
	}
	go s.sweepLoop(cleanupInterval)
	return s
}

func (s *MemoryStore) shardFor(key string) *shard {
	return &s.shards[maphash.String(s.seed, key)%numShards]
}

func (s *MemoryStore) Allow(_ context.Context, key string, lim Limit) (Decision, error) {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := s.clock.Now()
	e, ok := sh.items[key]
	if !ok {
		if s.count.Add(1) > s.maxKeys {
			s.count.Add(-1)
			return Decision{}, ErrStoreFull
		}
		e = entry{tokens: lim.Burst, updated: now}
	} else if now.After(e.updated) {
		e.tokens += now.Sub(e.updated).Seconds() * lim.RPS
		if e.tokens > lim.Burst {
			e.tokens = lim.Burst
		}
		e.updated = now
	}

	d := Decision{Limit: lim.Burst, Remaining: e.tokens}
	if e.tokens >= 1 {
		e.tokens--
		d.Allowed = true
		d.Remaining = e.tokens
	} else {
		d.RetryAfter = retryAfter(1-e.tokens, lim.RPS)
	}
	sh.items[key] = e
	return d, nil
}

// retryAfter is how long it takes to refill deficit tokens, rounded up to a second.
func retryAfter(deficit, rps float64) time.Duration {
	secs := deficit / rps
	d := time.Duration(secs * float64(time.Second))
	if d%time.Second != 0 {
		d += time.Second - d%time.Second
	}
	if d < time.Second {
		d = time.Second
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d
}

// Len returns the number of buckets, including idle ones not swept yet.
func (s *MemoryStore) Len() int {
	return int(s.count.Load())
}

// Close stops the sweeper and waits for it to exit. It is safe to call twice.
func (s *MemoryStore) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
}

func (s *MemoryStore) sweepLoop(interval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.sweep()
		case <-s.stop:
			return
		}
	}
}

func (s *MemoryStore) sweep() int {
	cutoff := s.clock.Now().Add(-s.idle)
	removed := 0
	for i := range s.shards {
		sh := &s.shards[i]
		n := 0
		sh.mu.Lock()
		for key, e := range sh.items {
			if !e.updated.After(cutoff) {
				delete(sh.items, key)
				n++
			}
		}
		sh.mu.Unlock()
		s.count.Add(int64(-n))
		removed += n
	}
	return removed
}
