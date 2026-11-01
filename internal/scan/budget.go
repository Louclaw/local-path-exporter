package scan

import (
	"context"
	"sync"
	"time"

	"github.com/Louclaw/local-path-exporter/internal/cgroup"
)

// budget is the scan timeout. Pauses that wait for memcg reclaim extend it,
// so SCAN_TIMEOUT measures walk time and not voluntary backoff.
type budget struct {
	mu       sync.Mutex
	deadline time.Time
	timer    *time.Timer
	cancel   context.CancelFunc
}

// budgetCtx is cancelled when the parent is cancelled or the deadline fires.
// extendBudget pushes the deadline and the timer, so a voluntary pause does
// not surface as DeadlineExceeded.
type budgetCtx struct {
	context.Context
	b      *budget
	parent context.Context
}

type budgetKey struct{}

// WithBudget returns a context whose deadline moves forward when the walk
// pauses above the memory threshold. Cancel stops the timer.
func WithBudget(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	b := &budget{cancel: cancel}
	out := &budgetCtx{Context: ctx, b: b, parent: parent}
	if timeout > 0 {
		b.deadline = time.Now().Add(timeout)
		b.timer = time.AfterFunc(timeout, b.fire)
	}
	stop := func() {
		b.mu.Lock()
		if b.timer != nil {
			b.timer.Stop()
		}
		b.mu.Unlock()
		cancel()
	}
	return context.WithValue(out, budgetKey{}, b), stop
}

func (b *budget) fire() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deadline.IsZero() || time.Now().Before(b.deadline) {
		if b.timer == nil || b.deadline.IsZero() {
			return
		}
		wait := time.Until(b.deadline)
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		b.timer.Reset(wait)
		return
	}
	b.cancel()
}

func (c *budgetCtx) Deadline() (time.Time, bool) {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	if c.b.deadline.IsZero() {
		return time.Time{}, false
	}
	return c.b.deadline, true
}

func (c *budgetCtx) Err() error {
	if err := c.parent.Err(); err != nil {
		return err
	}
	if err := c.Context.Err(); err != nil {
		c.b.mu.Lock()
		expired := !c.b.deadline.IsZero() && !time.Now().Before(c.b.deadline)
		c.b.mu.Unlock()
		if expired {
			return context.DeadlineExceeded
		}
		return err
	}
	return nil
}

func budgetErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := ctx.Value(budgetKey{}).(*budget)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.deadline.IsZero() && time.Now().After(b.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

type throttleKey struct{}
type throttleActiveKey struct{}

// WithThrottle attaches a throttler. It sleeps only during a full walk.
func WithThrottle(ctx context.Context, t *cgroup.Throttle) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, throttleKey{}, t)
}

func activateThrottle(ctx context.Context) context.Context {
	if ctx == nil || ctx.Value(throttleKey{}) == nil {
		return ctx
	}
	if ctx.Value(throttleActiveKey{}) == true {
		return ctx
	}
	return context.WithValue(ctx, throttleActiveKey{}, true)
}

func noteProgress(ctx context.Context, entries int64) {
	cgroup.Note(entries)
	if ctx == nil || ctx.Value(throttleActiveKey{}) != true {
		return
	}
	t, _ := ctx.Value(throttleKey{}).(*cgroup.Throttle)
	if t == nil {
		return
	}
	t.Check(ctx, entries, func(d time.Duration) { extendBudget(ctx, d) })
}

func extendBudget(ctx context.Context, d time.Duration) {
	b, _ := ctx.Value(budgetKey{}).(*budget)
	if b == nil || d <= 0 {
		return
	}
	// A few milliseconds of slack keep the timer from winning a race with the sleep.
	d += 5 * time.Millisecond
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deadline.IsZero() {
		return
	}
	b.deadline = b.deadline.Add(d)
	if b.timer == nil {
		return
	}
	b.timer.Stop()
	wait := time.Until(b.deadline)
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	b.timer.Reset(wait)
}
