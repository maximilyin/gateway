package ratelimit

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/maximilyin/gateway/internal/session"
)

const (
	headerLimit     = "X-RateLimit-Limit"
	headerRemaining = "X-RateLimit-Remaining"
)

// Limiter applies named policies. Global is one bucket for the whole process.
// Default is the per-client policy used when a route does not name one.
// SessionIssue limits how often one IP may create a session.
type Limiter struct {
	store  Store
	global Limit
	def    Limit
	issue  Limit
	named  map[string]Limit
	logger *slog.Logger
}

func New(store Store, global, def, issue Limit, named map[string]Limit, logger *slog.Logger) *Limiter {
	if named == nil {
		named = map[string]Limit{}
	}
	return &Limiter{store: store, global: global, def: def, issue: issue, named: named, logger: logger}
}

// AllowIssue spends one token from the per-IP session-creation bucket.
// On success it does not write a response. On failure it writes 429 or 503.
func (l *Limiter) AllowIssue(w http.ResponseWriter, r *http.Request) bool {
	return l.take(w, r, l.issue, "session_issue", "ip:"+clientIP(r), true)
}

// AllowGlobal spends one token from the single process-wide bucket.
func (l *Limiter) AllowGlobal(w http.ResponseWriter, r *http.Request) bool {
	return l.take(w, r, l.global, "global", "all", false)
}

// Session limits a proxied request by its session, then by the global bucket.
// The response headers describe the session bucket when the request is allowed,
// and the bucket that rejected it otherwise.
func (l *Limiter) Session(policy string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := session.FromContext(r.Context())
		if !ok {
			l.logger.ErrorContext(r.Context(), "rate limit missing session")
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		lim, name := l.policy(policy)
		if !l.take(w, r, lim, name, "session:"+sess.ID, true) {
			return
		}
		if !l.AllowGlobal(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IP limits a request that has no session (unknown paths, session info and revoke).
func (l *Limiter) IP(policy string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lim, name := l.policy(policy)
		if !l.take(w, r, lim, name, "ip:"+clientIP(r), true) {
			return
		}
		if !l.AllowGlobal(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *Limiter) policy(name string) (Limit, string) {
	if name == "" || name == "default" {
		return l.def, "default"
	}
	if lim, ok := l.named[name]; ok {
		return lim, name
	}
	return l.def, "default"
}

// take spends one token. headersOnSuccess writes the limit headers only when the
// request is allowed; a rejection always writes them.
func (l *Limiter) take(w http.ResponseWriter, r *http.Request, lim Limit, policy, id string, headersOnSuccess bool) bool {
	d, err := l.store.Allow(r.Context(), policy+"\x00"+id, lim)
	if err != nil {
		if !errors.Is(err, ErrStoreFull) {
			l.logger.ErrorContext(r.Context(), "rate limit store error", "error", err)
		} else {
			l.logger.WarnContext(r.Context(), "rate limit store is full")
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rate limiter unavailable"})
		return false
	}
	if !d.Allowed {
		writeLimited(w, d)
		return false
	}
	if headersOnSuccess {
		setLimitHeaders(w, d)
	}
	return true
}

func writeLimited(w http.ResponseWriter, d Decision) {
	setLimitHeaders(w, d)
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(d.RetryAfter)))
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
}

func setLimitHeaders(w http.ResponseWriter, d Decision) {
	remaining := math.Floor(d.Remaining)
	if remaining < 0 {
		remaining = 0
	}
	w.Header().Set(headerLimit, strconv.Itoa(int(d.Limit)))
	w.Header().Set(headerRemaining, strconv.Itoa(int(remaining)))
}

func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	if secs > 86400 {
		return 86400
	}
	return secs
}

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
