// Package gateway assembles the HTTP handler: health endpoints, middleware, router
// and proxies.
package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/maximilyin/gateway/internal/breaker"
	"github.com/maximilyin/gateway/internal/clock"
	"github.com/maximilyin/gateway/internal/config"
	"github.com/maximilyin/gateway/internal/middleware"
	"github.com/maximilyin/gateway/internal/proxy"
	"github.com/maximilyin/gateway/internal/ratelimit"
	"github.com/maximilyin/gateway/internal/router"
	"github.com/maximilyin/gateway/internal/session"
)

type Gateway struct {
	handler  http.Handler
	ready    atomic.Bool
	sessions *session.MemoryStore
	buckets  *ratelimit.MemoryStore
}

type options struct {
	clock clock.Clock
}

// Option customises New. Tests use it to inject a fake clock.
type Option func(*options)

func WithClock(c clock.Clock) Option {
	return func(o *options) { o.clock = c }
}

func New(cfg *config.Config, logger *slog.Logger, opts ...Option) (*Gateway, error) {
	o := options{clock: clock.Real{}}
	for _, opt := range opts {
		opt(&o)
	}

	// One proxy per upstream, so all routes to the same service share one
	// connection pool.
	proxies := make(map[string]*proxy.Proxy, len(cfg.Upstreams))
	for name, u := range cfg.Upstreams {
		target, err := config.ParseUpstreamURL(u.URL)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: %w", name, err)
		}
		bc := u.Breaker.WithDefaults()
		br := breaker.New(breaker.Settings{
			FailureThreshold: bc.FailureThreshold,
			OpenTimeout:      bc.OpenTimeout,
			HalfOpenMax:      bc.HalfOpenMax,
		}, o.clock, logger.With("upstream", name))
		proxies[name] = proxy.New(target, u.Timeout, logger.With("upstream", name), br)
	}

	// Both stores start a sweeper goroutine. Close them if a later step fails.
	sc := cfg.Sessions
	store := session.NewMemoryStore(o.clock, sc.MaxSessions, sc.CleanupInterval)
	manager := session.NewManager(store, o.clock, session.Config{TTL: sc.TTL, MaxLifetime: sc.MaxLifetime})
	sessions := session.NewHTTP(manager, sc.Header, logger)

	rcfg := cfg.RateLimits
	buckets := ratelimit.NewMemoryStore(o.clock, rcfg.MaxKeys, rcfg.IdleTTL, rcfg.CleanupInterval)
	named := make(map[string]ratelimit.Limit, len(rcfg.Policies))
	for name, lim := range rcfg.Policies {
		named[name] = limitOf(lim)
	}
	limits := ratelimit.New(buckets, limitOf(rcfg.Global), limitOf(rcfg.Default), limitOf(rcfg.SessionIssue), named, logger)
	// Issuance is limited by IP. The explicit POST also spends one global token.
	// An API request that has to create a session spends the issuance token here
	// and the global token later, together with the route policy.
	sessions.SetIssueGuard(limits.AllowIssue)
	sessions.SetPostGuard(limits.AllowGlobal)
	closeStores := func() {
		store.Close()
		buckets.Close()
	}

	routes := make([]router.Route, 0, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		p, ok := proxies[rc.Upstream]
		if !ok {
			closeStores()
			return nil, fmt.Errorf("route %q: unknown upstream %q", rc.Match, rc.Upstream)
		}
		// Attach runs only after the router has matched a configured route, so a
		// 404 does not mint a session. The session limit runs after Attach, so the
		// bucket key is the session that will actually serve the request.
		var h http.Handler = p
		if rc.RequireSession {
			h = session.Require(h)
		}
		h = limits.Session(rc.RateLimit, h)
		h = sessions.Attach(h)
		routes = append(routes, router.Route{
			Name:        rc.Name,
			Pattern:     rc.Match,
			Methods:     rc.Methods,
			StripPrefix: rc.StripPrefix,
			Handler:     h,
		})
	}
	rt, err := router.New(routes)
	if err != nil {
		closeStores()
		return nil, fmt.Errorf("routes: %w", err)
	}

	g := &Gateway{sessions: store, buckets: buckets}
	g.ready.Store(true)

	// ServeMux serves the gateway's own endpoints and hands everything else to the
	// router. It also cleans paths first ("/a/../b" is redirected to "/b"), so a
	// route can never be bypassed with dot segments.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", g.healthz)
	mux.HandleFunc("GET /readyz", g.readyz)
	mux.HandleFunc("POST "+sc.Endpoint, sessions.Issue)
	mux.Handle("GET "+sc.Endpoint, limits.IP("default", http.HandlerFunc(sessions.Info)))
	mux.Handle("DELETE "+sc.Endpoint, limits.IP("default", http.HandlerFunc(sessions.Revoke)))
	mux.Handle("/", middleware.MaxBody(cfg.Server.MaxBodyBytes)(limitMisses(rt, limits)))

	g.handler = middleware.Chain(mux,
		middleware.RequestID,
		middleware.AccessLog(logger),
		middleware.Recover(logger),
	)
	return g, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.handler.ServeHTTP(w, r)
}

// Close stops background goroutines. Call it after the HTTP server has shut down.
func (g *Gateway) Close() {
	g.sessions.Close()
	g.buckets.Close()
}

// limitMisses applies the default per-IP policy to paths that match no route.
// Matched routes limit themselves, after a session exists.
func limitMisses(rt *router.Router, limits *ratelimit.Limiter) http.Handler {
	miss := limits.IP("default", rt)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if route, _ := rt.Match(r.Method, r.URL.Path); route == nil {
			miss.ServeHTTP(w, r)
			return
		}
		rt.ServeHTTP(w, r)
	})
}

func limitOf(c config.RateLimitConfig) ratelimit.Limit {
	return ratelimit.Limit{RPS: c.RPS, Burst: float64(c.Burst)}
}

// SetReady switches /readyz between 200 and 503. It is flipped to false at the
// start of shutdown so a load balancer stops sending new traffic.
func (g *Gateway) SetReady(ready bool) {
	g.ready.Store(ready)
}

// healthz is the liveness probe: the process is up and serving HTTP.
func (g *Gateway) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// readyz is the readiness probe: the gateway wants to receive traffic.
func (g *Gateway) readyz(w http.ResponseWriter, _ *http.Request) {
	if !g.ready.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
