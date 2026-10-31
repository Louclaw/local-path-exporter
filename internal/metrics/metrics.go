// Package metrics exposes kubelet-compatible volume gauges from the last scan.
// Series are rebuilt from the cache on every scrape, so removed PVCs disappear.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Sample is one PVC. Label values identify the claim; they never carry a measurement.
type Sample struct {
	Namespace      string
	PVC            string
	PV             string
	Node           string
	UsedBytes      float64
	CapacityBytes  float64
	AvailableBytes float64
	Inodes         float64
	InodesFree     float64
	InodesUsed     float64
	// EmitCapacity is false when neither a PVC request nor the filesystem size is known.
	EmitCapacity bool
}

// Result is the outcome of measuring one volume.
type Result struct {
	Namespace string
	PVC       string
	Sample    Sample
	Err       error
}

// NewSample builds gauges.
// capacity is the PVC request, or the filesystem size when the claim has no request.
// emitCapacity is false when that size is unknown, so a zero capacity is not published.
// available is min(capacity-used, fsAvail), floored at zero. Without a capacity it is fsAvail.
func NewSample(ns, pvc, pv, node string, capacity, used, inodesUsed, fsAvail, fsFiles, fsFree int64, emitCapacity bool, available func(int64, int64, int64) int64) Sample {
	if used < 0 {
		used = 0
	}
	if inodesUsed < 0 {
		inodesUsed = 0
	}
	if fsFiles < 0 {
		fsFiles = 0
	}
	if fsFree < 0 {
		fsFree = 0
	}
	if fsAvail < 0 {
		fsAvail = 0
	}
	avail := fsAvail
	if emitCapacity {
		avail = available(capacity, used, fsAvail)
	} else {
		capacity = 0
	}
	return Sample{
		Namespace:      ns,
		PVC:            pvc,
		PV:             pv,
		Node:           node,
		UsedBytes:      float64(used),
		CapacityBytes:  float64(capacity),
		AvailableBytes: float64(avail),
		Inodes:         float64(fsFiles),
		InodesFree:     float64(fsFree),
		InodesUsed:     float64(inodesUsed),
		EmitCapacity:   emitCapacity,
	}
}

// Cache stores the latest samples and scan counters.
type Cache struct {
	mu          sync.Mutex
	samples     map[string]Sample
	duration    float64
	errors      float64
	lastSuccess float64
	entries     float64
	cacheBytes  float64
	scanned     bool
	now         func() time.Time
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{samples: map[string]Sample{}, now: time.Now}
}

// SetClock overrides the clock used for the last-success timestamp.
func (c *Cache) SetClock(now func() time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// FailCycle records a cycle that could not list volumes. Previous samples stay.
func (c *Cache) FailCycle(durationSec float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.duration = durationSec
	c.errors++
	c.scanned = true
}

// Commit replaces the sample set. A volume error keeps the previous sample
// and increments the error counter. A volume absent from results is dropped.
// last success advances when the cycle had nothing to do or measured something.
func (c *Cache) Commit(results []Result, durationSec float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	type slot struct {
		sample Sample
		ok     bool
		failed bool
	}
	order := make([]string, 0, len(results))
	slots := map[string]*slot{}
	var unnamed int
	for _, r := range results {
		if r.Namespace == "" || r.PVC == "" {
			unnamed++
			continue
		}
		k := r.Namespace + "\x00" + r.PVC
		s, seen := slots[k]
		if !seen {
			s = &slot{}
			slots[k] = s
			order = append(order, k)
		}
		if r.Err != nil {
			s.failed = true
			if s.sample.Namespace == "" {
				s.sample.Namespace = r.Namespace
				s.sample.PVC = r.PVC
			}
			continue
		}
		s.sample = r.Sample
		s.ok = true
	}

	next := make(map[string]Sample, len(slots))
	successes := 0
	failures := unnamed
	for _, k := range order {
		s := slots[k]
		if s.ok {
			next[k] = s.sample
			successes++
			continue
		}
		failures++
		if old, ok := c.samples[k]; ok {
			next[k] = old
		}
	}
	c.samples = next
	c.duration = durationSec
	c.errors += float64(failures)
	c.scanned = true
	if successes > 0 || len(results) == 0 {
		c.lastSuccess = float64(c.now().Unix())
	}
}

// SetEntries records how many directory entries the last cycle actually walked.
func (c *Cache) SetEntries(n float64) {
	c.mu.Lock()
	c.entries = n
	c.mu.Unlock()
}

// SetCacheBytes records the in-memory directory cache size.
func (c *Cache) SetCacheBytes(n float64) {
	c.mu.Lock()
	c.cacheBytes = n
	c.mu.Unlock()
}

// Previous returns the sample kept for a claim, if any.
func (c *Cache) Previous(ns, pvc string) (Sample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.samples[ns+"\x00"+pvc]
	return s, ok
}

// Snapshot is a copy of the cache for a scrape.
type Snapshot struct {
	Samples     []Sample
	Duration    float64
	Errors      float64
	LastSuccess float64
	Entries     float64
	CacheBytes  float64
	Scanned     bool
}

// Snapshot copies the current cache.
func (c *Cache) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := Snapshot{
		Samples:     make([]Sample, 0, len(c.samples)),
		Duration:    c.duration,
		Errors:      c.errors,
		LastSuccess: c.lastSuccess,
		Entries:     c.entries,
		CacheBytes:  c.cacheBytes,
		Scanned:     c.scanned,
	}
	for _, s := range c.samples {
		out.Samples = append(out.Samples, s)
	}
	return out
}

// Collector emits volume gauges from the cache on each scrape.
type Collector struct {
	cache      *Cache
	used       *prometheus.Desc
	capacity   *prometheus.Desc
	available  *prometheus.Desc
	inodes     *prometheus.Desc
	inodesFree *prometheus.Desc
	inodesUsed *prometheus.Desc
	duration   *prometheus.Desc
	errors     *prometheus.Desc
	last       *prometheus.Desc
	entries    *prometheus.Desc
	cacheBytes *prometheus.Desc
}

// NewCollector registers the kubelet volume names and the scan counters.
func NewCollector(cache *Cache) *Collector {
	labels := []string{"namespace", "persistentvolumeclaim", "persistentvolume", "node"}
	return &Collector{
		cache: cache,
		used: prometheus.NewDesc(
			"kubelet_volume_stats_used_bytes",
			"Number of used bytes in the volume",
			labels, nil,
		),
		capacity: prometheus.NewDesc(
			"kubelet_volume_stats_capacity_bytes",
			"Capacity in bytes of the volume",
			labels, nil,
		),
		available: prometheus.NewDesc(
			"kubelet_volume_stats_available_bytes",
			"Number of available bytes in the volume",
			labels, nil,
		),
		inodes: prometheus.NewDesc(
			"kubelet_volume_stats_inodes",
			"Maximum number of inodes on the filesystem that hosts the volume. Shared by every volume on that filesystem.",
			labels, nil,
		),
		inodesFree: prometheus.NewDesc(
			"kubelet_volume_stats_inodes_free",
			"Free inodes on the filesystem that hosts the volume. Not a per-PVC quota.",
			labels, nil,
		),
		inodesUsed: prometheus.NewDesc(
			"kubelet_volume_stats_inodes_used",
			"Number of used inodes in the volume",
			labels, nil,
		),
		duration: prometheus.NewDesc(
			"local_path_exporter_scan_duration_seconds",
			"Duration of the last scan cycle in seconds.",
			nil, nil,
		),
		errors: prometheus.NewDesc(
			"local_path_exporter_scan_errors_total",
			"Scan errors since process start. A failed volume keeps its previous sample.",
			nil, nil,
		),
		last: prometheus.NewDesc(
			"local_path_exporter_scan_last_success_timestamp_seconds",
			"Unix time of the last scan cycle that measured a volume or found none.",
			nil, nil,
		),
		entries: prometheus.NewDesc(
			"local_path_exporter_scan_entries",
			"Directory entries walked during the last scan cycle. Cached directories and volumes skipped for their interval add none.",
			nil, nil,
		),
		cacheBytes: prometheus.NewDesc(
			"local_path_exporter_scan_cache_bytes",
			"Estimated bytes of the in-memory directory cache.",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.used
	ch <- c.capacity
	ch <- c.available
	ch <- c.inodes
	ch <- c.inodesFree
	ch <- c.inodesUsed
	ch <- c.duration
	ch <- c.errors
	ch <- c.last
	ch <- c.entries
	ch <- c.cacheBytes
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	snap := c.cache.Snapshot()
	for _, s := range snap.Samples {
		c.gauge(ch, c.used, s.UsedBytes, s)
		if s.EmitCapacity {
			c.gauge(ch, c.capacity, s.CapacityBytes, s)
		}
		c.gauge(ch, c.available, s.AvailableBytes, s)
		c.gauge(ch, c.inodes, s.Inodes, s)
		c.gauge(ch, c.inodesFree, s.InodesFree, s)
		c.gauge(ch, c.inodesUsed, s.InodesUsed, s)
	}
	ch <- prometheus.MustNewConstMetric(c.errors, prometheus.CounterValue, snap.Errors)
	if snap.Scanned {
		ch <- prometheus.MustNewConstMetric(c.duration, prometheus.GaugeValue, snap.Duration)
		ch <- prometheus.MustNewConstMetric(c.entries, prometheus.GaugeValue, snap.Entries)
		ch <- prometheus.MustNewConstMetric(c.cacheBytes, prometheus.GaugeValue, snap.CacheBytes)
	}
	if snap.LastSuccess > 0 {
		ch <- prometheus.MustNewConstMetric(c.last, prometheus.GaugeValue, snap.LastSuccess)
	}
}

func (c *Collector) gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, v float64, s Sample) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, s.Namespace, s.PVC, s.PV, s.Node)
}
