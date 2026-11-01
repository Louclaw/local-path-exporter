// Package config reads process settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	DefaultStoragePath    = "/var/lib/rancher/k3s/storage"
	DefaultMetricsAddr    = ":9100"
	DefaultInterval       = 5 * time.Minute
	DefaultTimeout        = 2 * time.Minute
	DefaultParallelism    = 1
	DefaultEntriesPerStep = 50_000
	DefaultFullScan       = 6 * time.Hour
	DefaultCacheMaxBytes  = 8 << 20
	// DefaultThrottleHigh is memory.current, in bytes, above which a full scan pauses.
	DefaultThrottleHigh = 100 << 20
	// DefaultThrottleBatch is how many walked entries elapse between those checks.
	// One batch is about 2–3 MiB of kernel slab, inside the gap from 100 MiB to the 128 MiB limit.
	DefaultThrottleBatch = 2048
	// DefaultMaxPerSec caps every walk, including the incremental ones that
	// stay under the memory threshold. 0 disables the cap.
	DefaultMaxPerSec int64 = 4000
)

// Config is the process configuration.
type Config struct {
	NodeName       string
	StoragePath    string
	MetricsAddr    string
	ScanInterval   time.Duration
	ScanTimeout    time.Duration
	Parallelism    int
	EntriesPerStep int64
	FullScan       time.Duration
	CacheMaxBytes  int64
	// CachePath is an optional file on a writable emptyDir. Empty disables persistence.
	CachePath     string
	ThrottleHigh  int64
	ThrottleBatch int64
	MaxPerSec     int64
}

// FromEnv loads Config. NODE_NAME is required.
func FromEnv() (Config, error) {
	cfg := Config{
		NodeName:       os.Getenv("NODE_NAME"),
		StoragePath:    envOr("STORAGE_PATH", DefaultStoragePath),
		MetricsAddr:    envOr("METRICS_ADDR", DefaultMetricsAddr),
		ScanInterval:   DefaultInterval,
		ScanTimeout:    DefaultTimeout,
		Parallelism:    DefaultParallelism,
		EntriesPerStep: DefaultEntriesPerStep,
		FullScan:       DefaultFullScan,
		CacheMaxBytes:  DefaultCacheMaxBytes,
		CachePath:      os.Getenv("SCAN_CACHE_PATH"),
		ThrottleHigh:   DefaultThrottleHigh,
		ThrottleBatch:  DefaultThrottleBatch,
		MaxPerSec:      DefaultMaxPerSec,
	}
	if cfg.NodeName == "" {
		return Config{}, fmt.Errorf("NODE_NAME is required")
	}
	var err error
	if cfg.ScanInterval, err = durationEnv("SCAN_INTERVAL", DefaultInterval); err != nil {
		return Config{}, err
	}
	if cfg.ScanTimeout, err = durationEnv("SCAN_TIMEOUT", DefaultTimeout); err != nil {
		return Config{}, err
	}
	if cfg.Parallelism, err = intEnv("SCAN_PARALLELISM", DefaultParallelism); err != nil {
		return Config{}, err
	}
	if cfg.EntriesPerStep, err = int64Env("SCAN_ENTRIES_PER_STEP", DefaultEntriesPerStep); err != nil {
		return Config{}, err
	}
	if cfg.FullScan, err = durationEnv("FULL_SCAN_INTERVAL", DefaultFullScan); err != nil {
		return Config{}, err
	}
	if cfg.CacheMaxBytes, err = int64Env("SCAN_CACHE_MAX_BYTES", DefaultCacheMaxBytes); err != nil {
		return Config{}, err
	}
	if cfg.ThrottleHigh, err = int64Env("SCAN_THROTTLE_HIGH", DefaultThrottleHigh); err != nil {
		return Config{}, err
	}
	if cfg.ThrottleBatch, err = int64Env("SCAN_THROTTLE_BATCH", DefaultThrottleBatch); err != nil {
		return Config{}, err
	}
	if cfg.MaxPerSec, err = int64Env("SCAN_MAX_ENTRIES_PER_SEC", DefaultMaxPerSec); err != nil {
		return Config{}, err
	}
	if cfg.ScanInterval <= 0 {
		return Config{}, fmt.Errorf("SCAN_INTERVAL must be positive")
	}
	if cfg.ScanTimeout <= 0 {
		return Config{}, fmt.Errorf("SCAN_TIMEOUT must be positive")
	}
	if cfg.Parallelism < 1 {
		return Config{}, fmt.Errorf("SCAN_PARALLELISM must be >= 1")
	}
	if cfg.EntriesPerStep < 0 {
		return Config{}, fmt.Errorf("SCAN_ENTRIES_PER_STEP must be >= 0")
	}
	if cfg.FullScan < 6*time.Hour || cfg.FullScan > 24*time.Hour {
		return Config{}, fmt.Errorf("FULL_SCAN_INTERVAL must be between 6h and 24h")
	}
	if cfg.CacheMaxBytes < 0 {
		return Config{}, fmt.Errorf("SCAN_CACHE_MAX_BYTES must be >= 0")
	}
	if cfg.ThrottleHigh < 0 {
		return Config{}, fmt.Errorf("SCAN_THROTTLE_HIGH must be >= 0")
	}
	if cfg.ThrottleBatch < 0 {
		return Config{}, fmt.Errorf("SCAN_THROTTLE_BATCH must be >= 0")
	}
	if cfg.MaxPerSec < 0 {
		return Config{}, fmt.Errorf("SCAN_MAX_ENTRIES_PER_SEC must be >= 0")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func int64Env(key string, fallback int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func intEnv(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
