package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistry_Text(t *testing.T) {
	reg := New()
	hits := reg.Counter("demo_total", "How many times something happened.")
	hits.Inc(map[string]string{"code": "429"})
	hits.Inc(map[string]string{"code": "429"})
	hits.Inc(map[string]string{"code": "a\"b\\c\n"})
	reg.Gauge("demo_state", "Current state.", map[string]string{"upstream": "users"}, func() float64 { return 2 })
	reg.Gauge("demo_plain", "No labels.", nil, func() float64 { return 0 })

	got := reg.Text()
	for _, want := range []string{
		"# HELP demo_total How many times something happened.",
		"# TYPE demo_total counter",
		`demo_total{code="429"} 2`,
		`demo_total{code="a\"b\\c\n"} 1`,
		"# TYPE demo_state gauge",
		`demo_state{upstream="users"} 2`,
		"demo_plain 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("text missing %q\n%s", want, got)
		}
	}

	// A counter that nobody incremented is omitted.
	reg.Counter("demo_quiet_total", "Never incremented.")
	if strings.Contains(reg.Text(), "demo_quiet_total") {
		t.Fatal("quiet counter was rendered")
	}
}

func TestObserveStatus(t *testing.T) {
	reg := New()
	h := ObserveStatus(reg.Counter("responses_total", "Responses."))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !strings.Contains(reg.Text(), `responses_total{code="429"} 1`) {
		t.Fatalf("text = %s", reg.Text())
	}
}
