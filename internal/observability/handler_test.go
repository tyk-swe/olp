package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"runtime"
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
		"# TYPE go_memstats_mallocs_total counter",
		"# TYPE go_memstats_alloc_bytes_total counter",
	} {
		if !strings.Contains(body, series) {
			t.Fatalf("missing %q in:\n%s", series, body)
		}
	}
}

// TestAllocationCountersAdvanceWithAllocation: the harness measures a process's
// allocations per request as the difference of two scrapes, so the counters must
// be there and must grow when the process allocates.
func TestAllocationCountersAdvanceWithAllocation(t *testing.T) {
	read := func() (objects, bytes uint64) {
		var body strings.Builder
		writeAllocationMetrics(&body)
		for _, line := range strings.Split(body.String(), "\n") {
			var value uint64
			switch {
			case strings.HasPrefix(line, "go_memstats_mallocs_total "):
				fmt.Sscan(strings.TrimPrefix(line, "go_memstats_mallocs_total "), &value)
				objects = value
			case strings.HasPrefix(line, "go_memstats_alloc_bytes_total "):
				fmt.Sscan(strings.TrimPrefix(line, "go_memstats_alloc_bytes_total "), &value)
				bytes = value
			}
		}
		return objects, bytes
	}
	objectsBefore, bytesBefore := read()
	if objectsBefore == 0 || bytesBefore == 0 {
		t.Fatalf("a running process has allocated nothing: %d objects, %d bytes", objectsBefore, bytesBefore)
	}
	held := make([][]byte, 0, 20_000)
	for i := 0; i < 20_000; i++ {
		held = append(held, make([]byte, 1024))
	}
	objectsAfter, bytesAfter := read()
	if objectsAfter-objectsBefore < 10_000 || bytesAfter-bytesBefore < 10<<20 {
		t.Errorf("20,000 allocations of 1 KiB moved the counters by %d objects and %d bytes", objectsAfter-objectsBefore, bytesAfter-bytesBefore)
	}
	runtime.KeepAlive(held)
}
