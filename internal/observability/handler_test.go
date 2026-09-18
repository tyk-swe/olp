package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReadinessServesCachedSnapshot(t *testing.T) {
	cache := NewCache()
	handler := NewHandler(cache, nil).ServeMux()

	// A failed collection leaves the snapshot stale: readiness cannot vouch
	// for anything it reports.
	cache.refreshReadiness(context.Background(), &State{
		PingDB: func(context.Context) error { return errors.New("offline") },
	}, testLog())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 503 {
		t.Fatalf("failed collection must be unready: %d", w.Code)
	}
	if w.Header().Get(headerSnapshotFresh) != "0" {
		t.Fatalf("stale snapshot must not claim freshness")
	}

	// A successful collection serves the payload it produced.
	cache.recordReadiness(&Health{Status: "ok", Database: "ok"})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 200 {
		t.Fatalf("fresh snapshot: %d", w.Code)
	}
	if w.Header().Get(headerSnapshotFresh) != "1" {
		t.Fatalf("fresh snapshot must claim freshness")
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("payload: %s", w.Body.String())
	}
}

func TestLivenessNeverChecksDependencies(t *testing.T) {
	cache := NewCache()
	handler := NewHandler(cache, nil).ServeMux()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/health/live", nil))
	if w.Code != 200 {
		t.Fatalf("liveness depends on services: %d", w.Code)
	}
}

func TestMetricsRenderSnapshotAndLiveSeries(t *testing.T) {
	cache := NewCache()
	cache.recordReadiness(&Health{Status: "ok"})
	cache.recordMetrics("# HELP olp_test_stub A stub.\n# TYPE olp_test_stub gauge\nolp_test_stub 1\n")
	handler := NewHandler(cache, &LiveMetrics{}).ServeMux()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("metrics: %d", w.Code)
	}
	body := w.Body.String()
	for _, series := range []string{
		"olp_ready 1",
		"olp_observability_readiness_snapshot_fresh 1",
		"olp_test_stub 1",
		"olp_api_key_authority_age_seconds +Inf",
		"olp_runtime_desired_generation 0",
		"olp_trace_export_dropped_total ",
	} {
		if !strings.Contains(body, series) {
			t.Fatalf("missing %q in:\n%s", series, body)
		}
	}
}
