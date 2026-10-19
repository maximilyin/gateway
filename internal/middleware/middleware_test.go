package middleware

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestChain_Order(t *testing.T) {
	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mark("A"), mark("B"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got, want := strings.Join(order, ","), "A,B,handler"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		wantKeep bool
	}{
		{name: "missing is generated", incoming: "", wantKeep: false},
		{name: "valid is kept", incoming: "abc-123", wantKeep: true},
		{name: "control chars are replaced", incoming: "bad\x01id", wantKeep: false},
		{name: "spaces are replaced", incoming: "has space", wantKeep: false},
		{name: "too long is replaced", incoming: strings.Repeat("a", maxRequestIDLen+1), wantKeep: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctxID, headerID string
			h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxID = RequestIDFrom(r.Context())
				headerID = r.Header.Get(RequestIDHeader)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if ctxID == "" {
				t.Fatal("request ID missing from context")
			}
			if headerID != ctxID {
				t.Errorf("request header = %q, context = %q; want equal", headerID, ctxID)
			}
			if got := rec.Header().Get(RequestIDHeader); got != ctxID {
				t.Errorf("response header = %q, want %q", got, ctxID)
			}
			if tt.wantKeep && ctxID != tt.incoming {
				t.Errorf("ID = %q, want incoming %q kept", ctxID, tt.incoming)
			}
			if !tt.wantKeep && ctxID == tt.incoming {
				t.Errorf("ID = %q, want a newly generated one", ctxID)
			}
		})
	}
}

func TestRequestID_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id := newRequestID()
		if seen[id] {
			t.Fatalf("duplicate request ID %q", id)
		}
		seen[id] = true
	}
}

func TestRecover_Returns500(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	h := Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Errorf("panic value not logged: %s", logs.String())
	}
}

func TestRecover_RepanicsOnAbortHandler(t *testing.T) {
	h := Recover(discardLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler to propagate", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestAccessLog_RecordsStatusAndBytes(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	h := AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))

	out := logs.String()
	for _, want := range []string{`"status":418`, `"bytes":5`, `"method":"POST"`, `"path":"/x"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %s does not contain %s", out, want)
		}
	}
}

func TestStatusRecorder_DefaultsTo200(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	if rec.Status() != http.StatusOK {
		t.Errorf("Status() with no writes = %d, want 200", rec.Status())
	}
	_, _ = rec.Write([]byte("x"))
	rec.WriteHeader(http.StatusInternalServerError) // too late, must be ignored
	if rec.Status() != http.StatusOK {
		t.Errorf("Status() after implicit 200 = %d, want 200", rec.Status())
	}
}

func TestStatusRecorder_SupportsFlush(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner}

	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush through statusRecorder: %v", err)
	}
	if !inner.Flushed {
		t.Error("underlying writer was not flushed")
	}
}

func TestMaxBody(t *testing.T) {
	const limit = 10

	var readErr error
	h := MaxBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))

	t.Run("declared length over limit is rejected early", func(t *testing.T) {
		readErr = nil
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", limit+1))))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", rec.Code)
		}
	})

	t.Run("unknown length over limit fails on read", func(t *testing.T) {
		readErr = nil
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", limit+1)))
		req.ContentLength = -1 // as with chunked transfer encoding
		h.ServeHTTP(httptest.NewRecorder(), req)

		var mbe *http.MaxBytesError
		if !errors.As(readErr, &mbe) {
			t.Errorf("read error = %v, want *http.MaxBytesError", readErr)
		}
	})

	t.Run("body within limit passes", func(t *testing.T) {
		readErr = nil
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader("small")))
		if readErr != nil {
			t.Errorf("read error = %v, want nil", readErr)
		}
	})
}
