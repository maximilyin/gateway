package session

import (
	"context"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
)

// numShards splits the store into independently locked maps, so concurrent
// requests for different sessions rarely wait on the same mutex.
const numShards = 64

type shard struct {
	mu    sync.Mutex
	items map[string]Session
}

// MemoryStore is an in-process Store: a sharded map with TTL and a background
// sweeper that drops expired sessions. Data is lost on restart and is not shared
// between gateway replicas.
type MemoryStore struct {
	shards [numShards]shard
	seed   maphash.Seed
	clock  clock.Clock

	maxItems int64
	count    atomic.Int64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewMemoryStore creates the store and starts its sweeper goroutine, which runs
// every cleanupInterval until Close is called.
func NewMemoryStore(clk clock.Clock, maxItems int, cleanupInterval time.Duration) *MemoryStore {
	s := &MemoryStore{
		seed:     maphash.MakeSeed(),
		clock:    clk,
		maxItems: int64(maxItems),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	for i := range s.shards {
		s.shards[i].items = make(map[string]Session)
	}

	go s.sweepLoop(cleanupInterval)
	return s
}

// shardFor picks a shard by hashing the ID. maphash uses a random per-process
// seed, so clients cannot craft IDs that all land in one shard.
func (s *MemoryStore) shardFor(id string) *shard {
	return &s.shards[maphash.String(s.seed, id)%numShards]
}

func (s *MemoryStore) Save(_ context.Context, sess Session) error {
	sh := s.shardFor(sess.ID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if _, exists := sh.items[sess.ID]; !exists {
		// Reserve a slot first and roll back if over the limit, so two concurrent
		// Saves cannot both slip past the check.
		if s.count.Add(1) > s.maxItems {
			s.count.Add(-1)
			return ErrStoreFull
		}
	}
	sh.items[sess.ID] = sess
	return nil
}

func (s *MemoryStore) Get(_ context.Context, id string) (Session, error) {
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	sess, ok := sh.items[id]
	if !ok || s.expired(sess) {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *MemoryStore) Extend(_ context.Context, id string, expiresAt time.Time) error {
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	sess, ok := sh.items[id]
	if !ok || s.expired(sess) {
		return ErrNotFound
	}
	sess.ExpiresAt = expiresAt
	sh.items[id] = sess
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, id string) error {
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if _, ok := sh.items[id]; ok {
		delete(sh.items, id)
		s.count.Add(-1)
	}
	return nil
}

// Len returns the number of stored sessions, including expired ones that the
// sweeper has not removed yet.
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

func (s *MemoryStore) expired(sess Session) bool {
	return !s.clock.Now().Before(sess.ExpiresAt)
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

// sweep removes expired sessions one shard at a time, so it never blocks the
// whole store, only 1/numShards of it, and only briefly.
func (s *MemoryStore) sweep() int {
	now := s.clock.Now()
	removed := 0
	for i := range s.shards {
		sh := &s.shards[i]
		n := 0
		sh.mu.Lock()
		for id, sess := range sh.items {
			if !now.Before(sess.ExpiresAt) {
				delete(sh.items, id)
				n++
			}
		}
		sh.mu.Unlock()
		s.count.Add(int64(-n))
		removed += n
	}
	return removed
}
