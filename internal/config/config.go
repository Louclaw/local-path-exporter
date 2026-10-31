// Package config reads process settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	DefaultStoragePath = "/var/lib/rancher/k3s/storage"
	DefaultMetricsAddr = ":9100"
	DefaultInterval    = 5 * time.Minute
	DefaultTimeout     = 2 * time.Minute
	DefaultParallelism = 1
)

// Config is the process configuration.
type Config struct {
	NodeName     string
	StoragePath  string
	MetricsAddr  string
	ScanInterval time.Duration
	ScanTimeout  time.Duration
	Parallelism  int
}

// FromEnv loads Config. NODE_NAME is required.
func FromEnv() (Config, error) {
	cfg := Config{
		NodeName:     os.Getenv("NODE_NAME"),
		StoragePath:  envOr("STORAGE_PATH", DefaultStoragePath),
		MetricsAddr:  envOr("METRICS_ADDR", DefaultMetricsAddr),
		ScanInterval: DefaultInterval,
		ScanTimeout:  DefaultTimeout,
		Parallelism:  DefaultParallelism,
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
	if cfg.ScanInterval <= 0 {
		return Config{}, fmt.Errorf("SCAN_INTERVAL must be positive")
	}
	if cfg.ScanTimeout <= 0 {
		return Config{}, fmt.Errorf("SCAN_TIMEOUT must be positive")
	}
	if cfg.Parallelism < 1 {
		return Config{}, fmt.Errorf("SCAN_PARALLELISM must be >= 1")
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
