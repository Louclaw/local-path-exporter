package config

import (
	"testing"
	"time"
)

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("NODE_NAME", "node-a")
	t.Setenv("STORAGE_PATH", "")
	t.Setenv("METRICS_ADDR", "")
	t.Setenv("SCAN_INTERVAL", "")
	t.Setenv("SCAN_TIMEOUT", "")
	t.Setenv("SCAN_PARALLELISM", "")
	t.Setenv("SCAN_ENTRIES_PER_STEP", "")
	t.Setenv("FULL_SCAN_INTERVAL", "")
	t.Setenv("SCAN_CACHE_MAX_BYTES", "")
	t.Setenv("SCAN_CACHE_PATH", "")
	t.Setenv("SCAN_THROTTLE_HIGH", "")
	t.Setenv("SCAN_THROTTLE_BATCH", "")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeName != "node-a" || cfg.StoragePath != DefaultStoragePath || cfg.MetricsAddr != DefaultMetricsAddr {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.ScanInterval != 5*time.Minute || cfg.ScanTimeout != 2*time.Minute || cfg.Parallelism != 1 {
		t.Fatalf("unexpected scan defaults: %+v", cfg)
	}
	if cfg.EntriesPerStep != DefaultEntriesPerStep || cfg.FullScan != DefaultFullScan || cfg.CacheMaxBytes != DefaultCacheMaxBytes {
		t.Fatalf("unexpected incremental defaults: %+v", cfg)
	}
	if cfg.CachePath != "" || cfg.ThrottleHigh != DefaultThrottleHigh || cfg.ThrottleBatch != DefaultThrottleBatch || cfg.MaxPerSec != DefaultMaxPerSec {
		t.Fatalf("unexpected throttle defaults: %+v", cfg)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("NODE_NAME", "n")
	t.Setenv("STORAGE_PATH", "/data")
	t.Setenv("METRICS_ADDR", ":8080")
	t.Setenv("SCAN_INTERVAL", "30s")
	t.Setenv("SCAN_TIMEOUT", "1m")
	t.Setenv("SCAN_PARALLELISM", "4")
	t.Setenv("SCAN_ENTRIES_PER_STEP", "1000")
	t.Setenv("FULL_SCAN_INTERVAL", "12h")
	t.Setenv("SCAN_CACHE_MAX_BYTES", "4096")
	t.Setenv("SCAN_CACHE_PATH", "/var/lib/local-path-exporter/cache.v1")
	t.Setenv("SCAN_THROTTLE_HIGH", "0")
	t.Setenv("SCAN_THROTTLE_BATCH", "512")
	t.Setenv("SCAN_MAX_ENTRIES_PER_SEC", "1000")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StoragePath != "/data" || cfg.MetricsAddr != ":8080" || cfg.ScanInterval != 30*time.Second || cfg.ScanTimeout != time.Minute || cfg.Parallelism != 4 || cfg.EntriesPerStep != 1000 || cfg.FullScan != 12*time.Hour || cfg.CacheMaxBytes != 4096 {
		t.Fatalf("unexpected override: %+v", cfg)
	}
	if cfg.CachePath != "/var/lib/local-path-exporter/cache.v1" || cfg.ThrottleHigh != 0 || cfg.ThrottleBatch != 512 || cfg.MaxPerSec != 1000 {
		t.Fatalf("unexpected throttle override: %+v", cfg)
	}
}

func TestFromEnvRequiresNode(t *testing.T) {
	t.Setenv("NODE_NAME", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error")
	}
}

func TestFromEnvRejectsBadValues(t *testing.T) {
	t.Setenv("NODE_NAME", "n")
	t.Setenv("SCAN_INTERVAL", "0s")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected interval error")
	}
	t.Setenv("SCAN_INTERVAL", "5m")
	t.Setenv("SCAN_PARALLELISM", "0")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected parallelism error")
	}
	t.Setenv("SCAN_PARALLELISM", "1")
	t.Setenv("SCAN_ENTRIES_PER_STEP", "-1")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected entries-per-step error")
	}
	t.Setenv("SCAN_ENTRIES_PER_STEP", "50000")
	t.Setenv("FULL_SCAN_INTERVAL", "1h")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected full scan below 6h to fail")
	}
	t.Setenv("FULL_SCAN_INTERVAL", "48h")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected full scan above 24h to fail")
	}
	t.Setenv("FULL_SCAN_INTERVAL", "6h")
	t.Setenv("SCAN_THROTTLE_HIGH", "-1")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected throttle high to fail")
	}
	t.Setenv("SCAN_THROTTLE_HIGH", "0")
	t.Setenv("SCAN_THROTTLE_BATCH", "-1")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected throttle batch to fail")
	}
}
