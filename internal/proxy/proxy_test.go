package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestProxy(t *testing.T, upstreamURL string, timeout time.Duration) *Proxy {
	t.Helper()
	target, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	return New(target, timeout, slog.New(slog.DiscardHandler))
}

func TestProxy_ForwardsRequest(t *testing.T) {
	type seen struct {
		method, path, query, body, xff, custom string
	}
	got := make(chan seen, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			body:   string(body),
			xff:    r.Header.Get("X-Forwarded-For"),
			custom: r.Header.Get("X-Custom"),
		}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL, time.Second)

	req := httptest.NewRequest(http.MethodPost, "/users/42?expand=true", strings.NewReader(`{"name":"max"}`))
	req.RemoteAddr = "203.0.113.7:51000"
	req.Header.Set("X-Custom", "value")
	req.Header.Set("X-Forwarded-For", "1.2.3.4") // spoofed by the client
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if rec.Body.String() != "created" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "created")
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Error("upstream response header not passed back")
	}

	s := <-got
	if s.method != http.MethodPost || s.path != "/users/42" || s.query != "expand=true" {
		t.Errorf("upstream saw %s %s?%s, want POST /users/42?expand=true", s.method, s.path, s.query)
	}
	if s.body != `{"name":"max"}` {
		t.Errorf("upstream body = %q", s.body)
	}
	if s.custom != "value" {
		t.Errorf("X-Custom = %q, want %q", s.custom, "value")
	}
	if s.xff != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want only the real client IP 203.0.113.7", s.xff)
	}
}

func TestProxy_TargetBasePath(t *testing.T) {
	got := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got <- r.URL.Path
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/api", time.Second)
	p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/users", nil))

	if path := <-got; path != "/api/users" {
		t.Errorf("upstream path = %q, want /api/users", path)
	}
}

func TestProxy_UpstreamDown_Returns502(t *testing.T) {
	// Reserve a port and close it, so nothing is listening there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	p := newTestProxy(t, "http://"+addr, time.Second)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestProxy_SlowUpstream_Returns504(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	p := newTestProxy(t, upstream.URL, 50*time.Millisecond)
	rec := httptest.NewRecorder()

	start := time.Now()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("request took %s, timeout was not enforced", elapsed)
	}
}

func TestProxy_BodyTooLarge_Returns413(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL, time.Second)
	limited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 10)
		p.ServeHTTP(w, r)
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", 1000)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	limited.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}
