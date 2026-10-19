// Package proxy forwards requests to an upstream service.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/maximilyin/gateway/internal/middleware"
)

// Proxy forwards every request to a single upstream with a per-request timeout.
type Proxy struct {
	rp      *httputil.ReverseProxy
	timeout time.Duration
	logger  *slog.Logger
}

func New(target *url.URL, timeout time.Duration, logger *slog.Logger) *Proxy {
	p := &Proxy{timeout: timeout, logger: logger}

	p.rp = &httputil.ReverseProxy{
		// Rewrite runs on a copy of the incoming request (pr.Out). Before it is
		// called, ReverseProxy has already removed hop-by-hop headers and any
		// client-supplied X-Forwarded-* headers, so clients cannot spoof them.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		Transport:    newTransport(),
		ErrorHandler: p.handleError,
		// Negative value flushes every write immediately, which keeps streaming
		// responses (SSE, long polling) responsive.
		FlushInterval: -1,
	}
	return p
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()

	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// handleError picks the status code the client sees when the upstream call fails.
func (p *Proxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	reqID := middleware.RequestIDFrom(ctx)

	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr):
		w.WriteHeader(http.StatusRequestEntityTooLarge)

	case errors.Is(err, context.DeadlineExceeded):
		p.logger.WarnContext(ctx, "upstream timeout", "request_id", reqID, "error", err)
		w.WriteHeader(http.StatusGatewayTimeout)

	case errors.Is(err, context.Canceled):
		// The client went away; nobody is left to read a response.
		p.logger.DebugContext(ctx, "client canceled request", "request_id", reqID)

	default:
		p.logger.WarnContext(ctx, "upstream unavailable", "request_id", reqID, "error", err)
		w.WriteHeader(http.StatusBadGateway)
	}
}

// newTransport tunes the HTTP client side of the proxy. The defaults of
// http.DefaultTransport keep only 2 idle connections per host, which forces new
// TCP handshakes under load when all traffic goes to a few internal services.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil, // never route internal traffic through HTTP_PROXY from the environment
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
