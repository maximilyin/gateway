package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// named returns a handler that writes its name and the path it received.
func named(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, name+" "+r.URL.Path)
	})
}

func mustNew(t *testing.T, routes []Route) *Router {
	t.Helper()
	rt, err := New(routes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return rt
}

func TestMatch(t *testing.T) {
	rt := mustNew(t, []Route{
		// Declared from least to most specific on purpose: order must not matter.
		{Name: "catch-all", Pattern: "/*", Handler: named("catch-all")},
		{Name: "users", Pattern: "/service/users/*", Handler: named("users")},
		{Name: "users-admin", Pattern: "/service/users/admin/*", Handler: named("users-admin")},
		{Name: "users-me", Pattern: "/service/users/me", Handler: named("users-me")},
		{Name: "login", Pattern: "/service/users/login", Methods: []string{"POST"}, Handler: named("login")},
		{Name: "orders-read", Pattern: "/service/orders/*", Methods: []string{"GET"}, Handler: named("orders-read")},
		{Name: "root", Pattern: "/", Handler: named("root")},
	})

	tests := []struct {
		method, path string
		want         string // route name, "" for no match
	}{
		{"GET", "/service/users/me", "users-me"},
		{"GET", "/service/users/me/settings", "users"},
		{"GET", "/service/users/42", "users"},
		{"GET", "/service/users", "users"},
		{"GET", "/service/users/", "users"},
		{"GET", "/service/usersX", "catch-all"},
		{"GET", "/service/users/admin", "users-admin"},
		{"GET", "/service/users/admin/x/y", "users-admin"},
		{"POST", "/service/users/login", "login"},
		{"GET", "/service/users/login", "users"}, // method mismatch falls back to the prefix
		{"GET", "/service/orders/1", "orders-read"},
		{"HEAD", "/service/orders/1", "orders-read"}, // GET implies HEAD
		{"DELETE", "/service/orders/1", "catch-all"},
		{"GET", "/", "root"},
		{"GET", "/anything/else", "catch-all"},
		{"OPTIONS", "*", ""},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			r, _ := rt.Match(tt.method, tt.path)
			got := ""
			if r != nil {
				got = r.Name
			}
			if got != tt.want {
				t.Errorf("matched %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServeHTTP_NotFoundAndMethodNotAllowed(t *testing.T) {
	rt := mustNew(t, []Route{
		{Pattern: "/service/orders/*", Methods: []string{"GET", "POST"}, Handler: named("orders")},
		{Pattern: "/service/orders/export", Methods: []string{"PUT"}, Handler: named("export")},
	})

	tests := []struct {
		method, path string
		status       int
		allow        string
	}{
		{"GET", "/service/orders/1", http.StatusOK, ""},
		{"DELETE", "/service/orders/1", http.StatusMethodNotAllowed, "GET, HEAD, POST"},
		{"DELETE", "/service/orders/export", http.StatusMethodNotAllowed, "GET, HEAD, POST, PUT"},
		{"GET", "/service/users/1", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if got := rec.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
		})
	}
}

func TestServeHTTP_StripPrefix(t *testing.T) {
	rt := mustNew(t, []Route{
		{Pattern: "/service/users/*", StripPrefix: "/service/users", Handler: named("users")},
		{Pattern: "/api/v1/*", StripPrefix: "/api", Handler: named("api")},
		{Pattern: "/raw/*", Handler: named("raw")},
	})

	tests := []struct {
		path, want string
	}{
		{"/service/users/42", "users /42"},
		{"/service/users", "users /"},
		{"/service/users/", "users /"},
		{"/api/v1/items", "api /v1/items"},
		{"/raw/x", "raw /raw/x"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, req)

			if rec.Body.String() != tt.want {
				t.Errorf("got %q, want %q", rec.Body.String(), tt.want)
			}
			if req.URL.Path != tt.path {
				t.Errorf("original request was modified: path %q", req.URL.Path)
			}
		})
	}
}

func TestServeHTTP_StripPrefixKeepsEncoding(t *testing.T) {
	rt := mustNew(t, []Route{
		{Pattern: "/files/*", StripPrefix: "/files", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, r.URL.EscapedPath())
		})},
	})

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/a%2Fb", nil))

	if got := rec.Body.String(); got != "/a%2Fb" {
		t.Errorf("escaped path = %q, want /a%%2Fb", got)
	}
}

func TestRouteFrom(t *testing.T) {
	var seen *Route
	rt := mustNew(t, []Route{
		{Name: "users", Pattern: "/users/*", Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = RouteFrom(r.Context())
		})},
	})
	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/users/1", nil))

	if seen == nil || seen.Name != "users" {
		t.Fatalf("RouteFrom = %+v, want route users", seen)
	}
}

func TestNew_DefaultName(t *testing.T) {
	rt := mustNew(t, []Route{{Pattern: "/a/*", Handler: named("a")}})
	r, _ := rt.Match(http.MethodGet, "/a/b")
	if r == nil || r.Name != "/a/*" {
		t.Fatalf("route = %+v, want name defaulted to pattern", r)
	}
}

func TestNew_Errors(t *testing.T) {
	h := named("h")
	tests := []struct {
		name    string
		routes  []Route
		wantErr string
	}{
		{"no leading slash", []Route{{Pattern: "users/*", Handler: h}}, `must start with "/"`},
		{"wildcard in the middle", []Route{{Pattern: "/a/*/b", Handler: h}}, "invalid character '*'"},
		{"wildcard without slash", []Route{{Pattern: "/users*", Handler: h}}, "invalid character '*'"},
		{"empty segment", []Route{{Pattern: "/a//b", Handler: h}}, "empty segments"},
		{"dot-dot segment", []Route{{Pattern: "/a/../b/*", Handler: h}}, `"." or ".."`},
		{"percent", []Route{{Pattern: "/a%20b", Handler: h}}, "invalid character '%'"},
		{"query", []Route{{Pattern: "/a?x=1", Handler: h}}, "invalid character '?'"},
		{"nil handler", []Route{{Pattern: "/a"}}, "handler is required"},
		{"unknown method", []Route{{Pattern: "/a", Methods: []string{"FETCH"}, Handler: h}}, `unsupported method "FETCH"`},
		{"strip not a prefix", []Route{{Pattern: "/a/b/*", StripPrefix: "/x", Handler: h}}, "is not a path prefix"},
		{"strip mid-segment", []Route{{Pattern: "/api/*", StripPrefix: "/ap", Handler: h}}, "is not a path prefix"},
		{"strip trailing slash", []Route{{Pattern: "/api/*", StripPrefix: "/api/", Handler: h}}, `must not end with "/"`},
		{
			"duplicate pattern",
			[]Route{{Pattern: "/a/*", Handler: h}, {Pattern: "/a/*", Handler: h}},
			"conflicts with route",
		},
		{
			"overlapping methods",
			[]Route{
				{Pattern: "/a", Methods: []string{"GET", "POST"}, Handler: h},
				{Pattern: "/a", Methods: []string{"post"}, Handler: h},
			},
			"overlapping methods",
		},
		{
			"any-method overlaps everything",
			[]Route{
				{Pattern: "/a", Methods: []string{"GET"}, Handler: h},
				{Pattern: "/a", Handler: h},
			},
			"overlapping methods",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.routes)
			if err == nil {
				t.Fatalf("New succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNew_SamePatternDisjointMethods(t *testing.T) {
	rt := mustNew(t, []Route{
		{Name: "read", Pattern: "/a", Methods: []string{"GET"}, Handler: named("read")},
		{Name: "write", Pattern: "/a", Methods: []string{"POST", "PUT"}, Handler: named("write")},
	})

	for method, want := range map[string]string{"GET": "read", "POST": "write", "PUT": "write"} {
		r, _ := rt.Match(method, "/a")
		if r == nil || r.Name != want {
			t.Errorf("%s /a matched %+v, want %s", method, r, want)
		}
	}
}
