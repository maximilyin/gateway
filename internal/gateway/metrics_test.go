package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
	"github.com/maximilyin/gateway/internal/config"
)

func TestGateway_MetricsShowLimitAndBreaker(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "x")
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.RateLimits.Global = config.RateLimitConfig{RPS: 100, Burst: 100}
	cfg.RateLimits.Default = config.RateLimitConfig{RPS: 1, Burst: 2}
	cfg.RateLimits.SessionIssue = config.RateLimitConfig{RPS: 1, Burst: 10}
	cfg.Upstreams = map[string]config.UpstreamConfig{
		"svc": {
			URL:     upstream.URL,
			Timeout: time.Second,
			Breaker: config.BreakerConfig{FailureThreshold: 1, OpenTimeout: time.Minute, HalfOpenMax: 1},
		},
	}
	cfg.Routes = []config.RouteConfig{{Match: "/api/*", Upstream: "svc"}}
	gw := newTestGateway(t, cfg, WithClock(clk))

	first := call(gw, http.MethodGet, "/api/x", "", "203.0.113.8")
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500", first.Code)
	}
	token := first.Header().Get("X-Session-Token")
	// The open breaker still spends a rate-limit token, so this request is the
	// second of the burst and is refused before the upstream is called.
	refused := call(gw, http.MethodGet, "/api/y", token, "203.0.113.8")
	if refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, want 503 (breaker); body %s", refused.Code, refused.Body)
	}
	blocked := call(gw, http.MethodGet, "/api/x", token, "203.0.113.8")
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("third status = %d, want 429", blocked.Code)
	}

	body := call(gw, http.MethodGet, "/metrics", "", "203.0.113.8").Body.String()
	for _, want := range []string{
		`gateway_rate_limit_rejected_total{policy="default"} 1`,
		`gateway_circuit_breaker_state{upstream="svc"} 2`,
		`gateway_circuit_breaker_rejected_total{upstream="svc"} 1`,
		"gateway_sessions 1",
		`gateway_http_responses_total{code="429"} 1`,
		`gateway_http_responses_total{code="503"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}
