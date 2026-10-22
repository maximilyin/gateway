package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
	"github.com/maximilyin/gateway/internal/config"
)

func limitConfig(t *testing.T, clk clock.Clock) (*config.Config, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Sessions.TTL = time.Hour
	cfg.Sessions.MaxLifetime = 2 * time.Hour
	cfg.RateLimits.Global = config.RateLimitConfig{RPS: 100, Burst: 100}
	cfg.RateLimits.Default = config.RateLimitConfig{RPS: 1, Burst: 2}
	cfg.RateLimits.SessionIssue = config.RateLimitConfig{RPS: 1, Burst: 10}
	cfg.RateLimits.Policies = map[string]config.RateLimitConfig{
		"login": {RPS: 1, Burst: 1},
	}
	cfg.Upstreams = map[string]config.UpstreamConfig{
		"svc": {URL: srv.URL, Timeout: time.Second},
	}
	cfg.Routes = []config.RouteConfig{
		{Match: "/login", Methods: []string{"POST"}, Upstream: "svc", RateLimit: "login"},
		{Match: "/api/*", Upstream: "svc"},
	}
	return cfg, &hits
}

func call(gw *Gateway, method, path, token, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = ip + ":4000"
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	return rec
}

func TestGateway_SessionRateLimitRefills(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg, hits := limitConfig(t, clk)
	gw := newTestGateway(t, cfg, WithClock(clk))

	first := call(gw, http.MethodGet, "/api/x", "", "203.0.113.1")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200 (%s)", first.Code, first.Body)
	}
	token := first.Header().Get("X-Session-Token")
	if token == "" {
		t.Fatal("first response has no session token")
	}
	if first.Header().Get("X-RateLimit-Limit") != "2" {
		t.Errorf("limit header = %q, want 2", first.Header().Get("X-RateLimit-Limit"))
	}

	if rec := call(gw, http.MethodGet, "/api/x", token, "203.0.113.1"); rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", rec.Code)
	}
	blocked := call(gw, http.MethodGet, "/api/x", token, "203.0.113.1")
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("third status = %d, want 429", blocked.Code)
	}
	if blocked.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", blocked.Header().Get("Retry-After"))
	}
	if hits.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2", hits.Load())
	}

	clk.Advance(time.Second)
	if rec := call(gw, http.MethodGet, "/api/x", token, "203.0.113.1"); rec.Code != http.StatusOK {
		t.Fatalf("after refill status = %d, want 200", rec.Code)
	}
}

func TestGateway_SessionsHaveSeparateBuckets(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg, _ := limitConfig(t, clk)
	gw := newTestGateway(t, cfg, WithClock(clk))

	a := call(gw, http.MethodGet, "/api/x", "", "203.0.113.1")
	b := call(gw, http.MethodGet, "/api/y", "", "203.0.113.2")
	tokenA := a.Header().Get("X-Session-Token")
	tokenB := b.Header().Get("X-Session-Token")

	// Burst is 2. Each session already spent 1, so each has one left.
	if rec := call(gw, http.MethodGet, "/api/x", tokenA, "203.0.113.1"); rec.Code != http.StatusOK {
		t.Fatalf("session A second request: %d", rec.Code)
	}
	if rec := call(gw, http.MethodGet, "/api/x", tokenA, "203.0.113.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("session A third request: %d, want 429", rec.Code)
	}
	if rec := call(gw, http.MethodGet, "/api/y", tokenB, "203.0.113.2"); rec.Code != http.StatusOK {
		t.Fatalf("session B was limited by session A: %d", rec.Code)
	}
}

func TestGateway_NamedPolicyAndGlobalGuard(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg, hits := limitConfig(t, clk)
	cfg.RateLimits.Global = config.RateLimitConfig{RPS: 1, Burst: 2}
	cfg.RateLimits.Default = config.RateLimitConfig{RPS: 100, Burst: 100}
	gw := newTestGateway(t, cfg, WithClock(clk))

	login := call(gw, http.MethodPost, "/login", "", "203.0.113.1")
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (%s)", login.Code, login.Body)
	}
	if login.Header().Get("X-RateLimit-Limit") != "1" {
		t.Errorf("login limit header = %q, want the named policy burst 1", login.Header().Get("X-RateLimit-Limit"))
	}
	token := login.Header().Get("X-Session-Token")
	again := call(gw, http.MethodPost, "/login", token, "203.0.113.1")
	if again.Code != http.StatusTooManyRequests {
		t.Fatalf("second login = %d, want 429", again.Code)
	}

	// Login spent the first global token. The default policy is wide, so the next
	// API call spends the second and the one after that is stopped by the global burst.
	if rec := call(gw, http.MethodGet, "/api/x", token, "203.0.113.1"); rec.Code != http.StatusOK {
		t.Fatalf("api status = %d, want 200", rec.Code)
	}
	blocked := call(gw, http.MethodGet, "/api/x", token, "203.0.113.1")
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("X-RateLimit-Limit") != "2" {
		t.Fatalf("global guard: status %d limit %q, want 429 and limit 2", blocked.Code, blocked.Header().Get("X-RateLimit-Limit"))
	}
	if hits.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2", hits.Load())
	}
}

func TestGateway_SessionIssueIsLimitedByIP(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg, hits := limitConfig(t, clk)
	cfg.RateLimits.SessionIssue = config.RateLimitConfig{RPS: 0.01, Burst: 1}
	gw := newTestGateway(t, cfg, WithClock(clk))

	if rec := call(gw, http.MethodGet, "/api/x", "", "203.0.113.8"); rec.Code != http.StatusOK {
		t.Fatalf("first issue status = %d, want 200", rec.Code)
	}
	blocked := call(gw, http.MethodGet, "/api/x", "", "203.0.113.8")
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("second issue from same IP = %d, want 429", blocked.Code)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
	// Another address still gets a session.
	if rec := call(gw, http.MethodGet, "/api/x", "", "203.0.113.9"); rec.Code != http.StatusOK {
		t.Fatalf("other IP status = %d, want 200", rec.Code)
	}
}

func TestGateway_UnknownPathIsLimitedWithoutSession(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg, _ := limitConfig(t, clk)
	cfg.RateLimits.Default = config.RateLimitConfig{RPS: 1, Burst: 1}
	gw := newTestGateway(t, cfg, WithClock(clk))

	if gw.sessions.Len() != 0 {
		t.Fatal("store is not empty")
	}
	if rec := call(gw, http.MethodGet, "/missing", "", "203.0.113.1"); rec.Code != http.StatusNotFound {
		t.Fatalf("first miss = %d, want 404", rec.Code)
	}
	if rec := call(gw, http.MethodGet, "/missing", "", "203.0.113.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second miss = %d, want 429", rec.Code)
	}
	if gw.sessions.Len() != 0 {
		t.Errorf("404 created %d sessions", gw.sessions.Len())
	}
	if rec := call(gw, http.MethodGet, "/healthz", "", "203.0.113.1"); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 while the IP is limited", rec.Code)
	}
}
