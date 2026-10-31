package cgroup

import "github.com/prometheus/client_golang/prometheus"

// Collector publishes cgroup memory and the Go heap on each scrape.
type Collector struct {
	anon       *prometheus.Desc
	file       *prometheus.Desc
	slabR      *prometheus.Desc
	slabU      *prometheus.Desc
	kernel     *prometheus.Desc
	current    *prometheus.Desc
	workingSet *prometheus.Desc
	heap       *prometheus.Desc
}

// NewCollector returns gauges with no labels. They describe the process, not a PVC.
func NewCollector() *Collector {
	return &Collector{
		anon: prometheus.NewDesc(
			"local_path_exporter_cgroup_anon_bytes",
			"Anonymous memory charged to the process cgroup.",
			nil, nil,
		),
		file: prometheus.NewDesc(
			"local_path_exporter_cgroup_file_bytes",
			"File page cache charged to the process cgroup.",
			nil, nil,
		),
		slabR: prometheus.NewDesc(
			"local_path_exporter_cgroup_slab_reclaimable_bytes",
			"Reclaimable kernel slab charged to the process cgroup (dentries and inodes).",
			nil, nil,
		),
		slabU: prometheus.NewDesc(
			"local_path_exporter_cgroup_slab_unreclaimable_bytes",
			"Unreclaimable kernel slab charged to the process cgroup.",
			nil, nil,
		),
		kernel: prometheus.NewDesc(
			"local_path_exporter_cgroup_kernel_bytes",
			"Kernel memory charged to the process cgroup.",
			nil, nil,
		),
		current: prometheus.NewDesc(
			"local_path_exporter_cgroup_current_bytes",
			"memory.current of the process cgroup.",
			nil, nil,
		),
		workingSet: prometheus.NewDesc(
			"local_path_exporter_cgroup_working_set_bytes",
			"memory.current minus inactive_file. Inactive file cache is reclaimable and is what a near-limit alert should ignore.",
			nil, nil,
		),
		heap: prometheus.NewDesc(
			"local_path_exporter_go_heap_bytes",
			"Go heap bytes allocated (HeapAlloc). Independent of the cgroup kernel charges.",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.anon
	ch <- c.file
	ch <- c.slabR
	ch <- c.slabU
	ch <- c.kernel
	ch <- c.current
	ch <- c.workingSet
	ch <- c.heap
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	s := Read()
	ch <- prometheus.MustNewConstMetric(c.heap, prometheus.GaugeValue, float64(s.Heap))
	if !s.OK {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.anon, prometheus.GaugeValue, float64(s.Anon))
	ch <- prometheus.MustNewConstMetric(c.file, prometheus.GaugeValue, float64(s.File))
	ch <- prometheus.MustNewConstMetric(c.slabR, prometheus.GaugeValue, float64(s.SlabReclaimable))
	ch <- prometheus.MustNewConstMetric(c.slabU, prometheus.GaugeValue, float64(s.SlabUnreclaimable))
	ch <- prometheus.MustNewConstMetric(c.kernel, prometheus.GaugeValue, float64(s.Kernel))
	ch <- prometheus.MustNewConstMetric(c.current, prometheus.GaugeValue, float64(s.Current))
	if s.HasInactiveFile {
		ch <- prometheus.MustNewConstMetric(c.workingSet, prometheus.GaugeValue, float64(s.WorkingSet))
	}
}
