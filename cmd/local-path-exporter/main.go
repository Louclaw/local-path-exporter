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

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"

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
	factory.Start(ctx.Done())
	for _, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			slog.Error("informer cache sync failed")
			os.Exit(1)
		}
	}

	app := &application{
		cfg:   cfg,
		pvs:   pvInf.Lister(),
		pvcs:  pvcInf.Lister(),
		cache: store,
	}
	app.loop(ctx, &ready)
}

type application struct {
	cfg   config.Config
	pvs   corev1listers.PersistentVolumeLister
	pvcs  corev1listers.PersistentVolumeClaimLister
	cache *metrics.Cache
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
	pvs, err := a.pvs.List(labels.Everything())
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
	exporter.RunOnce(ctx, vols, inspect, a.cache, a.cfg.Parallelism, a.cfg.ScanTimeout, start)
	snap := a.cache.Snapshot()
	slog.Info("scan finished", "volumes", len(vols), "errors", snap.Errors-before, "seconds", time.Since(start).Seconds())
	return true
}

func inspect(ctx context.Context, path string) (scan.Report, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	priority.Lower()
	rep, err := scan.Inspect(ctx, path)
	if err != nil {
		slog.Warn("scan volume", "path", path, "err", err)
	}
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
