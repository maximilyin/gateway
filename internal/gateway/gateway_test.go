package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximilyin/gateway/internal/config"
	"github.com/maximilyin/gateway/internal/middleware"
)

func newTestGateway(t *testing.T, upstreamURL string) *Gateway {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.URL = upstreamURL
	cfg.Server.MaxBodyBytes = 16
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	gw, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

func TestGateway_HealthEndpoints(t *testing.T) {
	gw := newTestGateway(t, "http://127.0.0.1:1")

	tests := []struct {
		name   string
		path   string
		ready  bool
		status int
	}{
		{name: "healthz", path: "/healthz", ready: true, status: http.StatusOK},
		{name: "readyz when ready", path: "/readyz", ready: true, status: http.StatusOK},
		{name: "readyz when shutting down", path: "/readyz", ready: false, status: http.StatusServiceUnavailable},
		{name: "healthz ignores readiness", path: "/healthz", ready: false, status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw.SetReady(tt.ready)
			rec := httptest.NewRecorder()
			gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
		})
	}
}

func TestGateway_ProxiesWithRequestID(t *testing.T) {
	gotID := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID <- r.Header.Get(middleware.RequestIDHeader)
		_, _ = io.WriteString(w, "from upstream")
	}))
	defer upstream.Close()

	gw := newTestGateway(t, upstream.URL)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/service/users/1", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "from upstream" {
		t.Fatalf("got %d %q, want 200 %q", rec.Code, rec.Body.String(), "from upstream")
	}
	respID := rec.Header().Get(middleware.RequestIDHeader)
	if respID == "" {
		t.Fatal("response has no X-Request-ID")
	}
	if upID := <-gotID; upID != respID {
		t.Errorf("upstream got request ID %q, client got %q", upID, respID)
	}
}

func TestGateway_RejectsLargeBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("upstream must not be called for an oversized body")
	}))
	defer upstream.Close()

	gw := newTestGateway(t, upstream.URL)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(strings.Repeat("a", 100))))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}
