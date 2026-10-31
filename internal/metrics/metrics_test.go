package metrics

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Louclaw/local-path-exporter/internal/stats"
)

func TestCommitKeepsOldDropsGone(t *testing.T) {
	c := NewCache()
	c.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0) })

	ok := func(ns, name string, used float64) Result {
		return Result{
			Namespace: ns,
			PVC:       name,
			Sample: Sample{
				Namespace: ns, PVC: name, UsedBytes: used, CapacityBytes: 100,
			},
		}
	}
	c.Commit([]Result{ok("ns", "a", 10), ok("ns", "b", 20)}, 1.5)
	snap := c.Snapshot()
	if len(snap.Samples) != 2 || snap.Errors != 0 || snap.LastSuccess != 1_700_000_000 {
		t.Fatalf("snap %+v", snap)
	}

	c.Commit([]Result{
		ok("ns", "a", 11),
		{Namespace: "ns", PVC: "b", Err: errors.New("timeout")},
	}, 2)
	snap = c.Snapshot()
	got := map[string]float64{}
	for _, s := range snap.Samples {
		got[s.PVC] = s.UsedBytes
	}
	if got["a"] != 11 || got["b"] != 20 {
		t.Fatalf("values = %v", got)
	}
	if snap.Errors != 1 || snap.LastSuccess != 1_700_000_000 {
		t.Fatalf("errors=%v last=%v", snap.Errors, snap.LastSuccess)
	}

	c.Commit([]Result{ok("ns", "a", 12)}, 1)
	snap = c.Snapshot()
	if len(snap.Samples) != 1 || snap.Samples[0].PVC != "a" || snap.Samples[0].UsedBytes != 12 {
		t.Fatalf("stale series kept: %+v", snap.Samples)
	}
}

func TestCommitAllFailedDoesNotAdvanceSuccess(t *testing.T) {
	c := NewCache()
	c.SetClock(func() time.Time { return time.Unix(50, 0) })
	c.Commit([]Result{{
		Namespace: "ns", PVC: "a",
		Sample: Sample{Namespace: "ns", PVC: "a", UsedBytes: 5},
	}}, 1)
	c.SetClock(func() time.Time { return time.Unix(99, 0) })
	c.Commit([]Result{{Namespace: "ns", PVC: "a", Err: errors.New("io")}}, 3)
	snap := c.Snapshot()
	if snap.LastSuccess != 50 || snap.Errors != 1 || snap.Samples[0].UsedBytes != 5 {
		t.Fatalf("%+v", snap)
	}
}

func TestFailCycleKeepsSamples(t *testing.T) {
	c := NewCache()
	c.SetClock(func() time.Time { return time.Unix(10, 0) })
	c.Commit(nil, 0.2)
	if c.Snapshot().LastSuccess != 10 {
		t.Fatal("empty cycle should count as success")
	}
	c.FailCycle(4)
	snap := c.Snapshot()
	if snap.LastSuccess != 10 || snap.Errors != 1 || snap.Duration != 4 {
		t.Fatalf("%+v", snap)
	}
}

func TestCollectorSeries(t *testing.T) {
	c := NewCache()
	c.SetClock(func() time.Time { return time.Unix(100, 0) })
	c.Commit([]Result{{
		Namespace: "ns",
		PVC:       "data",
		Sample:    NewSample("ns", "data", "pvc-1", "node-a", 100, 40, 7, 1000, 50, 30, true, stats.AvailableBytes),
	}}, 1.25)

	col := NewCollector(c)
	expected := `
# HELP kubelet_volume_stats_available_bytes Number of available bytes in the volume
# TYPE kubelet_volume_stats_available_bytes gauge
kubelet_volume_stats_available_bytes{namespace="ns",node="node-a",persistentvolume="pvc-1",persistentvolumeclaim="data"} 60
# HELP kubelet_volume_stats_capacity_bytes Capacity in bytes of the volume
# TYPE kubelet_volume_stats_capacity_bytes gauge
kubelet_volume_stats_capacity_bytes{namespace="ns",node="node-a",persistentvolume="pvc-1",persistentvolumeclaim="data"} 100
# HELP kubelet_volume_stats_inodes Maximum number of inodes on the filesystem that hosts the volume. Shared by every volume on that filesystem.
# TYPE kubelet_volume_stats_inodes gauge
kubelet_volume_stats_inodes{namespace="ns",node="node-a",persistentvolume="pvc-1",persistentvolumeclaim="data"} 50
# HELP kubelet_volume_stats_inodes_free Free inodes on the filesystem that hosts the volume. Not a per-PVC quota.
# TYPE kubelet_volume_stats_inodes_free gauge
kubelet_volume_stats_inodes_free{namespace="ns",node="node-a",persistentvolume="pvc-1",persistentvolumeclaim="data"} 30
# HELP kubelet_volume_stats_inodes_used Number of used inodes in the volume
# TYPE kubelet_volume_stats_inodes_used gauge
kubelet_volume_stats_inodes_used{namespace="ns",node="node-a",persistentvolume="pvc-1",persistentvolumeclaim="data"} 7
# HELP kubelet_volume_stats_used_bytes Number of used bytes in the volume
# TYPE kubelet_volume_stats_used_bytes gauge
kubelet_volume_stats_used_bytes{namespace="ns",node="node-a",persistentvolume="pvc-1",persistentvolumeclaim="data"} 40
# HELP local_path_exporter_scan_cache_bytes Estimated bytes of the in-memory directory cache.
# TYPE local_path_exporter_scan_cache_bytes gauge
local_path_exporter_scan_cache_bytes 0
# HELP local_path_exporter_scan_duration_seconds Duration of the last scan cycle in seconds.
# TYPE local_path_exporter_scan_duration_seconds gauge
local_path_exporter_scan_duration_seconds 1.25
# HELP local_path_exporter_scan_entries Directory entries walked during the last scan cycle. Cached directories and volumes skipped for their interval add none.
# TYPE local_path_exporter_scan_entries gauge
local_path_exporter_scan_entries 0
# HELP local_path_exporter_scan_errors_total Scan errors since process start. A failed volume keeps its previous sample.
# TYPE local_path_exporter_scan_errors_total counter
local_path_exporter_scan_errors_total 0
# HELP local_path_exporter_scan_last_success_timestamp_seconds Unix time of the last scan cycle that measured a volume or found none.
# TYPE local_path_exporter_scan_last_success_timestamp_seconds gauge
local_path_exporter_scan_last_success_timestamp_seconds 100
`
	if err := testutil.CollectAndCompare(col, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}

	c.Commit([]Result{}, 2)
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(NewCollector(c)); err != nil {
		t.Fatal(err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range fams {
		if strings.HasPrefix(fam.GetName(), "kubelet_volume_stats_") && len(fam.Metric) != 0 {
			t.Fatalf("stale series still exported: %s", fam.GetName())
		}
	}
}

func TestCollectorOmitsUnknownCapacity(t *testing.T) {
	sample := NewSample("ns", "data", "pvc-1", "node-a", 0, 40, 1, 500, 10, 4, false, stats.AvailableBytes)
	if sample.EmitCapacity || sample.CapacityBytes != 0 || sample.AvailableBytes != 500 {
		t.Fatalf("unknown capacity must not look full: %+v", sample)
	}
	c := NewCache()
	c.Commit([]Result{{Namespace: "ns", PVC: "data", Sample: sample}}, 1)
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(NewCollector(c)); err != nil {
		t.Fatal(err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range fams {
		if fam.GetName() == "kubelet_volume_stats_capacity_bytes" {
			t.Fatal("capacity series emitted without a known size")
		}
	}
}
