package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
)

const tokenHeader = "X-Session-Token"

func newTestHTTP(t *testing.T, maxItems int) (*HTTP, *Manager) {
	t.Helper()
	clk := clock.NewFake(t0)
	m := NewManager(newTestStore(t, clk, maxItems), clk, Config{TTL: time.Minute, MaxLifetime: time.Hour})
	return NewHTTP(m, tokenHeader, slog.New(slog.DiscardHandler)), m
}

func TestHTTP_Issue(t *testing.T) {
	h, m := newTestHTTP(t, 10)

	req := httptest.NewRequest(http.MethodPost, "/auth/session", nil)
	req.RemoteAddr = "203.0.113.7:4000"
	rec := httptest.NewRecorder()
	h.Issue(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (the body holds a secret)", cc)
	}

	var body sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	s, err := m.Authenticate(context.Background(), body.Token)
	if err != nil {
		t.Fatalf("issued token does not authenticate: %v", err)
	}
	if s.ID != body.SessionID || s.ClientIP != "203.0.113.7" {
		t.Errorf("session = %+v, response = %+v", s, body)
	}
}

func TestHTTP_IssueStoreFull(t *testing.T) {
	h, _ := newTestHTTP(t, 1)

	for i, want := range []int{http.StatusCreated, http.StatusServiceUnavailable} {
		rec := httptest.NewRecorder()
		h.Issue(rec, httptest.NewRequest(http.MethodPost, "/auth/session", nil))
		if rec.Code != want {
			t.Errorf("issue #%d: status = %d, want %d", i+1, rec.Code, want)
		}
	}
}

func TestHTTP_InfoAndRevoke(t *testing.T) {
	h, m := newTestHTTP(t, 10)
	token, s, _ := m.Issue(context.Background(), "")

	call := func(handler http.HandlerFunc, method, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/auth/session", nil)
		if token != "" {
			req.Header.Set(tokenHeader, token)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	rec := call(h.Info, http.MethodGet, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("info: status %d, want 200", rec.Code)
	}
	var body sessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.SessionID != s.ID || body.Token != "" {
		t.Errorf("info body = %+v, want session ID and no token", body)
	}

	if rec := call(h.Info, http.MethodGet, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("info without token: status %d, want 401", rec.Code)
	}
	if rec := call(h.Revoke, http.MethodDelete, token); rec.Code != http.StatusNoContent {
		t.Errorf("revoke: status %d, want 204", rec.Code)
	}
	if rec := call(h.Info, http.MethodGet, token); rec.Code != http.StatusUnauthorized {
		t.Errorf("info after revoke: status %d, want 401", rec.Code)
	}
}

func TestHTTP_Attach(t *testing.T) {
	h, m := newTestHTTP(t, 10)
	token, s, _ := m.Issue(context.Background(), "")

	type result struct {
		called         bool
		ctxSession     *Session
		idHeader, tokH string
		issued         string
	}
	run := func(setup func(r *http.Request)) (result, int) {
		var res result
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			res.called = true
			if sess, ok := FromContext(r.Context()); ok {
				res.ctxSession = &sess
			}
			res.idHeader = r.Header.Get(IDHeader)
			res.tokH = r.Header.Get(tokenHeader)
		})
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		setup(req)
		rec := httptest.NewRecorder()
		h.Attach(next).ServeHTTP(rec, req)
		res.issued = rec.Header().Get(tokenHeader)
		return res, rec.Code
	}

	t.Run("valid token is reused", func(t *testing.T) {
		res, _ := run(func(r *http.Request) { r.Header.Set(tokenHeader, token) })
		if res.ctxSession == nil || res.ctxSession.ID != s.ID {
			t.Errorf("context session = %+v, want %s", res.ctxSession, s.ID)
		}
		if res.idHeader != s.ID {
			t.Errorf("X-Session-ID = %q, want %q", res.idHeader, s.ID)
		}
		if res.tokH != "" {
			t.Error("token header was forwarded upstream")
		}
		if res.issued != "" {
			t.Error("a live session must not be replaced with a new token")
		}
	})

	t.Run("no token issues a session", func(t *testing.T) {
		res, _ := run(func(r *http.Request) { r.Header.Set(IDHeader, "spoofed") })
		if !res.called || res.ctxSession == nil || res.ctxSession.ID == "" {
			t.Fatalf("result = %+v, want a newly issued session", res)
		}
		if res.ctxSession.ID == "spoofed" || res.idHeader == "spoofed" {
			t.Errorf("spoofed X-Session-ID kept: %+v", res)
		}
		if res.idHeader != res.ctxSession.ID {
			t.Errorf("upstream session id %q != context %q", res.idHeader, res.ctxSession.ID)
		}
		if res.issued == "" {
			t.Error("new token was not returned in the response header")
		}
		if _, err := m.Authenticate(context.Background(), res.issued); err != nil {
			t.Errorf("issued token does not authenticate: %v", err)
		}
	})

	t.Run("invalid token is replaced", func(t *testing.T) {
		res, code := run(func(r *http.Request) { r.Header.Set(tokenHeader, "bad") })
		if code != http.StatusOK || !res.called || res.issued == "" {
			t.Errorf("status = %d, called = %v, issued = %q; want 200 with a new token", code, res.called, res.issued)
		}
		if res.ctxSession != nil && res.ctxSession.ID == s.ID {
			t.Error("invalid token reused the previous session")
		}
	})
}

func TestRequire(t *testing.T) {
	called := false
	h := Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized || called {
		t.Errorf("without session: status %d, called %v; want 401, false", rec.Code, called)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(NewContext(req.Context(), Session{ID: "x"}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Error("with session: next handler was not called")
	}
}
