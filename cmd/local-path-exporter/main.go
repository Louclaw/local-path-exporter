package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	toolcache "k8s.io/client-go/tools/cache"

	"github.com/Louclaw/local-path-exporter/internal/cgroup"
	"github.com/Louclaw/local-path-exporter/internal/config"
	"github.com/Louclaw/local-path-exporter/internal/exporter"
	"github.com/Louclaw/local-path-exporter/internal/mapping"
	"github.com/Louclaw/local-path-exporter/internal/metrics"
	"github.com/Louclaw/local-path-exporter/internal/priority"
	"github.com/Louclaw/local-path-exporter/internal/scan"
	"github.com/Louclaw/local-path-exporter/internal/serve"

	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	priority.Lower()

	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store := metrics.NewCache()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.NewCollector(store))
	throttle := cgroup.NewThrottle(uint64(cfg.ThrottleHigh), cfg.ThrottleBatch)
	reg.MustRegister(cgroup.NewCollector(throttle))

	var ready atomic.Bool
	srv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           serve.Handler(reg, &ready),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shctx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http", "err", err)
			stop()
		}
	}()
	slog.Info("listening", "addr", cfg.MetricsAddr, "node", cfg.NodeName, "interval", cfg.ScanInterval.String())

	client, err := kubeClient()
	if err != nil {
		slog.Error("kube client", "err", err)
		os.Exit(1)
	}
	factory := informers.NewSharedInformerFactory(client, 10*time.Minute)
	pvInf := factory.Core().V1().PersistentVolumes()
	pvcInf := factory.Core().V1().PersistentVolumeClaims()
	if err := pvInf.Informer().AddIndexers(toolcache.Indexers{
		pvNodeIndex: indexPVByNode,
	}); err != nil {
		slog.Error("pv index", "err", err)
		os.Exit(1)
	}
	factory.Start(ctx.Done())
	for _, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			slog.Error("informer cache sync failed")
			os.Exit(1)
		}
	}

	trees := scan.NewTreeCache(cfg.CacheMaxBytes)
	if err := trees.Load(cfg.CachePath); err != nil {
		// A bad file must not be retried on the next restart: that is the crashloop.
		slog.Warn("load cache", "path", cfg.CachePath, "err", err)
		if cfg.CachePath != "" {
			if rmErr := os.Remove(cfg.CachePath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				slog.Warn("remove cache", "path", cfg.CachePath, "err", rmErr)
			}
		}
	} else if cfg.CachePath != "" {
		slog.Info("cache loaded", "path", cfg.CachePath, "bytes", trees.Bytes())
	}
	runtime.GC()
	debug.FreeOSMemory()
	loaded := cgroup.Read()
	store.SetLoadCurrent(float64(loaded.Current))
	slog.Info("cache load memory", "cgroup_current", loaded.Current, "heap", loaded.Heap, "cache_bytes", trees.Bytes())
	app := &application{
		cfg:      cfg,
		pvIndex:  pvInf.Informer().GetIndexer(),
		pvcs:     pvcInf.Lister(),
		cache:    store,
		pace:     exporter.NewPacer(cfg.ScanInterval, cfg.EntriesPerStep, cfg.FullScan),
		trees:    trees,
		throttle: throttle,
	}
	app.loop(ctx, &ready)
}

const pvNodeIndex = "node"

type application struct {
	cfg      config.Config
	pvIndex  toolcache.Indexer
	pvcs     corev1listers.PersistentVolumeClaimLister
	cache    *metrics.Cache
	pace     *exporter.Pacer
	trees    *scan.TreeCache
	throttle *cgroup.Throttle
	gate     scan.Gate
}

func indexPVByNode(obj interface{}) ([]string, error) {
	switch t := obj.(type) {
	case *corev1.PersistentVolume:
		return mapping.IndexedNodes(t), nil
	case toolcache.DeletedFinalStateUnknown:
		if pv, ok := t.Obj.(*corev1.PersistentVolume); ok {
			return mapping.IndexedNodes(pv), nil
		}
	}
	return nil, nil
}

func (a *application) loop(ctx context.Context, ready *atomic.Bool) {
	a.cycle(ctx)
	ready.Store(true)
	ticker := time.NewTicker(a.cfg.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cycle(ctx)
		}
	}
}

// cycle lists PVCs on this node and scans them. It returns false when the
// storage directory or the API cache could not be read. Volume errors keep
// the previous samples inside the cache.
func (a *application) cycle(ctx context.Context) bool {
	start := time.Now()
	names, err := scan.ChildDirs(a.cfg.StoragePath)
	if err != nil {
		slog.Warn("list storage", "path", a.cfg.StoragePath, "err", err)
		a.cache.FailCycle(time.Since(start).Seconds())
		return false
	}
	pvs, err := a.volumesOnNode()
	if err != nil {
		slog.Warn("list persistentvolumes", "err", err)
		a.cache.FailCycle(time.Since(start).Seconds())
		return false
	}
	pvcs, err := a.pvcs.List(labels.Everything())
	if err != nil {
		slog.Warn("list persistentvolumeclaims", "err", err)
		a.cache.FailCycle(time.Since(start).Seconds())
		return false
	}
	vols := mapping.Resolve(pvs, pvcs, a.cfg.NodeName, a.cfg.StoragePath, names)
	a.arm(vols, start)
	before := a.cache.Snapshot().Errors
	exporter.RunOnce(ctx, vols, a.inspect, a.cache, a.cfg.Parallelism, a.cfg.ScanTimeout, start, a.pace)
	if err := a.trees.Save(a.cfg.CachePath); err != nil {
		slog.Warn("save cache", "path", a.cfg.CachePath, "err", err)
	}
	a.cache.SetCacheBytes(float64(a.trees.Bytes()))
	snap := a.cache.Snapshot()
	slog.Info("scan finished", "volumes", len(vols), "errors", snap.Errors-before, "seconds", time.Since(start).Seconds())
	return true
}

func (a *application) volumesOnNode() ([]*corev1.PersistentVolume, error) {
	objs, err := a.pvIndex.ByIndex(pvNodeIndex, a.cfg.NodeName)
	if err != nil {
		return nil, err
	}
	out := make([]*corev1.PersistentVolume, 0, len(objs))
	for _, obj := range objs {
		pv, ok := obj.(*corev1.PersistentVolume)
		if !ok {
			continue
		}
		out = append(out, pv)
	}
	return out, nil
}

// arm picks the single volume allowed to run a full walk this cycle.
// Incremental walks are not limited. A second full walk waits for the next cycle
// so two large trees are not charged back to back.
func (a *application) arm(vols []mapping.Volume, now time.Time) {
	picked := ""
	for _, vol := range vols {
		if a.pace != nil && a.pace.Skipping(vol.Namespace, vol.Name, now) {
			if _, ok := a.cache.Previous(vol.Namespace, vol.Name); ok {
				continue
			}
		}
		if a.trees != nil && !a.trees.Due(vol.Path, a.cfg.FullScan, now) {
			continue
		}
		picked = vol.Path
		break
	}
	a.gate.Arm(picked)
}

func (a *application) inspect(ctx context.Context, path string) (scan.Report, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	priority.Lower()
	start := time.Now()
	full := a.trees == nil || a.trees.Due(path, a.cfg.FullScan, start)
	if err := a.gate.Allow(path, full); err != nil {
		slog.Info("defer full scan", "path", path)
		return scan.Report{}, err
	}
	before := cgroup.Read()
	var pausesBefore int64
	var pausedBefore time.Duration
	if a.throttle != nil {
		pausesBefore = a.throttle.Pauses()
		pausedBefore = a.throttle.Paused()
	}
	ctx = scan.WithThrottle(ctx, a.throttle)
	rep, err := scan.InspectWith(ctx, path, a.trees, start, a.cfg.FullScan)
	after := cgroup.Read()
	elapsed := time.Since(start)
	attrs := []any{
		"path", path,
		"full", full,
		"seconds", elapsed.Seconds(),
		"entries", rep.Usage.Entries,
		"throttle_pauses", a.throttlePauses(pausesBefore),
		"throttle_pause_seconds", a.throttlePaused(pausedBefore).Seconds(),
		"tracked", rep.Usage.Tracked,
		"inodes", rep.Usage.Inodes,
		"bytes", rep.Usage.Bytes,
		"cache_bytes", a.trees.Bytes(),
		"cgroup_current", after.Current,
		"working_set", after.WorkingSet,
		"delta_current", int64(after.Current) - int64(before.Current),
		"delta_anon", int64(after.Anon) - int64(before.Anon),
		"delta_file", int64(after.File) - int64(before.File),
		"delta_slab_reclaimable", int64(after.SlabReclaimable) - int64(before.SlabReclaimable),
		"delta_slab_unreclaimable", int64(after.SlabUnreclaimable) - int64(before.SlabUnreclaimable),
		"delta_kernel", int64(after.Kernel) - int64(before.Kernel),
		"go_heap", after.Heap,
		"delta_heap", int64(after.Heap) - int64(before.Heap),
	}
	if err != nil {
		attrs = append(attrs, "err", err)
		slog.Warn("scan volume", attrs...)
	} else {
		slog.Info("scan volume", attrs...)
	}
	cgroup.Release()
	return rep, err
}

func (a *application) throttlePauses(before int64) int64 {
	if a.throttle == nil {
		return 0
	}
	return a.throttle.Pauses() - before
}

func (a *application) throttlePaused(before time.Duration) time.Duration {
	if a.throttle == nil {
		return 0
	}
	return a.throttle.Paused() - before
}

func kubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "local-path-exporter"
	return kubernetes.NewForConfig(cfg)
}
