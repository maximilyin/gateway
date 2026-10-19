// Package gateway assembles the HTTP handler: health endpoints, middleware and proxy.
package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/maximilyin/gateway/internal/config"
	"github.com/maximilyin/gateway/internal/middleware"
	"github.com/maximilyin/gateway/internal/proxy"
)

type Gateway struct {
	handler http.Handler
	ready   atomic.Bool
}

func New(cfg *config.Config, logger *slog.Logger) (*Gateway, error) {
	target, err := config.ParseUpstreamURL(cfg.Upstream.URL)
	if err != nil {
		return nil, fmt.Errorf("upstream url: %w", err)
	}

	g := &Gateway{}
	g.ready.Store(true)

	p := proxy.New(target, cfg.Upstream.Timeout, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", g.healthz)
	mux.HandleFunc("GET /readyz", g.readyz)
	mux.Handle("/", middleware.MaxBody(cfg.Server.MaxBodyBytes)(p))

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
