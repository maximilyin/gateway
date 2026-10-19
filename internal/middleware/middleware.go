// Package middleware contains http.Handler wrappers shared by the whole gateway.
package middleware

import "net/http"

// Middleware wraps a handler with extra behaviour and returns a new handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so that the first one in the list is the outermost:
// Chain(h, A, B) handles a request as A -> B -> h.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
