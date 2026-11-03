// Package metrics keeps counters and gauges for this process and renders them
// in the Prometheus text format. Reading GET /metrics is enough to see the
// numbers; a Prometheus server and Grafana are optional.
package metrics

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry is a set of counters and gauges. It is safe for concurrent use.
type Registry struct {
	mu     sync.Mutex
	byName map[string]*family
	order  []*family
}

func New() *Registry {
	return &Registry{byName: map[string]*family{}}
}

// Counter returns the counter with this name, creating it on first use.
// The same name must not also be used as a gauge.
func (r *Registry) Counter(name, help string) *Counter {
	return &Counter{fam: r.family(name, help, "counter")}
}

// Gauge registers a value that is read when the registry is rendered.
// labels is copied. Several gauges may share a name when their labels differ.
func (r *Registry) Gauge(name, help string, labels map[string]string, read func() float64) {
	r.family(name, help, "gauge").setGauge(labels, read)
}

// ServeHTTP writes the Prometheus text exposition.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(r.Text()))
}

// Text renders every series. A counter appears only after it has been incremented.
func (r *Registry) Text() string {
	r.mu.Lock()
	fams := append([]*family(nil), r.order...)
	r.mu.Unlock()

	var b strings.Builder
	for _, f := range fams {
		samples := f.snapshot()
		if len(samples) == 0 {
			continue
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i].labels < samples[j].labels })
		b.WriteString("# HELP ")
		b.WriteString(f.name)
		b.WriteByte(' ')
		b.WriteString(escapeHelp(f.help))
		b.WriteByte('\n')
		b.WriteString("# TYPE ")
		b.WriteString(f.name)
		b.WriteByte(' ')
		b.WriteString(f.kind)
		b.WriteByte('\n')
		for _, s := range samples {
			b.WriteString(f.name)
			b.WriteString(s.labels)
			b.WriteByte(' ')
			b.WriteString(strconv.FormatFloat(s.value, 'f', -1, 64))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (r *Registry) family(name, help, kind string) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.byName[name]; ok {
		if f.kind != kind {
			panic("metrics: " + name + " is already a " + f.kind)
		}
		return f
	}
	f := &family{name: name, help: help, kind: kind, series: map[string]*series{}}
	r.byName[name] = f
	r.order = append(r.order, f)
	return f
}

// Counter is a cumulative total since the process started.
type Counter struct {
	fam *family
}

// Inc adds one to the series identified by labels. The map is not retained.
func (c *Counter) Inc(labels map[string]string) {
	c.fam.add(labels)
}

type family struct {
	name, help, kind string
	mu               sync.Mutex
	series           map[string]*series
}

type series struct {
	labels string
	val    atomic.Int64
	read   func() float64
}

type sample struct {
	labels string
	value  float64
}

func (f *family) add(labels map[string]string) {
	key, text := labelSet(labels)
	f.mu.Lock()
	s, ok := f.series[key]
	if !ok {
		s = &series{labels: text}
		f.series[key] = s
	}
	f.mu.Unlock()
	s.val.Add(1)
}

func (f *family) setGauge(labels map[string]string, read func() float64) {
	key, text := labelSet(labels)
	f.mu.Lock()
	f.series[key] = &series{labels: text, read: read}
	f.mu.Unlock()
}

func (f *family) snapshot() []sample {
	f.mu.Lock()
	copied := make([]*series, 0, len(f.series))
	for _, s := range f.series {
		copied = append(copied, s)
	}
	f.mu.Unlock()

	out := make([]sample, 0, len(copied))
	for _, s := range copied {
		v := float64(s.val.Load())
		if s.read != nil {
			v = s.read()
		}
		out = append(out, sample{labels: s.labels, value: v})
	}
	return out
}

type pair struct {
	name, value string
}

func labelSet(labels map[string]string) (key, text string) {
	if len(labels) == 0 {
		return "", ""
	}
	pairs := make([]pair, 0, len(labels))
	for name, value := range labels {
		pairs = append(pairs, pair{name, value})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })

	var keyb, textb strings.Builder
	textb.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			textb.WriteByte(',')
		}
		keyb.WriteString(p.name)
		keyb.WriteByte(0)
		keyb.WriteString(p.value)
		keyb.WriteByte(0)
		textb.WriteString(p.name)
		textb.WriteString(`="`)
		textb.WriteString(escapeLabel(p.value))
		textb.WriteByte('"')
	}
	textb.WriteByte('}')
	return keyb.String(), textb.String()
}

func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
