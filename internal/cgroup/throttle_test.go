package cgroup

import (
	"context"
	"testing"
	"time"
)

func TestThrottleBackoffIsBounded(t *testing.T) {
	var reads int
	th := NewThrottle(100, 1)
	th.SetCurrentForTest(func() (uint64, bool) {
		reads++
		if reads < 4 {
			return 120 << 20, true
		}
		return 50 << 20, true
	})
	start := time.Now()
	th.Check(context.Background(), 1, nil)
	elapsed := time.Since(start)
	if th.Pauses() < 1 {
		t.Fatal("expected a pause above the threshold")
	}
	if elapsed > time.Second {
		t.Fatalf("backoff not bounded: %s pauses=%d", elapsed, th.Pauses())
	}
	if th.LastPaused() <= 0 {
		t.Fatal("pause time not recorded")
	}
}

func TestThrottleStopsWhenUnder(t *testing.T) {
	th := NewThrottle(100, 10)
	th.SetCurrentForTest(func() (uint64, bool) { return 10, true })
	th.Check(context.Background(), 10, nil)
	if th.Pauses() != 0 {
		t.Fatalf("pauses = %d", th.Pauses())
	}
}
