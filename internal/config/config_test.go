package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimal is the smallest valid config body; tests append or override sections.
const minimal = `
upstreams:
  users:
    url: http://users-svc:8080
routes:
  - match: /service/users/*
    upstream: users
`

func TestParse_Valid(t *testing.T) {
	const yml = `
server:
  listen: ":9090"
  read_header_timeout: 2s
  max_body_bytes: 1024
log:
  level: debug
upstreams:
  users:
    url: http://users-svc:8080
    timeout: 3s
  orders:
    url: http://orders-svc:8080
sessions:
  ttl: 5m
routes:
  - name: users-login
    match: /service/users/login
    methods: [POST]
    upstream: users
    strip_prefix: /service/users
  - match: /service/orders/*
    upstream: orders
    require_session: true
`
	cfg, err := Parse(strings.NewReader(yml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if cfg.Server.Listen != ":9090" {
		t.Errorf("Listen = %q, want %q", cfg.Server.Listen, ":9090")
	}
	if cfg.Server.ReadHeaderTimeout != 2*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want 2s", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Server.MaxBodyBytes != 1024 {
		t.Errorf("MaxBodyBytes = %d, want 1024", cfg.Server.MaxBodyBytes)
	}
	if cfg.Log.Level != slog.LevelDebug {
		t.Errorf("Log.Level = %s, want DEBUG", cfg.Log.Level)
	}

	if got := cfg.Upstreams["users"].Timeout; got != 3*time.Second {
		t.Errorf("users timeout = %s, want 3s", got)
	}
	if got := cfg.Upstreams["orders"].Timeout; got != DefaultUpstreamTimeout {
		t.Errorf("orders timeout = %s, want default %s", got, DefaultUpstreamTimeout)
	}

	if len(cfg.Routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(cfg.Routes))
	}
	want := RouteConfig{
		Name:        "users-login",
		Match:       "/service/users/login",
		Methods:     []string{"POST"},
		Upstream:    "users",
		StripPrefix: "/service/users",
	}
	got := cfg.Routes[0]
	if got.Name != want.Name || got.Match != want.Match || got.Upstream != want.Upstream ||
		got.StripPrefix != want.StripPrefix || len(got.Methods) != 1 || got.Methods[0] != "POST" {
		t.Errorf("route[0] = %+v, want %+v", got, want)
	}

	if cfg.Routes[0].RequireSession || !cfg.Routes[1].RequireSession {
		t.Errorf("require_session = %v, %v; want false, true", cfg.Routes[0].RequireSession, cfg.Routes[1].RequireSession)
	}
	if cfg.Sessions.TTL != 5*time.Minute {
		t.Errorf("Sessions.TTL = %s, want 5m", cfg.Sessions.TTL)
	}

	// Fields absent from the YAML must keep their defaults.
	def := Default()
	if cfg.Sessions.Header != def.Sessions.Header || cfg.Sessions.Endpoint != def.Sessions.Endpoint {
		t.Errorf("sessions header/endpoint = %q %q, want defaults", cfg.Sessions.Header, cfg.Sessions.Endpoint)
	}
	if cfg.Server.IdleTimeout != def.Server.IdleTimeout {
		t.Errorf("IdleTimeout = %s, want default %s", cfg.Server.IdleTimeout, def.Server.IdleTimeout)
	}
	if cfg.Server.ShutdownTimeout != def.Server.ShutdownTimeout {
		t.Errorf("ShutdownTimeout = %s, want default %s", cfg.Server.ShutdownTimeout, def.Server.ShutdownTimeout)
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name    string
		yml     string
		wantErr string
	}{
		{
			name:    "empty file",
			yml:     "",
			wantErr: "file is empty",
		},
		{
			name:    "unknown field",
			yml:     "upstreams:\n  a:\n    url: http://a\n    timeot: 1s\n",
			wantErr: "field timeot not found",
		},
		{
			name:    "no upstreams and routes",
			yml:     "server:\n  listen: \":8080\"\n",
			wantErr: "at least one upstream is required",
		},
		{
			name:    "missing upstream url",
			yml:     "upstreams:\n  a:\n    timeout: 1s\nroutes:\n  - match: /*\n    upstream: a\n",
			wantErr: "upstreams.a.url: is required",
		},
		{
			name:    "bad scheme",
			yml:     "upstreams:\n  a:\n    url: ftp://a\nroutes:\n  - match: /*\n    upstream: a\n",
			wantErr: `scheme must be http or https, got "ftp"`,
		},
		{
			name:    "missing host",
			yml:     "upstreams:\n  a:\n    url: http://\nroutes:\n  - match: /*\n    upstream: a\n",
			wantErr: "host is required",
		},
		{
			name:    "bad duration",
			yml:     "upstreams:\n  a:\n    url: http://a\n    timeout: soon\n",
			wantErr: "parse config",
		},
		{
			name:    "bad log level",
			yml:     "log:\n  level: loud\n" + minimal,
			wantErr: "parse config",
		},
		{
			name:    "upstream timeout exceeds write timeout",
			yml:     "server:\n  write_timeout: 5s\nupstreams:\n  a:\n    url: http://a\n    timeout: 10s\nroutes:\n  - match: /*\n    upstream: a\n",
			wantErr: "upstreams.a.timeout (10s) must be less than server.write_timeout (5s)",
		},
		{
			name:    "non-positive body limit",
			yml:     "server:\n  max_body_bytes: 0\n" + minimal,
			wantErr: "server.max_body_bytes must be positive",
		},
		{
			name:    "no routes",
			yml:     "upstreams:\n  a:\n    url: http://a\n",
			wantErr: "at least one route is required",
		},
		{
			name:    "route without match",
			yml:     "upstreams:\n  a:\n    url: http://a\nroutes:\n  - upstream: a\n",
			wantErr: "routes[0].match is required",
		},
		{
			name:    "route without upstream",
			yml:     "upstreams:\n  a:\n    url: http://a\nroutes:\n  - match: /*\n",
			wantErr: "routes[0].upstream is required",
		},
		{
			name:    "session endpoint with wildcard",
			yml:     "sessions:\n  endpoint: /auth/{id}\n" + minimal,
			wantErr: "sessions.endpoint",
		},
		{
			name:    "session endpoint with trailing slash",
			yml:     "sessions:\n  endpoint: /auth/\n" + minimal,
			wantErr: "sessions.endpoint",
		},
		{
			name:    "session header with colon",
			yml:     "sessions:\n  header: \"X-Token:\"\n" + minimal,
			wantErr: "is not a valid header name",
		},
		{
			name:    "max lifetime below ttl",
			yml:     "sessions:\n  ttl: 1h\n  max_lifetime: 30m\n" + minimal,
			wantErr: "sessions.max_lifetime (30m0s) must not be less than sessions.ttl (1h0m0s)",
		},
		{
			name:    "non-positive max sessions",
			yml:     "sessions:\n  max_sessions: 0\n" + minimal,
			wantErr: "sessions.max_sessions must be positive",
		},
		{
			name:    "route with unknown upstream",
			yml:     "upstreams:\n  a:\n    url: http://a\nroutes:\n  - match: /*\n    upstream: b\n",
			wantErr: `routes[0].upstream: unknown upstream "b"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.yml))
			if err == nil {
				t.Fatalf("Parse succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_ReportsAllErrors(t *testing.T) {
	cfg := Default()
	cfg.Server.Listen = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded, want error")
	}
	for _, want := range []string{
		"server.listen is required",
		"at least one upstream is required",
		"at least one route is required",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("Load of missing file succeeded, want error")
	}
}
