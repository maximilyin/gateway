package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse_Valid(t *testing.T) {
	const yml = `
server:
  listen: ":9090"
  read_header_timeout: 2s
  max_body_bytes: 1024
log:
  level: debug
upstream:
  url: http://users-svc:8080
  timeout: 3s
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
	if cfg.Upstream.Timeout != 3*time.Second {
		t.Errorf("Upstream.Timeout = %s, want 3s", cfg.Upstream.Timeout)
	}

	// Fields absent from the YAML must keep their defaults.
	def := Default()
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
			yml:     "upstream:\n  url: http://a\n  timeot: 1s\n",
			wantErr: "field timeot not found",
		},
		{
			name:    "missing upstream url",
			yml:     "server:\n  listen: \":8080\"\n",
			wantErr: "upstream.url: is required",
		},
		{
			name:    "bad scheme",
			yml:     "upstream:\n  url: ftp://a\n",
			wantErr: `scheme must be http or https, got "ftp"`,
		},
		{
			name:    "missing host",
			yml:     "upstream:\n  url: http://\n",
			wantErr: "host is required",
		},
		{
			name:    "bad duration",
			yml:     "upstream:\n  url: http://a\n  timeout: soon\n",
			wantErr: "parse config",
		},
		{
			name:    "bad log level",
			yml:     "log:\n  level: loud\nupstream:\n  url: http://a\n",
			wantErr: "parse config",
		},
		{
			name:    "upstream timeout exceeds write timeout",
			yml:     "server:\n  write_timeout: 5s\nupstream:\n  url: http://a\n  timeout: 10s\n",
			wantErr: "must be less than server.write_timeout",
		},
		{
			name:    "non-positive body limit",
			yml:     "server:\n  max_body_bytes: 0\nupstream:\n  url: http://a\n",
			wantErr: "server.max_body_bytes must be positive",
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
	cfg.Upstream.URL = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded, want error")
	}
	for _, want := range []string{"server.listen is required", "upstream.url: is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte("upstream:\n  url: http://a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("Load of missing file succeeded, want error")
	}
}
