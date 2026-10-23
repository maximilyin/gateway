// Package breaker stops calling an upstream that is failing, then lets a few
// requests through to see whether it has recovered.
package breaker

import (
	"log/slog"
	"sync"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
)

type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Result tells the breaker how an attempt that was allowed to proceed ended.
type Result int

const (
	// Success is a response from the upstream, including a 4xx: the service is up.
	Success Result = iota
	// Failure is a 5xx, a timeout, or a connection error.
	Failure
	// Ignored is a client-side outcome, such as a canceled request or a body
	// the gateway rejected itself. It must not move the breaker.
	Ignored
)

type Settings struct {
	// FailureThreshold is how many consecutive failures open the breaker.
	FailureThreshold int
	// OpenTimeout is how long the breaker stays open before a probe is allowed.
	OpenTimeout time.Duration
	// HalfOpenMax is how many probes may be in flight, and how many successes
	// are required to close the breaker. Any probe failure opens it again.
	HalfOpenMax int
}

// Permit is the right to forward one request. Report it exactly once.
type Permit struct {
	gen int
}

type Breaker struct {
	mu     sync.Mutex
	clock  clock.Clock
	cfg    Settings
	logger *slog.Logger

	state     State
	gen       int
	failures  int
	openedAt  time.Time
	inflight  int
	successes int
}

func New(cfg Settings, clk clock.Clock, logger *slog.Logger) *Breaker {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Breaker{clock: clk, cfg: cfg, logger: logger, state: Closed}
}

// State returns the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance(b.clock.Now())
	return b.state
}

// Allow reports whether this request may be forwarded. When it is refused, retry
// is how long the caller should wait before trying again.
func (b *Breaker) Allow() (permit Permit, retry time.Duration, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	b.advance(now)

	switch b.state {
	case Open:
		return Permit{}, b.remain(now), false
	case HalfOpen:
		if b.inflight >= b.cfg.HalfOpenMax {
			return Permit{}, time.Second, false
		}
		b.inflight++
		return Permit{gen: b.gen}, 0, true
	default:
		return Permit{gen: b.gen}, 0, true
	}
}

// Report finishes an attempt that Allow accepted.
func (b *Breaker) Report(permit Permit, result Result) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if result == Ignored {
		if b.state == HalfOpen && permit.gen == b.gen && b.inflight > 0 {
			b.inflight--
		}
		return
	}
	// A probe from a previous generation finished after the state already moved.
	if permit.gen != b.gen {
		return
	}

	now := b.clock.Now()
	switch b.state {
	case HalfOpen:
		if b.inflight > 0 {
			b.inflight--
		}
		if result == Failure {
			b.trip(now)
			return
		}
		b.successes++
		if b.successes >= b.cfg.HalfOpenMax {
			b.close()
		}
	default:
		if result == Failure {
			b.failures++
			if b.failures >= b.cfg.FailureThreshold {
				b.trip(now)
			}
			return
		}
		b.failures = 0
	}
}

// advance moves open to half-open once OpenTimeout has elapsed.
func (b *Breaker) advance(now time.Time) {
	if b.state == Open && !now.Before(b.openedAt.Add(b.cfg.OpenTimeout)) {
		b.state = HalfOpen
		b.gen++
		b.inflight = 0
		b.successes = 0
		b.logger.Info("circuit half-open")
	}
}

func (b *Breaker) trip(now time.Time) {
	b.state = Open
	b.gen++
	b.failures = 0
	b.inflight = 0
	b.successes = 0
	b.openedAt = now
	b.logger.Warn("circuit open", "open_timeout", b.cfg.OpenTimeout.String())
}

func (b *Breaker) close() {
	b.state = Closed
	b.gen++
	b.failures = 0
	b.inflight = 0
	b.successes = 0
	b.logger.Info("circuit closed")
}

func (b *Breaker) remain(now time.Time) time.Duration {
	d := b.openedAt.Add(b.cfg.OpenTimeout).Sub(now)
	if d < time.Second {
		return time.Second
	}
	return d
}
