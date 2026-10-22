package clock

import (
	"testing"
	"time"
)

func TestFake(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewFake(start)

	if !c.Now().Equal(start) {
		t.Fatalf("Now = %s, want %s", c.Now(), start)
	}
	c.Advance(90 * time.Second)
	if want := start.Add(90 * time.Second); !c.Now().Equal(want) {
		t.Fatalf("Now after Advance = %s, want %s", c.Now(), want)
	}
}

func TestReal(t *testing.T) {
	before := time.Now()
	got := Real{}.Now()
	if got.Before(before) {
		t.Fatalf("Real.Now = %s is before %s", got, before)
	}
}
