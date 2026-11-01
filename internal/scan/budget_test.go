package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Louclaw/local-path-exporter/internal/cgroup"
)

func TestThrottlePauseDoesNotExpireBudget(t *testing.T) {
	ctx, cancel := WithBudget(context.Background(), 40*time.Millisecond)
	defer cancel()
	th := cgroup.NewThrottle(1, 1)
	th.SetCurrentForTest(func() (uint64, bool) { return 10, true })
	ctx = WithThrottle(ctx, th)
	ctx = activateThrottle(ctx)

	start := time.Now()
	noteProgress(ctx, 1)
	if time.Since(start) < 200*time.Millisecond {
		t.Fatalf("expected a bounded pause, waited %s", time.Since(start))
	}
	if err := budgetErr(ctx); err != nil {
		t.Fatalf("budget expired during a voluntary pause: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled during a voluntary pause: %v", ctx.Err())
	default:
	}
}

func TestBudgetExpiresWithoutPause(t *testing.T) {
	ctx, cancel := WithBudget(context.Background(), 30*time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("deadline did not fire")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("err = %v", ctx.Err())
	}
}

func TestBudgetParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, stop := WithBudget(parent, time.Hour)
	defer stop()
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("err = %v", ctx.Err())
	}
}

func TestIncrementalDoesNotThrottle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := NewTreeCache(8 << 20)
	th := cgroup.NewThrottle(1, 1)
	var checks int
	th.SetCurrentForTest(func() (uint64, bool) {
		checks++
		return 0, true
	})
	ctx := WithThrottle(context.Background(), th)
	if _, err := InspectWith(ctx, root, cache, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if checks == 0 {
		t.Fatal("full scan did not consult the throttler")
	}
	checks = 0
	if _, err := InspectWith(ctx, root, cache, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if checks != 0 {
		t.Fatalf("incremental scan consulted the throttler %d times", checks)
	}
}
