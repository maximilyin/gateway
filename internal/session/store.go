// Package session issues opaque session tokens, stores sessions and attaches the
// current session to requests.
package session

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound  = errors.New("session not found")
	ErrStoreFull = errors.New("session store is full")
)

// Session is what the gateway knows about a client. ID is the SHA-256 of the
// token, so it is safe to log and to forward upstream; the token itself is only
// ever returned to the client once.
type Session struct {
	ID        string
	ClientIP  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Store persists sessions. Implementations treat expired sessions as absent.
// Methods take a context and return errors so that a networked store (Redis)
// can implement the same interface.
type Store interface {
	// Save inserts a new session, or returns ErrStoreFull.
	Save(ctx context.Context, s Session) error
	// Get returns the session or ErrNotFound.
	Get(ctx context.Context, id string) (Session, error)
	// Extend sets a new expiry on an existing session, or returns ErrNotFound.
	// Unlike Save it never re-creates a session that was deleted concurrently.
	Extend(ctx context.Context, id string, expiresAt time.Time) error
	// Delete removes the session; deleting a missing session is not an error.
	Delete(ctx context.Context, id string) error
}
