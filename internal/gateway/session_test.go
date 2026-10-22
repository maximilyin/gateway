package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
	"github.com/maximilyin/gateway/internal/config"
	"github.com/maximilyin/gateway/internal/session"
)

type upstreamSeen struct {
	sessionID, token string
}

func sessionGateway(t *testing.T, clk clock.Clock) (*Gateway, <-chan upstreamSeen) {
	t.Helper()
	seen := make(chan upstreamSeen, 10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- upstreamSeen{
			sessionID: r.Header.Get(session.IDHeader),
			token:     r.Header.Get("X-Session-Token"),
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.Sessions.TTL = 10 * time.Minute
	cfg.Sessions.MaxLifetime = time.Hour
	cfg.Upstreams = map[string]config.UpstreamConfig{
		"svc": {URL: upstream.URL, Timeout: time.Second},
	}
	cfg.Routes = []config.RouteConfig{
		{Match: "/private/*", Upstream: "svc", RequireSession: true},
		{Match: "/public/*", Upstream: "svc"},
	}
	widenRateLimits(cfg)
	return newTestGateway(t, cfg, WithClock(clk)), seen
}

type issued struct {
	Token     string    `json:"token"`
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func issueSession(t *testing.T, gw *Gateway) issued {
	t.Helper()
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/session", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	var got issued
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func get(gw *Gateway, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	return rec
}

func TestGateway_FirstRequestIssuesSession(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	gw, seen := sessionGateway(t, clk)

	rec := get(gw, "/private/data", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: status %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	token := rec.Header().Get("X-Session-Token")
	if token == "" {
		t.Fatal("first response has no session token")
	}
	up := <-seen
	if up.token != "" {
		t.Errorf("token leaked to upstream: %q", up.token)
	}
	if up.sessionID == "" {
		t.Fatal("upstream got no X-Session-ID")
	}

	// The client sends the token back and stays on the same session.
	// A live session is not replaced, so the response carries no new token.
	rec = get(gw, "/private/data", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("second request: status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Session-Token"); got != "" {
		t.Errorf("second response issued a new token %q", got)
	}
	up2 := <-seen
	if up2.sessionID != up.sessionID {
		t.Errorf("session changed: first %q, second %q", up.sessionID, up2.sessionID)
	}
}

func TestGateway_ExplicitSessionEndpoints(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	gw, seen := sessionGateway(t, clk)

	s := issueSession(t, gw)
	if s.Token == "" || s.SessionID == "" {
		t.Fatalf("issued session is incomplete: %+v", s)
	}

	if rec := get(gw, "/private/data", s.Token); rec.Code != http.StatusOK {
		t.Fatalf("private route with session: status %d, want 200", rec.Code)
	}
	if up := <-seen; up.sessionID != s.SessionID || up.token != "" {
		t.Errorf("upstream saw %+v, want session %s and no token", up, s.SessionID)
	}

	if rec := get(gw, "/auth/session", s.Token); rec.Code != http.StatusOK {
		t.Fatalf("session info: status %d, want 200", rec.Code)
	}

	req := httptest.NewRequest(http.MethodDelete, "/auth/session", nil)
	req.Header.Set("X-Session-Token", s.Token)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d, want 204", rec.Code)
	}
	// The explicit info endpoint still rejects the dead token.
	if rec := get(gw, "/auth/session", s.Token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("info after revoke: status %d, want 401", rec.Code)
	}
	// An API route replaces it and still proxies the request.
	rec = get(gw, "/private/data", s.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("api after revoke: status %d, want 200", rec.Code)
	}
	newToken := rec.Header().Get("X-Session-Token")
	if newToken == "" || newToken == s.Token {
		t.Fatalf("replacement token = %q, want a new one", newToken)
	}
	if up := <-seen; up.sessionID == s.SessionID {
		t.Error("revoked session was reused")
	}
}

func TestGateway_ExpiredTokenIsReplaced(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	gw, seen := sessionGateway(t, clk)
	s := issueSession(t, gw)

	clk.Advance(10 * time.Minute)
	rec := get(gw, "/private/data", s.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("after TTL: status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Session-Token"); got == "" || got == s.Token {
		t.Fatalf("replacement token = %q, want a new one", got)
	}
	if up := <-seen; up.sessionID == s.SessionID {
		t.Error("expired session was reused")
	}
}

func TestGateway_UnknownPathDoesNotIssueSession(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	gw, _ := sessionGateway(t, clk)

	rec := get(gw, "/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if rec.Header().Get("X-Session-Token") != "" {
		t.Error("404 response issued a session token")
	}
	if gw.sessions.Len() != 0 {
		t.Errorf("store Len = %d, want 0", gw.sessions.Len())
	}
}

func TestGateway_PublicRouteStripsSpoofedSessionID(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	gw, seen := sessionGateway(t, clk)

	req := httptest.NewRequest(http.MethodGet, "/public/x", nil)
	req.Header.Set(session.IDHeader, "spoofed")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	token := rec.Header().Get("X-Session-Token")
	if token == "" {
		t.Fatal("first public request did not issue a token")
	}
	up := <-seen
	if up.sessionID == "" || up.sessionID == "spoofed" {
		t.Errorf("upstream X-Session-ID = %q, want a gateway-issued id", up.sessionID)
	}
}
