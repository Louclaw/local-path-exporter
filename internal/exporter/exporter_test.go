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

	vols := []mapping.Volume{{Path: "/pvc", Namespace: "ns", Name: "data", PV: "pvc-1", Node: "node-a", Capacity: 100, CapacityFromPVC: true}}
	RunOnce(context.Background(), vols, func(ctx context.Context, path string) (scan.Report, error) {
		<-ctx.Done()
		return scan.Report{}, ctx.Err()
	}, cache, 1, 20*time.Millisecond, time.Now(), nil)

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
		{Path: "/a", Namespace: "ns", Name: "a", PV: "pvc-a", Node: "node-a", Capacity: 100, CapacityFromPVC: true},
		{Path: "/b", Namespace: "ns", Name: "b", PV: "pvc-b", Node: "node-a", Capacity: 100, CapacityFromPVC: true},
		{Path: "/c", Namespace: "ns", Name: "c", PV: "pvc-c", Node: "node-a", Capacity: 30, CapacityFromPVC: true},
		{Path: "/bad", Namespace: "ns", Name: "bad", PV: "pvc-bad", Node: "node-a", Capacity: 100, CapacityFromPVC: true},
	}
	cache := metrics.NewCache()
	done := make(chan struct{})
	go func() {
		RunOnce(context.Background(), vols, inspect, cache, 2, time.Minute, time.Now(), nil)
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
	if byName["a"].UsedBytes != 40 || byName["a"].AvailableBytes != 60 || byName["a"].InodesUsed != 3 || byName["a"].PV != "pvc-a" || byName["a"].Node != "node-a" {
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

func TestRunOnceFilesystemCapacity(t *testing.T) {
	cache := metrics.NewCache()
	vols := []mapping.Volume{{
		Path: "/a", Namespace: "ns", Name: "data", PV: "pvc-fallback", Node: "node-a",
	}}
	RunOnce(context.Background(), vols, func(ctx context.Context, path string) (scan.Report, error) {
		return scan.Report{
			Usage:      scan.Usage{Bytes: 100, Inodes: 2},
			TotalBytes: 8000,
			AvailBytes: 7000,
			Files:      10,
			FilesFree:  8,
		}, nil
	}, cache, 1, time.Second, time.Now(), nil)
	snap := cache.Snapshot()
	if len(snap.Samples) != 1 {
		t.Fatal(snap.Samples)
	}
	s := snap.Samples[0]
	if !s.EmitCapacity || s.CapacityBytes != 8000 || s.AvailableBytes != 7000 || s.PV != "pvc-fallback" {
		t.Fatalf("filesystem fallback: %+v", s)
	}
}

func TestRunOnceParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache := metrics.NewCache()
	RunOnce(ctx, []mapping.Volume{{Path: "/a", Namespace: "ns", Name: "a"}}, func(ctx context.Context, path string) (scan.Report, error) {
		t.Fatal("inspect should not run")
		return scan.Report{}, nil
	}, cache, 1, time.Second, time.Now(), nil)
	if cache.Snapshot().Errors != 1 {
		t.Fatal("expected error")
	}
}

func TestPacerSkipsLargeVolume(t *testing.T) {
	cache := metrics.NewCache()
	var calls atomic.Int32
	inspect := func(ctx context.Context, path string) (scan.Report, error) {
		calls.Add(1)
		return scan.Report{Usage: scan.Usage{Bytes: 10, Inodes: 4, Entries: 100}}, nil
	}
	vol := mapping.Volume{Path: "/big", Namespace: "ns", Name: "big", PV: "pvc-big", Node: "n", Capacity: 100, CapacityFromPVC: true}
	pace := NewPacer(time.Hour, 50, 0)
	RunOnce(context.Background(), []mapping.Volume{vol}, inspect, cache, 1, time.Second, time.Now(), pace)
	if calls.Load() != 1 || cache.Snapshot().Entries != 100 {
		t.Fatalf("calls=%d entries=%v", calls.Load(), cache.Snapshot().Entries)
	}
	RunOnce(context.Background(), []mapping.Volume{vol}, inspect, cache, 1, time.Second, time.Now(), pace)
	if calls.Load() != 1 {
		t.Fatalf("large volume scanned again: %d", calls.Load())
	}
	snap := cache.Snapshot()
	if len(snap.Samples) != 1 || snap.Samples[0].UsedBytes != 10 || snap.Entries != 0 {
		t.Fatalf("skipped cycle lost the sample: %+v entries=%v", snap.Samples, snap.Entries)
	}
}
