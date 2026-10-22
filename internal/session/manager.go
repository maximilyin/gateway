package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
)

// ErrInvalidToken means the token is malformed, unknown or expired. The cases are
// deliberately indistinguishable to the client.
var ErrInvalidToken = errors.New("invalid or expired session token")

const tokenBytes = 32

// tokenLen is the length of a token encoded with unpadded base64url.
var tokenLen = base64.RawURLEncoding.EncodedLen(tokenBytes)

type Config struct {
	// TTL is the idle timeout: a session unused for this long expires. Every use
	// extends it (sliding expiration).
	TTL time.Duration
	// MaxLifetime caps a session's total age no matter how actively it is used.
	MaxLifetime time.Duration
}

type Manager struct {
	store Store
	clock clock.Clock
	cfg   Config
}

func NewManager(store Store, clk clock.Clock, cfg Config) *Manager {
	return &Manager{store: store, clock: clk, cfg: cfg}
}

// Issue creates a session and returns the token the client must present.
func (m *Manager) Issue(ctx context.Context, clientIP string) (token string, s Session, err error) {
	var raw [tokenBytes]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read never returns an error
	token = base64.RawURLEncoding.EncodeToString(raw[:])

	now := m.clock.Now()
	s = Session{
		ID:        hashToken(token),
		ClientIP:  clientIP,
		CreatedAt: now,
		ExpiresAt: m.capExpiry(now, now.Add(m.cfg.TTL)),
	}
	if err := m.store.Save(ctx, s); err != nil {
		return "", Session{}, err
	}
	return token, s, nil
}

// Authenticate resolves a token to its session and slides its expiry forward.
func (m *Manager) Authenticate(ctx context.Context, token string) (Session, error) {
	if !validTokenFormat(token) {
		return Session{}, ErrInvalidToken
	}

	s, err := m.store.Get(ctx, hashToken(token))
	if errors.Is(err, ErrNotFound) {
		return Session{}, ErrInvalidToken
	}
	if err != nil {
		return Session{}, err
	}

	if err := m.refresh(ctx, &s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// Revoke deletes the session behind the token.
func (m *Manager) Revoke(ctx context.Context, token string) error {
	if !validTokenFormat(token) {
		return ErrInvalidToken
	}
	return m.store.Delete(ctx, hashToken(token))
}

// refresh extends the session to now+TTL, but only once less than half of the
// TTL is left. Writing on every request would turn each read into a write, which
// matters for a networked store.
func (m *Manager) refresh(ctx context.Context, s *Session) error {
	now := m.clock.Now()
	if s.ExpiresAt.Sub(now) >= m.cfg.TTL/2 {
		return nil
	}

	newExpiry := m.capExpiry(s.CreatedAt, now.Add(m.cfg.TTL))
	if !newExpiry.After(s.ExpiresAt) {
		return nil // already at MaxLifetime
	}

	err := m.store.Extend(ctx, s.ID, newExpiry)
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidToken // expired or revoked between Get and Extend
	}
	if err != nil {
		return err
	}
	s.ExpiresAt = newExpiry
	return nil
}

func (m *Manager) capExpiry(createdAt, expiry time.Time) time.Time {
	if hardLimit := createdAt.Add(m.cfg.MaxLifetime); expiry.After(hardLimit) {
		return hardLimit
	}
	return expiry
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// validTokenFormat rejects anything that cannot be a token we issued before
// hashing it, so junk headers cost nothing.
func validTokenFormat(token string) bool {
	if len(token) != tokenLen {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
