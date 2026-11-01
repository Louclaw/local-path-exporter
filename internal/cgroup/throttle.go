package cgroup

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	// DefaultThrottleHigh is the memory.current level where a full scan waits.
	DefaultThrottleHigh = 100 << 20
	// DefaultThrottleBatch is how many walked entries elapse between checks.
	DefaultThrottleBatch = 8192
	// DefaultMaxPerSec is a hard cap on walked entries, independent of memory.current.
	// The memory threshold does not fire while the cgroup is still under it, and a
	// burst there is what filled hel1 before reclaim could run.
	DefaultMaxPerSec = 4000
	minThrottleSleep = 20 * time.Millisecond
	maxThrottleSleep = 200 * time.Millisecond
	maxThrottlePause = 500 * time.Millisecond
)

// Throttle slows every walk. Two independent brakes: a hard entries/sec cap,
// and a backoff while memory.current stays above High. memory.high is max on
// a normal kubelet cgroup, so the memory backoff only helps once the walk is
// already near memory.max.
type Throttle struct {
	High      uint64
	Batch     int64
	MaxPerSec int64

	current func() (uint64, bool)

	pauses    atomic.Int64
	paused    atomic.Int64 // nanoseconds, process lifetime
	last      atomic.Int64 // nanoseconds in the most recent Check burst
	rateStart atomic.Int64 // unix nanoseconds of the first paced entry
}

// NewThrottle returns a throttler. high or batch <= 0 disables it.
func NewThrottle(high uint64, batch int64) *Throttle {
	return &Throttle{High: high, Batch: batch, current: readCurrent}
}

// SetCurrentForTest replaces the memory.current reader.
func (t *Throttle) SetCurrentForTest(fn func() (uint64, bool)) {
	if t == nil {
		return
	}
	t.current = fn
}

// Pauses is the number of backoff sleeps since process start.
func (t *Throttle) Pauses() int64 {
	if t == nil {
		return 0
	}
	return t.pauses.Load()
}

// Paused is the time spent backing off since process start.
func (t *Throttle) Paused() time.Duration {
	if t == nil {
		return 0
	}
	return time.Duration(t.paused.Load())
}

// LastPaused is the time spent in the most recent check that actually slept.
func (t *Throttle) LastPaused() time.Duration {
	if t == nil {
		return 0
	}
	return time.Duration(t.last.Load())
}

// Check applies the entries/sec cap and, when memory.current is above High,
// a capped backoff. extend receives each sleep so the scan timeout does not count it.
func (t *Throttle) Check(ctx context.Context, entries int64, extend func(time.Duration)) {
	if t == nil || entries <= 0 {
		return
	}
	step := t.Batch
	if step <= 0 {
		step = 1024
	}
	if entries%step != 0 {
		return
	}
	t.limitRate(ctx, entries, extend)
	if t.High == 0 || t.Batch <= 0 {
		return
	}
	curFn := t.current
	if curFn == nil {
		curFn = readCurrent
	}
	delay := minThrottleSleep
	var waited time.Duration
	for waited < maxThrottlePause {
		cur, ok := curFn()
		if !ok || cur <= t.High {
			t.finish(waited)
			return
		}
		step := delay
		if step > maxThrottleSleep {
			step = maxThrottleSleep
		}
		if waited+step > maxThrottlePause {
			step = maxThrottlePause - waited
		}
		if step <= 0 {
			break
		}
		if extend != nil {
			extend(step)
		}
		t.pauses.Add(1)
		timer := time.NewTimer(step)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.finish(waited)
			return
		case <-timer.C:
		}
		waited += step
		if delay < maxThrottleSleep {
			delay *= 2
		}
	}
	t.finish(waited)
}

// limitRate sleeps so the walk cannot exceed MaxPerSec, even when
// memory.current is still under the threshold.
func (t *Throttle) limitRate(ctx context.Context, entries int64, extend func(time.Duration)) {
	if t.MaxPerSec <= 0 {
		return
	}
	now := time.Now()
	start := t.rateStart.Load()
	if start == 0 {
		t.rateStart.CompareAndSwap(0, now.UnixNano())
		start = t.rateStart.Load()
	}
	var waited time.Duration
	for {
		elapsed := time.Since(time.Unix(0, start))
		want := time.Duration(entries) * time.Second / time.Duration(t.MaxPerSec)
		if want <= elapsed {
			break
		}
		d := want - elapsed
		// Sleep in short steps so a cancelled scan is not stuck, without
		// capping the total wait: the cap is the whole point of the rate.
		if d > time.Second {
			d = time.Second
		}
		if extend != nil {
			extend(d)
		}
		t.pauses.Add(1)
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.finish(waited)
			return
		case <-timer.C:
		}
		waited += d
	}
	t.finish(waited)
}

func (t *Throttle) finish(waited time.Duration) {
	if waited <= 0 {
		return
	}
	t.paused.Add(int64(waited))
	t.last.Store(int64(waited))
}

func readCurrent() (uint64, bool) {
	s := Read()
	if !s.OK {
		return 0, false
	}
	return s.Current, true
}
