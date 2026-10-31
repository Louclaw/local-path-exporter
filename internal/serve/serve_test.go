package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Louclaw/local-path-exporter/internal/metrics"
)

func TestProbesAndMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	cache := metrics.NewCache()
	reg.MustRegister(metrics.NewCollector(cache))
	var ready atomic.Bool
	h := Handler(reg, &ready)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before scan %d", rr.Code)
	}
	ready.Store(true)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("readyz after scan %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "local_path_exporter_scan_errors_total") {
		t.Fatalf("body %s", rr.Body.String())
	}
}
