// Package gateway assembles the HTTP handler: health endpoints, middleware, router
// and proxies.
package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/maximilyin/gateway/internal/clock"
	"github.com/maximilyin/gateway/internal/config"
	"github.com/maximilyin/gateway/internal/middleware"
	"github.com/maximilyin/gateway/internal/proxy"
	"github.com/maximilyin/gateway/internal/router"
	"github.com/maximilyin/gateway/internal/session"
)

type Gateway struct {
	handler  http.Handler
	ready    atomic.Bool
	sessions *session.MemoryStore
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
		proxies[name] = proxy.New(target, u.Timeout, logger.With("upstream", name))
	}

	// The store starts a sweeper goroutine. Close it if a later step fails.
	sc := cfg.Sessions
	store := session.NewMemoryStore(o.clock, sc.MaxSessions, sc.CleanupInterval)
	manager := session.NewManager(store, o.clock, session.Config{TTL: sc.TTL, MaxLifetime: sc.MaxLifetime})
	sessions := session.NewHTTP(manager, sc.Header, logger)

	routes := make([]router.Route, 0, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		p, ok := proxies[rc.Upstream]
		if !ok {
			store.Close()
			return nil, fmt.Errorf("route %q: unknown upstream %q", rc.Match, rc.Upstream)
		}
		// Attach runs only after the router has matched a configured route, so a
		// 404 does not mint a session. A missing or dead token is issued here,
		// before the request is proxied.
		var h http.Handler = p
		if rc.RequireSession {
			h = session.Require(h)
		}
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
		store.Close()
		return nil, fmt.Errorf("routes: %w", err)
	}

	g := &Gateway{sessions: store}
	g.ready.Store(true)

	// ServeMux serves the gateway's own endpoints and hands everything else to the
	// router. It also cleans paths first ("/a/../b" is redirected to "/b"), so a
	// route can never be bypassed with dot segments.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", g.healthz)
	mux.HandleFunc("GET /readyz", g.readyz)
	mux.HandleFunc("POST "+sc.Endpoint, sessions.Issue)
	mux.HandleFunc("GET "+sc.Endpoint, sessions.Info)
	mux.HandleFunc("DELETE "+sc.Endpoint, sessions.Revoke)
	mux.Handle("/", middleware.MaxBody(cfg.Server.MaxBodyBytes)(rt))

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
