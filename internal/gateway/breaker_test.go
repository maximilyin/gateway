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

func TestGateway_CircuitBreakerSharedByRoutes(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var hits atomic.Int64
	var code atomic.Int64
	code.Store(http.StatusInternalServerError)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(code.Load()))
		_, _ = io.WriteString(w, "x")
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	widenRateLimits(cfg)
	cfg.Upstreams = map[string]config.UpstreamConfig{
		"svc": {
			URL:     upstream.URL,
			Timeout: time.Second,
			Breaker: config.BreakerConfig{FailureThreshold: 2, OpenTimeout: time.Minute, HalfOpenMax: 1},
		},
	}
	cfg.Routes = []config.RouteConfig{
		{Match: "/a/*", Upstream: "svc"},
		{Match: "/b/*", Upstream: "svc"},
	}
	gw := newTestGateway(t, cfg, WithClock(clk))

	if rec := call(gw, http.MethodGet, "/a/1", "", "203.0.113.1"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d", rec.Code)
	}
	if rec := call(gw, http.MethodGet, "/b/1", "", "203.0.113.1"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("second status = %d", rec.Code)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}

	// Both routes share the upstream breaker, so neither is forwarded.
	for _, path := range []string{"/a/2", "/b/2"} {
		rec := call(gw, http.MethodGet, path, "", "203.0.113.1")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503", path, rec.Code)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("open breaker forwarded a request, hits = %d", hits.Load())
	}

	code.Store(http.StatusOK)
	clk.Advance(time.Minute)
	token := call(gw, http.MethodGet, "/a/3", "", "203.0.113.1")
	if token.Code != http.StatusOK {
		t.Fatalf("probe status = %d, want 200 (%s)", token.Code, token.Body)
	}
	if hits.Load() != 3 {
		t.Fatalf("hits after probe = %d, want 3", hits.Load())
	}
	if rec := call(gw, http.MethodGet, "/b/3", token.Header().Get("X-Session-Token"), "203.0.113.1"); rec.Code != http.StatusOK {
		t.Fatalf("after close status = %d, want 200", rec.Code)
	}
}
