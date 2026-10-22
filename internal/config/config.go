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
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Server    ServerConfig              `yaml:"server"`
	Log       LogConfig                 `yaml:"log"`
	Upstreams map[string]UpstreamConfig `yaml:"upstreams"`
	Routes    []RouteConfig             `yaml:"routes"`
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
	}

	return errors.Join(errs...)
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
