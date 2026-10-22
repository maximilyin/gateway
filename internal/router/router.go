// Package router matches request paths against configured patterns.
//
// Two pattern kinds are supported:
//
//	/service/users/me   exact: matches only this path
//	/service/users/*    prefix: matches /service/users and everything below it,
//	                    on a segment boundary (/service/usersX does not match)
//
// The most specific route wins regardless of declaration order: an exact match
// first, then the longest prefix. A route whose path matches but whose methods do
// not is skipped in favour of a less specific one.
package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// Route describes one routing rule.
type Route struct {
	// Name identifies the route in logs; defaults to the pattern.
	Name    string
	Pattern string
	// Methods restricts the route to these HTTP methods; empty means any method.
	// Allowing GET implicitly allows HEAD.
	Methods []string
	// StripPrefix is removed from the path before the request is forwarded.
	StripPrefix string
	Handler     http.Handler
}

type route struct {
	Route
	methods map[string]bool // nil means any method
}

func (r *route) allows(method string) bool {
	return r.methods == nil || r.methods[method]
}

type Router struct {
	exact  map[string][]*route // key: full path
	prefix map[string][]*route // key: pattern without the trailing "/*"
}

func New(routes []Route) (*Router, error) {
	rt := &Router{
		exact:  make(map[string][]*route),
		prefix: make(map[string][]*route),
	}

	var errs []error
	for i, cfg := range routes {
		if err := rt.add(cfg); err != nil {
			errs = append(errs, fmt.Errorf("route %d (%s): %w", i, cfg.Pattern, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return rt, nil
}

func (rt *Router) add(cfg Route) error {
	base, isPrefix, err := parsePattern(cfg.Pattern)
	if err != nil {
		return err
	}
	if cfg.Handler == nil {
		return errors.New("handler is required")
	}
	if err := validateStripPrefix(cfg.StripPrefix, base); err != nil {
		return err
	}
	methods, err := parseMethods(cfg.Methods)
	if err != nil {
		return err
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Pattern
	}

	r := &route{Route: cfg, methods: methods}

	bucket := rt.exact
	if isPrefix {
		bucket = rt.prefix
	}
	for _, other := range bucket[base] {
		if overlaps(r.methods, other.methods) {
			return fmt.Errorf("conflicts with route %q: same pattern and overlapping methods", other.Name)
		}
	}
	bucket[base] = append(bucket[base], r)
	return nil
}

// Match finds the route for a request. When no route matches, allowed lists the
// methods accepted by routes whose path did match, so the caller can tell a 405
// apart from a 404.
func (rt *Router) Match(method, path string) (r *Route, allowed []string) {
	if !strings.HasPrefix(path, "/") {
		return nil, nil // e.g. "OPTIONS *"
	}

	var allowedSet map[string]bool

	try := func(candidates []*route) *Route {
		for _, c := range candidates {
			if c.allows(method) {
				return &c.Route
			}
			if allowedSet == nil {
				allowedSet = make(map[string]bool)
			}
			for m := range c.methods {
				allowedSet[m] = true
			}
		}
		return nil
	}

	if found := try(rt.exact[path]); found != nil {
		return found, nil
	}

	// Walk from the full path up through its parents: /a/b/c, /a/b, /a, "".
	// Prefix routes are keyed by their base, so the first hit is the longest prefix.
	for p := path; ; {
		if found := try(rt.prefix[p]); found != nil {
			return found, nil
		}
		if p == "" {
			break
		}
		p = p[:strings.LastIndexByte(p, '/')]
	}

	allowed = make([]string, 0, len(allowedSet))
	for m := range allowedSet {
		allowed = append(allowed, m)
	}
	sort.Strings(allowed)
	return nil, allowed
}

// ServeHTTP dispatches the request to the matched route's handler, or answers
// 404 / 405 itself.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, allowed := rt.Match(r.Method, r.URL.Path)
	if route == nil {
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		http.NotFound(w, r)
		return
	}

	r = r.WithContext(context.WithValue(r.Context(), routeKey{}, route))
	if route.StripPrefix != "" {
		r = withStrippedPrefix(r, route.StripPrefix)
	}
	route.Handler.ServeHTTP(w, r)
}

type routeKey struct{}

// RouteFrom returns the route matched for this request, or nil.
func RouteFrom(ctx context.Context) *Route {
	r, _ := ctx.Value(routeKey{}).(*Route)
	return r
}

// withStrippedPrefix returns a shallow copy of r with prefix removed from the
// path. The URL is copied too, because the original request must stay untouched.
func withStrippedPrefix(r *http.Request, prefix string) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	u := new(url.URL)
	*u = *r.URL
	r2.URL = u

	u.Path = trimPathPrefix(u.Path, prefix)
	if u.RawPath != "" {
		// RawPath is only used when it is a valid encoding of Path; if it no longer
		// is, url.URL falls back to re-encoding Path, so a mismatch here is safe.
		u.RawPath = trimPathPrefix(u.RawPath, prefix)
	}
	return r2
}

func trimPathPrefix(p, prefix string) string {
	p = strings.TrimPrefix(p, prefix)
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	return p
}

// parsePattern validates a pattern and returns the path it is keyed by: the full
// path for exact patterns, the part before "/*" for prefix ones ("" for "/*").
func parsePattern(pattern string) (base string, isPrefix bool, err error) {
	if !strings.HasPrefix(pattern, "/") {
		return "", false, errors.New(`pattern must start with "/"`)
	}

	base = pattern
	if strings.HasSuffix(pattern, "/*") {
		base = strings.TrimSuffix(pattern, "/*")
		isPrefix = true
	}
	if err := validatePath(base, isPrefix); err != nil {
		return "", false, err
	}
	return base, isPrefix, nil
}

// validatePath rejects paths that can never match a cleaned request path
// ("//", "." and ".." segments) and characters that only appear percent-encoded.
func validatePath(p string, isPrefixBase bool) error {
	for i := 0; i < len(p); i++ {
		if !isPathChar(p[i]) {
			return fmt.Errorf("invalid character %q in pattern (only the trailing \"/*\" wildcard is supported)", p[i])
		}
	}
	segments := strings.Split(p, "/")[1:] // p starts with "/", so the first element is ""
	for i, seg := range segments {
		last := i == len(segments)-1
		switch {
		case seg == "" && last && !isPrefixBase:
			// trailing slash in an exact pattern, e.g. "/docs/"
		case seg == "":
			return errors.New(`pattern must not contain empty segments ("//")`)
		case seg == "." || seg == "..":
			return errors.New(`pattern must not contain "." or ".." segments`)
		}
	}
	return nil
}

func isPathChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("/-._~!$&'()+,;=:@", c) >= 0
}

func validateStripPrefix(strip, base string) error {
	if strip == "" {
		return nil
	}
	if !strings.HasPrefix(strip, "/") || strings.HasSuffix(strip, "/") {
		return fmt.Errorf(`strip_prefix %q must start with "/" and must not end with "/"`, strip)
	}
	if base != strip && !strings.HasPrefix(base, strip+"/") {
		return fmt.Errorf("strip_prefix %q is not a path prefix of the pattern", strip)
	}
	return nil
}

var knownMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

func parseMethods(list []string) (map[string]bool, error) {
	if len(list) == 0 {
		return nil, nil
	}
	set := make(map[string]bool, len(list)+1)
	for _, m := range list {
		m = strings.ToUpper(m)
		if !slices.Contains(knownMethods, m) {
			return nil, fmt.Errorf("unsupported method %q", m)
		}
		set[m] = true
	}
	if set[http.MethodGet] {
		set[http.MethodHead] = true
	}
	return set, nil
}

// overlaps reports whether two method sets accept at least one common method.
func overlaps(a, b map[string]bool) bool {
	if a == nil || b == nil {
		return true
	}
	for m := range a {
		if b[m] {
			return true
		}
	}
	return false
}
