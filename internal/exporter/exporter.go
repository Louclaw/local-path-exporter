// Package exporter runs one bounded scan cycle and commits it to the cache.
package exporter

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Louclaw/local-path-exporter/internal/mapping"
	"github.com/Louclaw/local-path-exporter/internal/metrics"
	"github.com/Louclaw/local-path-exporter/internal/scan"
	"github.com/Louclaw/local-path-exporter/internal/stats"
)

// InspectFunc measures one directory. The context carries the per-volume timeout.
type InspectFunc func(ctx context.Context, path string) (scan.Report, error)

// Pacer waits longer between walks of bigger volumes. The wait is
// base * max(1, entries/perStep), capped at cap. A nil Pacer, a non-positive
// base, or a non-positive perStep scans every volume every cycle.
type Pacer struct {
	mu      sync.Mutex
	base    time.Duration
	perStep int64
	cap     time.Duration
	last    map[string]time.Time
	entries map[string]int64
}

// NewPacer returns a pacer. perStep is how many tracked entries add one base interval.
func NewPacer(base time.Duration, perStep int64, cap time.Duration) *Pacer {
	return &Pacer{
		base:    base,
		perStep: perStep,
		cap:     cap,
		last:    map[string]time.Time{},
		entries: map[string]int64{},
	}
}

func (p *Pacer) interval(entries int64) time.Duration {
	if p == nil || p.base <= 0 || p.perStep <= 0 {
		return 0
	}
	steps := entries / p.perStep
	if steps < 1 {
		steps = 1
	}
	d := time.Duration(steps) * p.base
	if p.cap > 0 && d > p.cap {
		return p.cap
	}
	return d
}

// Skipping reports whether the volume is still inside its wait.
func (p *Pacer) Skipping(ns, name string, now time.Time) bool {
	skip, _, _ := p.skip(ns, name, now)
	return skip
}

func (p *Pacer) skip(ns, name string, now time.Time) (bool, int64, time.Duration) {
	if p == nil || p.base <= 0 || p.perStep <= 0 {
		return false, 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := ns + "\x00" + name
	n := p.entries[k]
	last, ok := p.last[k]
	if !ok {
		return false, n, 0
	}
	wait := p.interval(n)
	age := now.Sub(last)
	if wait <= 0 || age >= wait {
		return false, n, age
	}
	return true, n, age
}

func (p *Pacer) observe(ns, name string, entries int64, now time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := ns + "\x00" + name
	p.last[k] = now
	p.entries[k] = entries
}

// RunOnce measures vols with at most parallelism walks at a time.
// Each walk has its own deadline. A full-scan pause extends that deadline,
// so the timeout does not include voluntary backoff. When a walk returns,
// unused pages are released before the semaphore is freed, so the next volume
// does not start on top of the previous heap. The cache is updated once,
// after every walk returns. pace may be nil.
func RunOnce(ctx context.Context, vols []mapping.Volume, inspect InspectFunc, cache *metrics.Cache, parallelism int, timeout time.Duration, started time.Time, pace *Pacer) {
	if parallelism < 1 {
		parallelism = 1
	}
	if timeout <= 0 {
		timeout = time.Minute
	}
	results := make([]metrics.Result, len(vols))
	var wg sync.WaitGroup
	var traversed atomic.Int64
	sem := make(chan struct{}, parallelism)
	for i, vol := range vols {
		wg.Add(1)
		go func(i int, vol mapping.Volume) {
			defer wg.Done()
			if skip, n, age := pace.skip(vol.Namespace, vol.Name, time.Now()); skip {
				if sample, ok := cache.Previous(vol.Namespace, vol.Name); ok {
					results[i] = metrics.Result{Namespace: vol.Namespace, PVC: vol.Name, Sample: sample}
					slog.Info("skip volume", "path", vol.Path, "tracked", n, "age", age.String())
					return
				}
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				defer debug.FreeOSMemory()
			case <-ctx.Done():
				results[i] = metrics.Result{Namespace: vol.Namespace, PVC: vol.Name, Err: ctx.Err()}
				return
			}
			if err := ctx.Err(); err != nil {
				results[i] = metrics.Result{Namespace: vol.Namespace, PVC: vol.Name, Err: err}
				return
			}
			cctx, cancel := scan.WithBudget(ctx, timeout)
			defer cancel()
			rep, err := inspect(cctx, vol.Path)
			if errors.Is(err, scan.ErrDeferred) {
				results[i] = metrics.Result{Namespace: vol.Namespace, PVC: vol.Name, Deferred: true}
				return
			}
			if err != nil {
				results[i] = metrics.Result{Namespace: vol.Namespace, PVC: vol.Name, Err: err}
				return
			}
			traversed.Add(rep.Usage.Entries)
			tracked := rep.Usage.Tracked
			if tracked <= 0 {
				tracked = rep.Usage.Entries
			}
			pace.observe(vol.Namespace, vol.Name, tracked, time.Now())
			capacity, emit := vol.Capacity, vol.CapacityFromPVC && vol.Capacity > 0
			if !emit && rep.TotalBytes > 0 {
				capacity, emit = rep.TotalBytes, true
			}
			results[i] = metrics.Result{
				Namespace: vol.Namespace,
				PVC:       vol.Name,
				Sample: metrics.NewSample(
					vol.Namespace, vol.Name, vol.PV, vol.Node,
					capacity,
					rep.Usage.Bytes, rep.Usage.Inodes,
					rep.AvailBytes, rep.Files, rep.FilesFree,
					emit,
					stats.AvailableBytes,
				),
			}
		}(i, vol)
	}
	wg.Wait()
	cache.SetEntries(float64(traversed.Load()))
	cache.Commit(results, time.Since(started).Seconds())
}
