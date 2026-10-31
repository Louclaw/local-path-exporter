package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
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
	reg.MustRegister(cgroup.NewCollector())

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

	app := &application{
		cfg:     cfg,
		pvIndex: pvInf.Informer().GetIndexer(),
		pvcs:    pvcInf.Lister(),
		cache:   store,
		pace:    exporter.NewPacer(cfg.ScanInterval, cfg.EntriesPerStep, cfg.FullScan),
		trees:   scan.NewTreeCache(cfg.CacheMaxBytes),
	}
	app.loop(ctx, &ready)
}

const pvNodeIndex = "node"

type application struct {
	cfg     config.Config
	pvIndex toolcache.Indexer
	pvcs    corev1listers.PersistentVolumeClaimLister
	cache   *metrics.Cache
	pace    *exporter.Pacer
	trees   *scan.TreeCache
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
	before := a.cache.Snapshot().Errors
	exporter.RunOnce(ctx, vols, a.inspect, a.cache, a.cfg.Parallelism, a.cfg.ScanTimeout, start, a.pace)
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

func (a *application) inspect(ctx context.Context, path string) (scan.Report, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	priority.Lower()
	before := cgroup.Read()
	start := time.Now()
	rep, err := scan.InspectWith(ctx, path, a.trees, start, a.cfg.FullScan)
	after := cgroup.Read()
	elapsed := time.Since(start)
	attrs := []any{
		"path", path,
		"seconds", elapsed.Seconds(),
		"entries", rep.Usage.Entries,
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

func kubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "local-path-exporter"
	return kubernetes.NewForConfig(cfg)
}
