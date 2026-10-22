package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maximilyin/gateway/internal/config"
	"github.com/maximilyin/gateway/internal/middleware"
)

func newTestGateway(t *testing.T, cfg *config.Config, opts ...Option) *Gateway {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	gw, err := New(cfg, slog.New(slog.DiscardHandler), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	return gw
}

// singleUpstream builds a config that sends every path to one upstream.
func singleUpstream(url string) *config.Config {
	cfg := config.Default()
	cfg.Server.MaxBodyBytes = 16
	cfg.Upstreams = map[string]config.UpstreamConfig{
		"svc": {URL: url, Timeout: time.Second},
	}
	cfg.Routes = []config.RouteConfig{{Match: "/*", Upstream: "svc"}}
	return cfg
}

// echoServer replies with its name and the path it received.
func echoServer(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, name+" "+r.URL.RequestURI())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGateway_HealthEndpoints(t *testing.T) {
	gw := newTestGateway(t, singleUpstream("http://127.0.0.1:1"))

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

	gw := newTestGateway(t, singleUpstream(upstream.URL))
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

	gw := newTestGateway(t, singleUpstream(upstream.URL))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(strings.Repeat("a", 100))))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestGateway_Routing(t *testing.T) {
	users := echoServer(t, "users")
	orders := echoServer(t, "orders")

	cfg := config.Default()
	cfg.Upstreams = map[string]config.UpstreamConfig{
		"users":  {URL: users.URL, Timeout: time.Second},
		"orders": {URL: orders.URL + "/v2", Timeout: time.Second},
	}
	cfg.Routes = []config.RouteConfig{
		{Match: "/service/users/*", Upstream: "users", StripPrefix: "/service/users"},
		{Match: "/service/users/login", Methods: []string{"POST"}, Upstream: "users"},
		{Match: "/service/orders/*", Methods: []string{"GET"}, Upstream: "orders", StripPrefix: "/service/orders"},
	}
	gw := newTestGateway(t, cfg)

	tests := []struct {
		method, target string
		status         int
		body           string
	}{
		{"GET", "/service/users/42?expand=1", 200, "users /42?expand=1"},
		{"GET", "/service/users", 200, "users /"},
		{"POST", "/service/users/login", 200, "users /service/users/login"},
		{"GET", "/service/orders/7", 200, "orders /v2/7"},
		{"DELETE", "/service/orders/7", 405, ""},
		{"GET", "/service/payments/1", 404, ""},
		{"GET", "/service/usersX", 404, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			gw.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.target, nil))

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.status, rec.Body.String())
			}
			if tt.body != "" && rec.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.body)
			}
		})
	}
}

func TestGateway_DotSegmentsAreRedirectedNotRouted(t *testing.T) {
	cfg := config.Default()
	cfg.Upstreams = map[string]config.UpstreamConfig{
		"public": {URL: echoServer(t, "public").URL, Timeout: time.Second},
	}
	cfg.Routes = []config.RouteConfig{{Match: "/public/*", Upstream: "public"}}
	gw := newTestGateway(t, cfg)

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public/../admin/secret", nil))

	if rec.Code < 300 || rec.Code > 399 {
		t.Fatalf("status = %d, want a redirect to the cleaned path", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/secret" {
		t.Errorf("Location = %q, want /admin/secret", loc)
	}
}

func TestNew_InvalidRoutePattern(t *testing.T) {
	cfg := singleUpstream("http://127.0.0.1:1")
	cfg.Routes = []config.RouteConfig{{Match: "/a/*/b", Upstream: "svc"}}

	_, err := New(cfg, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("New error = %v, want invalid pattern error", err)
	}
}
