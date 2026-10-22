// Package ratelimit limits requests with an in-memory token bucket.
package ratelimit

import (
	"context"
	"errors"
	"time"
)

// ErrStoreFull means a new bucket cannot be created because the store is at its cap.
var ErrStoreFull = errors.New("rate limit store is full")

// Limit is a token bucket. Burst is the bucket size and also the number of
// requests that can pass at once. RPS is how many tokens return per second.
type Limit struct {
	RPS   float64
	Burst float64
}

// Decision is the outcome of taking one token.
type Decision struct {
	Allowed    bool
	Limit      float64
	Remaining  float64
	RetryAfter time.Duration
}

// Store decides whether one request identified by key may pass under limit.
// A key names both the policy and the client, so one client has a separate
// bucket per policy. Implementations must be safe for concurrent use.
type Store interface {
	Allow(ctx context.Context, key string, limit Limit) (Decision, error)
}
