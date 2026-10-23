package breaker

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/maximilyin/gateway/internal/clock"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newTestBreaker(clk clock.Clock, failures, probes int, open time.Duration) *Breaker {
	return New(Settings{
		FailureThreshold: failures,
		OpenTimeout:      open,
		HalfOpenMax:      probes,
	}, clk, slog.New(slog.DiscardHandler))
}

func TestBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	clk := clock.NewFake(t0)
	b := newTestBreaker(clk, 3, 1, time.Minute)

	for range 2 {
		permit, _, ok := b.Allow()
		if !ok {
			t.Fatal("closed breaker refused a request")
		}
		b.Report(permit, Failure)
	}
	if b.State() != Closed {
		t.Fatalf("state = %s, want closed", b.State())
	}

	permit, _, _ := b.Allow()
	b.Report(permit, Failure)
	if b.State() != Open {
		t.Fatalf("state = %s, want open", b.State())
	}

	if _, _, ok := b.Allow(); ok {
		t.Fatal("open breaker allowed a request")
	}
}

func TestBreaker_SuccessResetsFailures(t *testing.T) {
	clk := clock.NewFake(t0)
	b := newTestBreaker(clk, 3, 1, time.Minute)

	permit, _, _ := b.Allow()
	b.Report(permit, Failure)
	permit, _, _ = b.Allow()
	b.Report(permit, Failure)
	permit, _, _ = b.Allow()
	b.Report(permit, Success)

	for range 2 {
		permit, _, _ = b.Allow()
		b.Report(permit, Failure)
	}
	if b.State() != Closed {
		t.Fatalf("state = %s, want closed after the counter was reset", b.State())
	}
}

func TestBreaker_IgnoredDoesNotCount(t *testing.T) {
	clk := clock.NewFake(t0)
	b := newTestBreaker(clk, 2, 1, time.Minute)

	for range 5 {
		permit, _, _ := b.Allow()
		b.Report(permit, Ignored)
	}
	if b.State() != Closed {
		t.Fatalf("state = %s, want closed", b.State())
	}
}

func TestBreaker_HalfOpenProbe(t *testing.T) {
	clk := clock.NewFake(t0)
	b := newTestBreaker(clk, 1, 2, 30*time.Second)

	permit, _, _ := b.Allow()
	b.Report(permit, Failure)
	if _, retry, ok := b.Allow(); ok || retry != 30*time.Second {
		t.Fatalf("open allow = %v retry %s, want refused for 30s", ok, retry)
	}

	clk.Advance(30 * time.Second)
	if b.State() != HalfOpen {
		t.Fatalf("state = %s, want half-open", b.State())
	}

	p1, _, ok1 := b.Allow()
	p2, _, ok2 := b.Allow()
	if !ok1 || !ok2 {
		t.Fatal("half-open refused a probe")
	}
	if _, _, ok := b.Allow(); ok {
		t.Fatal("half-open allowed more probes than half_open_max")
	}

	b.Report(p1, Success)
	if b.State() != HalfOpen {
		t.Fatalf("state after one success = %s, want half-open", b.State())
	}
	b.Report(p2, Success)
	if b.State() != Closed {
		t.Fatalf("state = %s, want closed", b.State())
	}
}

func TestBreaker_ProbeFailureReopens(t *testing.T) {
	clk := clock.NewFake(t0)
	b := newTestBreaker(clk, 1, 2, time.Second)

	permit, _, _ := b.Allow()
	b.Report(permit, Failure)
	clk.Advance(time.Second)

	p1, _, _ := b.Allow()
	p2, _, _ := b.Allow()
	b.Report(p1, Failure)
	if b.State() != Open {
		t.Fatalf("state = %s, want open", b.State())
	}
	// The other probe belongs to the generation that already ended.
	b.Report(p2, Success)
	if b.State() != Open {
		t.Fatalf("stale success closed the breaker, state = %s", b.State())
	}

	clk.Advance(time.Second)
	if _, _, ok := b.Allow(); !ok || b.State() != HalfOpen {
		t.Fatal("breaker did not accept a new probe after the second open window")
	}
}

func TestBreaker_CanceledProbeReleasesSlot(t *testing.T) {
	clk := clock.NewFake(t0)
	b := newTestBreaker(clk, 1, 1, time.Second)

	permit, _, _ := b.Allow()
	b.Report(permit, Failure)
	clk.Advance(time.Second)

	probe, _, ok := b.Allow()
	if !ok {
		t.Fatal("probe was refused")
	}
	if _, _, ok := b.Allow(); ok {
		t.Fatal("second probe was allowed while the first is in flight")
	}
	b.Report(probe, Ignored)
	if _, _, ok := b.Allow(); !ok {
		t.Fatal("slot stayed taken after an ignored probe")
	}
}

func TestBreaker_ConcurrentFailuresTripOnce(t *testing.T) {
	b := newTestBreaker(clock.NewFake(t0), 32, 1, time.Minute)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			permit, _, ok := b.Allow()
			if ok {
				b.Report(permit, Failure)
			}
		}()
	}
	wg.Wait()
	if b.State() != Open {
		t.Fatalf("state = %s, want open", b.State())
	}
}
