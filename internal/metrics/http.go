package metrics

import (
	"net/http"
	"strconv"
)

// ObserveStatus counts finished responses by HTTP status code.
func ObserveStatus(c *Counter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			code := rec.status
			if code == 0 {
				code = http.StatusOK
			}
			c.Inc(map[string]string{"code": strconv.Itoa(code)})
		})
	}
}

// statusRecorder remembers the status written through it and still exposes
// Flush and Hijack via Unwrap, which the reverse proxy needs.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
