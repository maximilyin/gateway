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
	"strconv"
	"time"

	"github.com/maximilyin/gateway/internal/breaker"
	"github.com/maximilyin/gateway/internal/middleware"
)

// Proxy forwards every request to a single upstream with a per-request timeout.
type Proxy struct {
	rp      *httputil.ReverseProxy
	timeout time.Duration
	logger  *slog.Logger
	breaker *breaker.Breaker
}

func New(target *url.URL, timeout time.Duration, logger *slog.Logger, br *breaker.Breaker) *Proxy {
	p := &Proxy{timeout: timeout, logger: logger, breaker: br}

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
	var permit breaker.Permit
	if p.breaker != nil {
		var retry time.Duration
		var ok bool
		permit, retry, ok = p.breaker.Allow()
		if !ok {
			p.reject(w, retry)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()

	capw := &statusCapture{ResponseWriter: w}
	p.rp.ServeHTTP(capw, r.WithContext(ctx))
	if p.breaker != nil {
		p.breaker.Report(permit, classify(capw.status))
	}
}

func classify(status int) breaker.Result {
	switch {
	case status == 0 || status == http.StatusRequestEntityTooLarge:
		return breaker.Ignored
	case status >= 500:
		return breaker.Failure
	default:
		return breaker.Success
	}
}

func (p *Proxy) reject(w http.ResponseWriter, retry time.Duration) {
	secs := int((retry + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":"upstream unavailable"}` + "\n"))
}

// statusCapture remembers the status written by ReverseProxy or its ErrorHandler
// and still exposes Flush and Hijack through Unwrap.
type statusCapture struct {
	http.ResponseWriter
	status int
}

func (c *statusCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *statusCapture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.ResponseWriter.Write(b)
}

func (c *statusCapture) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
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
