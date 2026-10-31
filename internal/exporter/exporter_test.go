package exporter

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Louclaw/local-path-exporter/internal/mapping"
	"github.com/Louclaw/local-path-exporter/internal/metrics"
	"github.com/Louclaw/local-path-exporter/internal/scan"
)

func TestRunOnceTimeoutKeepsPrevious(t *testing.T) {
	cache := metrics.NewCache()
	cache.SetClock(func() time.Time { return time.Unix(10, 0) })
	cache.Commit([]metrics.Result{{
		Namespace: "ns",
		PVC:       "data",
		Sample:    metrics.Sample{Namespace: "ns", PVC: "data", UsedBytes: 23},
	}}, 0)

	vols := []mapping.Volume{{Path: "/pvc", Namespace: "ns", Name: "data", Capacity: 100}}
	RunOnce(context.Background(), vols, func(ctx context.Context, path string) (scan.Report, error) {
		<-ctx.Done()
		return scan.Report{}, ctx.Err()
	}, cache, 1, 20*time.Millisecond, time.Now())

	snap := cache.Snapshot()
	if len(snap.Samples) != 1 || snap.Samples[0].UsedBytes != 23 {
		t.Fatalf("old sample lost: %+v", snap.Samples)
	}
	if snap.Errors != 1 || snap.LastSuccess != 10 {
		t.Fatalf("errors=%v last=%v", snap.Errors, snap.LastSuccess)
	}
}

func TestRunOnceParallelismAndMath(t *testing.T) {
	var inflight atomic.Int32
	var maxInflight atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	inspect := func(ctx context.Context, path string) (scan.Report, error) {
		n := inflight.Add(1)
		for {
			old := maxInflight.Load()
			if n <= old || maxInflight.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			inflight.Add(-1)
			return scan.Report{}, ctx.Err()
		}
		inflight.Add(-1)
		if path == "/bad" {
			return scan.Report{}, errors.New("stat")
		}
		return scan.Report{
			Usage:      scan.Usage{Bytes: 40, Inodes: 3},
			AvailBytes: 1000,
			Files:      10,
			FilesFree:  4,
		}, nil
	}

	vols := []mapping.Volume{
		{Path: "/a", Namespace: "ns", Name: "a", Capacity: 100},
		{Path: "/b", Namespace: "ns", Name: "b", Capacity: 100},
		{Path: "/c", Namespace: "ns", Name: "c", Capacity: 30},
		{Path: "/bad", Namespace: "ns", Name: "bad", Capacity: 100},
	}
	cache := metrics.NewCache()
	done := make(chan struct{})
	go func() {
		RunOnce(context.Background(), vols, inspect, cache, 2, time.Minute, time.Now())
		close(done)
	}()
	for i := 0; i < 2; i++ {
		<-started
	}
	select {
	case <-started:
		t.Fatal("parallelism exceeded")
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	<-done
	if maxInflight.Load() > 2 {
		t.Fatalf("max inflight %d", maxInflight.Load())
	}
	snap := cache.Snapshot()
	byName := map[string]metrics.Sample{}
	for _, s := range snap.Samples {
		byName[s.PVC] = s
	}
	if byName["a"].UsedBytes != 40 || byName["a"].AvailableBytes != 60 || byName["a"].InodesUsed != 3 {
		t.Fatalf("a = %+v", byName["a"])
	}
	// used 40 > capacity 30, so available is 0 even though the filesystem has space.
	if byName["c"].AvailableBytes != 0 || byName["c"].CapacityBytes != 30 {
		t.Fatalf("c = %+v", byName["c"])
	}
	if _, ok := byName["bad"]; ok {
		t.Fatal("first failure must not invent a sample")
	}
	if snap.Errors != 1 {
		t.Fatalf("errors %v", snap.Errors)
	}
}

func TestRunOnceParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache := metrics.NewCache()
	RunOnce(ctx, []mapping.Volume{{Path: "/a", Namespace: "ns", Name: "a"}}, func(ctx context.Context, path string) (scan.Report, error) {
		t.Fatal("inspect should not run")
		return scan.Report{}, nil
	}, cache, 1, time.Second, time.Now())
	if cache.Snapshot().Errors != 1 {
		t.Fatal("expected error")
	}
}
