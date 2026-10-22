package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// IDHeader carries the session ID to upstream services. Any value sent by the
// client is removed, so upstreams can trust it.
const IDHeader = "X-Session-ID"

// HTTP exposes a Manager over HTTP: the session endpoints and the middleware that
// resolves the session of each proxied request.
type HTTP struct {
	manager     *Manager
	tokenHeader string
	logger      *slog.Logger
}

func NewHTTP(m *Manager, tokenHeader string, logger *slog.Logger) *HTTP {
	return &HTTP{manager: m, tokenHeader: tokenHeader, logger: logger}
}

type sessionResponse struct {
	Token     string    `json:"token,omitempty"`
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Issue handles POST: creates a session and returns its token (201).
func (h *HTTP) Issue(w http.ResponseWriter, r *http.Request) {
	token, s, err := h.manager.Issue(r.Context(), clientIP(r))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sessionResponse{
		Token:     token,
		SessionID: s.ID,
		ExpiresAt: s.ExpiresAt,
	})
}

// Info handles GET: describes the session of the presented token.
func (h *HTTP) Info(w http.ResponseWriter, r *http.Request) {
	s, err := h.manager.Authenticate(r.Context(), r.Header.Get(h.tokenHeader))
	if err != nil {
		h.authError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{SessionID: s.ID, ExpiresAt: s.ExpiresAt})
}

// Revoke handles DELETE: ends the session of the presented token (204).
func (h *HTTP) Revoke(w http.ResponseWriter, r *http.Request) {
	if err := h.manager.Revoke(r.Context(), r.Header.Get(h.tokenHeader)); err != nil {
		h.authError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Attach guarantees a live session for the request, then calls next.
//
// A missing, unknown or expired token is not an error: a new session is issued
// (the same operation as POST /auth/session) and the request continues with it.
// The new token is written to the response under the same header name the client
// sends on later requests. A still-valid token is reused and is not echoed back.
// The raw token is never forwarded upstream; X-Session-ID replaces it.
func (h *HTTP) Attach(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(h.tokenHeader)
		r.Header.Del(h.tokenHeader)
		r.Header.Del(IDHeader)

		var (
			sess   Session
			issued string
			err    error
		)
		if token != "" {
			sess, err = h.manager.Authenticate(r.Context(), token)
			if err != nil && !errors.Is(err, ErrInvalidToken) {
				h.storeError(w, r, err)
				return
			}
		}
		if sess.ID == "" {
			issued, sess, err = h.manager.Issue(r.Context(), clientIP(r))
			if err != nil {
				h.storeError(w, r, err)
				return
			}
			// Set before next runs: ReverseProxy adds upstream headers, it does not
			// clear ones already on the writer.
			w.Header().Set(h.tokenHeader, issued)
		}

		r.Header.Set(IDHeader, sess.ID)
		next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), sess)))
	})
}

// Require rejects requests that Attach did not associate with a session.
func Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "session required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type contextKey struct{}

func NewContext(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// FromContext returns the session attached to the request, if any.
func FromContext(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(contextKey{}).(Session)
	return s, ok
}

func (h *HTTP) authError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrInvalidToken) {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: ErrInvalidToken.Error()})
		return
	}
	h.storeError(w, r, err)
}

func (h *HTTP) storeError(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, ErrStoreFull) {
		h.logger.ErrorContext(r.Context(), "session store error", "error", err)
	} else {
		h.logger.WarnContext(r.Context(), "session store is full")
	}
	writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "sessions temporarily unavailable"})
}

// clientIP returns the address of the direct peer. Honouring X-Forwarded-For
// from trusted proxies comes with the rate limiter.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
