package observability

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/metric"
)

var (
	ready                  = metric.Gauge("olp_ready", "Whether the process currently satisfies the HTTP readiness contract.")
	readinessSnapshotAge   = metric.Gauge("olp_observability_readiness_snapshot_age_seconds", "Age of the last successful readiness snapshot.")
	metricsSnapshotAge     = metric.Gauge("olp_observability_metrics_snapshot_age_seconds", "Age of the last successful metrics snapshot.")
	readinessSnapshotFresh = metric.Gauge("olp_observability_readiness_snapshot_fresh", "Whether the readiness snapshot is fresh.")
	metricsSnapshotFresh   = metric.Gauge("olp_observability_metrics_snapshot_fresh", "Whether the metrics snapshot is fresh.")
)

// Handler is the private observability listener. It has its own listener so a
// flooded public listener can never starve health checks.
type Handler struct {
	cache   *Cache
	metrics http.Handler
}

func NewHandler(cache *Cache, live ...prometheus.Collector) (*Handler, error) {
	registry := prometheus.NewRegistry()
	for _, collector := range append([]prometheus.Collector{collectors.NewGoCollector(), cache.collector()}, live...) {
		if err := registry.Register(collector); err != nil {
			return nil, err
		}
	}
	return &Handler{cache: cache, metrics: promhttp.HandlerFor(registry, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})}, nil
}

func (h *Handler) ServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", h.live)
	mux.HandleFunc("GET /health/ready", h.ready)
	mux.HandleFunc("GET /metrics", h.serveMetrics)
	return mux
}

func (h *Handler) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, LiveHealth())
}

func (h *Handler) ready(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	snapshot := h.cache.readinessSnapshot()
	snapshot.attachFreshness(w, now)
	health, err := readyHealth(snapshot, now)
	if err != nil {
		access.WriteProblem(w, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (h *Handler) serveMetrics(w http.ResponseWriter, r *http.Request) {
	h.cache.metricsSnapshot().attachFreshness(w, time.Now())
	h.metrics.ServeHTTP(w, r)
}

// readyHealth refuses a stale snapshot: a collector that stopped succeeding
// cannot vouch for anything it reports.
func readyHealth(snapshot snapshot[*Health], now time.Time) (*Health, error) {
	if !snapshot.current(now) {
		return nil, access.Fail(http.StatusServiceUnavailable, "observability_snapshot_stale", "The readiness snapshot is stale.")
	}
	if snapshot.Value == nil {
		return nil, access.Fail(http.StatusServiceUnavailable, "observability_snapshot_unavailable", "No readiness snapshot has been collected.")
	}
	return snapshot.Value, nil
}

func (c *Cache) collector() prometheus.Collector {
	return metric.Collector(func() []prometheus.Metric {
		now := time.Now()
		readiness, metrics := c.readinessSnapshot(), c.metricsSnapshot()
		return append([]prometheus.Metric{
			ready.Sample(metric.Flag(readiness.current(now) && readiness.Value != nil)),
			readinessSnapshotAge.Sample(readiness.age(now)),
			metricsSnapshotAge.Sample(metrics.age(now)),
			readinessSnapshotFresh.Sample(metric.Flag(readiness.current(now))),
			metricsSnapshotFresh.Sample(metric.Flag(metrics.current(now))),
		}, metrics.Value...)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
