package middleware

import "net/http"

// MaxBody limits request bodies to limit bytes. A declared Content-Length above the
// limit is rejected immediately; a chunked body is cut off by http.MaxBytesReader
// once it grows past the limit, and the read error surfaces in the proxy.
func MaxBody(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}
