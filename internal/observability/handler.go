package observability

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/telemetry"
)

// Handler is the private observability listener: liveness, cached readiness,
// and the Prometheus exposition. It is bound on its own listener so a flooded
// public listener can never starve health checks.
type Handler struct {
	cache       *Cache
	liveMetrics *LiveMetrics
}

// LiveMetrics renders the series that must reflect the process at request
// time rather than at the last refresh: authority age, the desired runtime
// generation, HTTP admission, and the trace-drop counter.
type LiveMetrics struct {
	// AuthorityAge is the age of the current authority read; nil renders +Inf.
	AuthorityAge func() *float64
	// DesiredGeneration is the newest generation ordinal observed in storage.
	DesiredGeneration func() int64
	// InferenceAdmission and ManagementAdmission are the public listener's
	// process-local admission pools.
	InferenceAdmission  *Pool
	ManagementAdmission *Pool
}

// NewHandler mounts the endpoints the private listener serves.
func NewHandler(cache *Cache, live *LiveMetrics) *Handler {
	return &Handler{cache: cache, liveMetrics: live}
}

// ServeMux returns the private listener's routes.
func (h *Handler) ServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", h.live)
	mux.HandleFunc("GET /health/ready", h.ready)
	mux.HandleFunc("GET /metrics", h.metrics)
	return mux
}

func (h *Handler) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, LiveHealth())
}

// ready serves the cached readiness snapshot. A stale snapshot is a 503: a
// collector that stopped succeeding cannot vouch for anything it reports.
func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	snapshot := h.cache.Readiness()
	fresh := readinessIsCurrent(snapshot, now)
	attachFreshness(w, snapshot.LastSuccess, fresh, now)
	if !fresh {
		writeProblem(w, http.StatusServiceUnavailable, "observability_snapshot_stale", "The readiness snapshot is stale.")
		return
	}
	if snapshot.Result == nil {
		writeProblem(w, http.StatusServiceUnavailable, "observability_snapshot_unavailable", "No readiness snapshot has been collected.")
		return
	}
	writeJSON(w, http.StatusOK, snapshot.Result)
}

// metrics serves the cached exposition plus the series that must reflect the
// process at request time: snapshot freshness, authority age, desired
// generation, admission, and trace drops.
func (h *Handler) metrics(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	readiness := h.cache.Readiness()
	metrics := h.cache.Metrics()
	readinessFresh := readinessIsCurrent(readiness, now)
	metricsFresh := metricsIsCurrent(metrics, now)
	readinessAge, metricsAge := "18446744073709551615", "18446744073709551615"
	if !readiness.LastSuccess.IsZero() {
		readinessAge = ageSeconds(readiness.LastSuccess, now)
	}
	if !metrics.LastSuccess.IsZero() {
		metricsAge = ageSeconds(metrics.LastSuccess, now)
	}
	var body strings.Builder
	fmt.Fprintf(&body,
		"# HELP olp_ready Whether the process currently satisfies the HTTP readiness contract.\n"+
			"# TYPE olp_ready gauge\n"+
			"olp_ready %s\n"+
			"# HELP olp_observability_readiness_snapshot_age_seconds Age of the last successful readiness snapshot.\n"+
			"# TYPE olp_observability_readiness_snapshot_age_seconds gauge\n"+
			"olp_observability_readiness_snapshot_age_seconds %s\n"+
			"# HELP olp_observability_metrics_snapshot_age_seconds Age of the last successful metrics snapshot.\n"+
			"# TYPE olp_observability_metrics_snapshot_age_seconds gauge\n"+
			"olp_observability_metrics_snapshot_age_seconds %s\n"+
			"# HELP olp_observability_readiness_snapshot_fresh Whether the readiness snapshot is fresh.\n"+
			"# TYPE olp_observability_readiness_snapshot_fresh gauge\n"+
			"olp_observability_readiness_snapshot_fresh %s\n"+
			"# HELP olp_observability_metrics_snapshot_fresh Whether the metrics snapshot is fresh.\n"+
			"# TYPE olp_observability_metrics_snapshot_fresh gauge\n"+
			"olp_observability_metrics_snapshot_fresh %s\n",
		boolMetric(readinessFresh && readiness.Result != nil),
		readinessAge, metricsAge,
		boolMetric(readinessFresh), boolMetric(metricsFresh))
	body.WriteString(metrics.Body)
	authorityAge := "+Inf"
	var desiredGeneration int64
	if h.liveMetrics != nil {
		if h.liveMetrics.AuthorityAge != nil {
			if age := h.liveMetrics.AuthorityAge(); age != nil {
				authorityAge = strconv.FormatFloat(*age, 'f', -1, 64)
			}
		}
		if h.liveMetrics.DesiredGeneration != nil {
			desiredGeneration = h.liveMetrics.DesiredGeneration()
		}
	}
	fmt.Fprintf(&body,
		"# HELP olp_api_key_authority_age_seconds Seconds since the current authority read began; infinity before the first successful read.\n"+
			"# TYPE olp_api_key_authority_age_seconds gauge\n"+
			"olp_api_key_authority_age_seconds %s\n"+
			"# HELP olp_runtime_desired_generation Latest observed desired runtime generation.\n"+
			"# TYPE olp_runtime_desired_generation gauge\n"+
			"olp_runtime_desired_generation %d\n",
		authorityAge, desiredGeneration)
	if h.liveMetrics != nil {
		AdmissionMetrics(h.liveMetrics.InferenceAdmission, h.liveMetrics.ManagementAdmission, &body)
	}
	body.WriteString("# HELP olp_trace_export_dropped_total Spans dropped before successful OTLP export.\n" +
		"# TYPE olp_trace_export_dropped_total counter\n")
	fmt.Fprintf(&body, "olp_trace_export_dropped_total %d\n", telemetry.ExportDroppedTotal())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	attachFreshness(w, metrics.LastSuccess, metricsFresh, now)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(body.String()))
}

func boolMetric(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func ageSeconds(at, now time.Time) string {
	age := max(now.Sub(at), 0)
	return strconvInt(int64(age / time.Second))
}

func strconvInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// writeProblem renders an RFC 9457 problem document.
func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": http.StatusText(status), "status": status, "code": code, "detail": detail,
	})
}
