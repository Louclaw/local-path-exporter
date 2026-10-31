// Package exporter runs one bounded scan cycle and commits it to the cache.
package exporter

import (
	"context"
	"sync"
	"time"

	"github.com/Louclaw/local-path-exporter/internal/mapping"
	"github.com/Louclaw/local-path-exporter/internal/metrics"
	"github.com/Louclaw/local-path-exporter/internal/scan"
	"github.com/Louclaw/local-path-exporter/internal/stats"
)

// InspectFunc measures one directory. The context carries the per-volume timeout.
type InspectFunc func(ctx context.Context, path string) (scan.Report, error)

// RunOnce measures vols with at most parallelism walks at a time.
// Each walk uses its own timeout. The cache is updated once, after every walk returns.
func RunOnce(ctx context.Context, vols []mapping.Volume, inspect InspectFunc, cache *metrics.Cache, parallelism int, timeout time.Duration, started time.Time) {
	if parallelism < 1 {
		parallelism = 1
	}
	if timeout <= 0 {
		timeout = time.Minute
	}
	results := make([]metrics.Result, len(vols))
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallelism)
	for i, vol := range vols {
		wg.Add(1)
		go func(i int, vol mapping.Volume) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = metrics.Result{Namespace: vol.Namespace, PVC: vol.Name, Err: ctx.Err()}
				return
			}
			if err := ctx.Err(); err != nil {
				results[i] = metrics.Result{Namespace: vol.Namespace, PVC: vol.Name, Err: err}
				return
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			rep, err := inspect(cctx, vol.Path)
			if err != nil {
				results[i] = metrics.Result{Namespace: vol.Namespace, PVC: vol.Name, Err: err}
				return
			}
			results[i] = metrics.Result{
				Namespace: vol.Namespace,
				PVC:       vol.Name,
				Sample: metrics.NewSample(
					vol.Namespace, vol.Name, vol.Capacity,
					rep.Usage.Bytes, rep.Usage.Inodes,
					rep.AvailBytes, rep.Files, rep.FilesFree,
					stats.AvailableBytes,
				),
			}
		}(i, vol)
	}
	wg.Wait()
	cache.Commit(results, time.Since(started).Seconds())
}
