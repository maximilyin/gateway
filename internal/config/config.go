// Package config loads and validates the gateway configuration from YAML.
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Server     ServerConfig              `yaml:"server"`
	Log        LogConfig                 `yaml:"log"`
	Sessions   SessionsConfig            `yaml:"sessions"`
	RateLimits RateLimitsConfig          `yaml:"rate_limits"`
	Upstreams  map[string]UpstreamConfig `yaml:"upstreams"`
	Routes     []RouteConfig             `yaml:"routes"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`

	// ReadHeaderTimeout bounds how long a client may take to send request headers
	// (slowloris protection). ReadTimeout also covers the body.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	// WriteTimeout caps the whole response, including the time spent waiting for the
	// upstream, so it must be larger than any upstream timeout. Zero disables it.
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`

	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes"`
}

type LogConfig struct {
	Level slog.Level `yaml:"level"`
}

type SessionsConfig struct {
	// Endpoint serves POST (issue), GET (info) and DELETE (revoke).
	Endpoint string `yaml:"endpoint"`
	// Header is the request header that carries the session token.
	Header string `yaml:"header"`
	// TTL is the idle timeout, extended on every use up to MaxLifetime.
	TTL         time.Duration `yaml:"ttl"`
	MaxLifetime time.Duration `yaml:"max_lifetime"`
	// MaxSessions bounds memory use; issuing beyond it fails with 503.
	MaxSessions     int           `yaml:"max_sessions"`
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
}

// RateLimitConfig is one token bucket: burst requests may pass immediately,
// then requests refill at rps per second.
type RateLimitConfig struct {
	RPS   float64 `yaml:"rps"`
	Burst int     `yaml:"burst"`
}

// RateLimitsConfig holds the process-wide guard, the per-client default, the
// per-IP session issuance limit, and optional named policies routes can select.
type RateLimitsConfig struct {
	Global       RateLimitConfig            `yaml:"global"`
	Default      RateLimitConfig            `yaml:"default"`
	SessionIssue RateLimitConfig            `yaml:"session_issue"`
	Policies     map[string]RateLimitConfig `yaml:"policies"`
	// IdleTTL is how long an unused bucket is kept. After that the client starts
	// from a full bucket again.
	IdleTTL         time.Duration `yaml:"idle_ttl"`
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
	MaxKeys         int           `yaml:"max_keys"`
}

type UpstreamConfig struct {
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"timeout"`
}

// RouteConfig maps a path pattern to an upstream. Pattern syntax and precedence
// are described in package router.
type RouteConfig struct {
	Name        string   `yaml:"name"`
	Match       string   `yaml:"match"`
	Methods     []string `yaml:"methods"`
	Upstream    string   `yaml:"upstream"`
	StripPrefix string   `yaml:"strip_prefix"`
	// RateLimit names a policy under rate_limits.policies. Empty uses default.
	RateLimit string `yaml:"rate_limit"`
	// RequireSession documents that the route is session-scoped. Every matched
	// route already receives a session: the first request without a live token
	// is issued one before the upstream call.
	RequireSession bool `yaml:"require_session"`
}

const DefaultUpstreamTimeout = 30 * time.Second

// Default returns a config with every optional field filled in. Parse decodes YAML
// on top of it, so anything missing in the file keeps its default value.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:            ":8080",
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			ShutdownTimeout:   15 * time.Second,
			MaxBodyBytes:      10 << 20, // 10 MiB
		},
		Log: LogConfig{Level: slog.LevelInfo},
		Sessions: SessionsConfig{
			Endpoint:        "/auth/session",
			Header:          "X-Session-Token",
			TTL:             30 * time.Minute,
			MaxLifetime:     24 * time.Hour,
			MaxSessions:     100_000,
			CleanupInterval: time.Minute,
		},
		RateLimits: RateLimitsConfig{
			Global:          RateLimitConfig{RPS: 5000, Burst: 10000},
			Default:         RateLimitConfig{RPS: 10, Burst: 20},
			SessionIssue:    RateLimitConfig{RPS: 1, Burst: 5},
			IdleTTL:         30 * time.Minute,
			CleanupInterval: time.Minute,
			MaxKeys:         100_000,
		},
	}
}

// applyDefaults fills defaults that Default cannot pre-populate: entries of maps
// and slices only exist after the YAML has been decoded.
func (c *Config) applyDefaults() {
	for name, u := range c.Upstreams {
		if u.Timeout == 0 {
			u.Timeout = DefaultUpstreamTimeout
			c.Upstreams[name] = u
		}
	}
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	return Parse(f)
}

func Parse(r io.Reader) (*Config, error) {
	cfg := Default()

	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("parse config: file is empty")
		}
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// Validate reports every problem at once instead of stopping at the first one.
func (c *Config) Validate() error {
	var errs []error

	s := c.Server
	if s.Listen == "" {
		errs = append(errs, errors.New("server.listen is required"))
	}
	if s.ReadHeaderTimeout <= 0 {
		errs = append(errs, errors.New("server.read_header_timeout must be positive"))
	}
	if s.ReadTimeout < 0 || s.WriteTimeout < 0 || s.IdleTimeout < 0 {
		errs = append(errs, errors.New("server timeouts must not be negative"))
	}
	if s.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("server.shutdown_timeout must be positive"))
	}
	if s.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("server.max_body_bytes must be positive"))
	}

	errs = append(errs, c.Sessions.validate()...)
	errs = append(errs, c.RateLimits.validate()...)

	if len(c.Upstreams) == 0 {
		errs = append(errs, errors.New("at least one upstream is required"))
	}
	// Map iteration order is random in Go; sort names so errors are deterministic.
	for _, name := range slices.Sorted(maps.Keys(c.Upstreams)) {
		u := c.Upstreams[name]
		if _, err := ParseUpstreamURL(u.URL); err != nil {
			errs = append(errs, fmt.Errorf("upstreams.%s.url: %w", name, err))
		}
		if u.Timeout <= 0 {
			errs = append(errs, fmt.Errorf("upstreams.%s.timeout must be positive", name))
		}
		if s.WriteTimeout > 0 && u.Timeout >= s.WriteTimeout {
			errs = append(errs, fmt.Errorf(
				"upstreams.%s.timeout (%s) must be less than server.write_timeout (%s)",
				name, u.Timeout, s.WriteTimeout))
		}
	}

	if len(c.Routes) == 0 {
		errs = append(errs, errors.New("at least one route is required"))
	}
	for i, r := range c.Routes {
		if r.Match == "" {
			errs = append(errs, fmt.Errorf("routes[%d].match is required", i))
		}
		if r.Upstream == "" {
			errs = append(errs, fmt.Errorf("routes[%d].upstream is required", i))
		} else if _, ok := c.Upstreams[r.Upstream]; !ok {
			errs = append(errs, fmt.Errorf("routes[%d].upstream: unknown upstream %q", i, r.Upstream))
		}
		if r.RateLimit != "" {
			if _, ok := c.RateLimits.Policies[r.RateLimit]; !ok {
				errs = append(errs, fmt.Errorf("routes[%d].rate_limit: unknown policy %q", i, r.RateLimit))
			}
		}
	}

	return errors.Join(errs...)
}

func (c RateLimitsConfig) validate() []error {
	var errs []error
	errs = append(errs, validateLimit("rate_limits.global", c.Global)...)
	errs = append(errs, validateLimit("rate_limits.default", c.Default)...)
	errs = append(errs, validateLimit("rate_limits.session_issue", c.SessionIssue)...)
	if c.IdleTTL <= 0 {
		errs = append(errs, errors.New("rate_limits.idle_ttl must be positive"))
	}
	if c.CleanupInterval <= 0 {
		errs = append(errs, errors.New("rate_limits.cleanup_interval must be positive"))
	}
	if c.MaxKeys <= 0 {
		errs = append(errs, errors.New("rate_limits.max_keys must be positive"))
	}
	for _, name := range slices.Sorted(maps.Keys(c.Policies)) {
		switch name {
		case "global", "default", "session_issue":
			errs = append(errs, fmt.Errorf("rate_limits.policies.%s: reserved name", name))
		}
		if strings.ContainsAny(name, " \t\r\n") || name == "" {
			errs = append(errs, fmt.Errorf("rate_limits.policies: invalid name %q", name))
		}
		errs = append(errs, validateLimit("rate_limits.policies."+name, c.Policies[name])...)
	}
	return errs
}

func validateLimit(where string, l RateLimitConfig) []error {
	var errs []error
	if l.RPS <= 0 {
		errs = append(errs, fmt.Errorf("%s.rps must be positive", where))
	}
	if l.Burst < 1 {
		errs = append(errs, fmt.Errorf("%s.burst must be at least 1", where))
	}
	return errs
}

func (s SessionsConfig) validate() []error {
	var errs []error
	if !validEndpoint(s.Endpoint) {
		errs = append(errs, fmt.Errorf("sessions.endpoint %q must be a plain absolute path like /auth/session", s.Endpoint))
	}
	if s.Header == "" || strings.ContainsAny(s.Header, " :\t\r\n") {
		errs = append(errs, fmt.Errorf("sessions.header %q is not a valid header name", s.Header))
	}
	if s.TTL <= 0 {
		errs = append(errs, errors.New("sessions.ttl must be positive"))
	}
	if s.MaxLifetime < s.TTL {
		errs = append(errs, fmt.Errorf("sessions.max_lifetime (%s) must not be less than sessions.ttl (%s)", s.MaxLifetime, s.TTL))
	}
	if s.MaxSessions <= 0 {
		errs = append(errs, errors.New("sessions.max_sessions must be positive"))
	}
	if s.CleanupInterval <= 0 {
		errs = append(errs, errors.New("sessions.cleanup_interval must be positive"))
	}
	return errs
}

// validEndpoint accepts paths made of letters, digits and "/-._~", so they can be
// used verbatim in http.ServeMux patterns (which give "{", "}" special meaning).
func validEndpoint(p string) bool {
	if len(p) < 2 || p[0] != '/' || strings.HasSuffix(p, "/") || strings.Contains(p, "//") {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("/-._~", c) >= 0) {
			return false
		}
	}
	return true
}

func ParseUpstreamURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("host is required")
	}
	return u, nil
}
