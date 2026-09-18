package observability

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	refreshTimeout        = 4 * time.Second
	readinessRefreshEvery = 5 * time.Second
	metricsRefreshEvery   = 15 * time.Second
	snapshotStaleAfter    = 30 * time.Second
)

// SnapshotFreshness carries the age headers the observability endpoints set.
const (
	headerSnapshotAge   = "X-Olp-Observability-Snapshot-Age-Seconds"
	headerSnapshotFresh = "X-Olp-Observability-Snapshot-Fresh"
)

// CachedReadiness is the process-local readiness snapshot the request path
// reads without touching dependencies.
type CachedReadiness struct {
	Result      *Health
	LastAttempt time.Time
	LastSuccess time.Time
}

// CachedMetrics is the last collected Prometheus exposition body.
type CachedMetrics struct {
	Body        string
	LastAttempt time.Time
	LastSuccess time.Time
}

// Cache holds the two snapshots the private listener serves. All dependency
// I/O happens in the background refresh loop; readers only ever take the
// latest pointer.
type Cache struct {
	readiness atomic.Pointer[CachedReadiness]
	metrics   atomic.Pointer[CachedMetrics]
}

// NewCache starts both snapshots empty.
func NewCache() *Cache {
	c := &Cache{}
	c.readiness.Store(&CachedReadiness{})
	c.metrics.Store(&CachedMetrics{})
	return c
}

// Readiness returns the latest readiness snapshot.
func (c *Cache) Readiness() CachedReadiness { return *c.readiness.Load() }

// Metrics returns the latest metrics snapshot.
func (c *Cache) Metrics() CachedMetrics { return *c.metrics.Load() }

func (c *Cache) recordReadiness(h *Health) {
	now := time.Now()
	c.readiness.Store(&CachedReadiness{Result: h, LastAttempt: now, LastSuccess: now})
}

func (c *Cache) recordReadinessFailure() {
	snapshot := *c.readiness.Load()
	snapshot.LastAttempt = time.Now()
	c.readiness.Store(&snapshot)
}

func (c *Cache) recordMetrics(body string) {
	now := time.Now()
	c.metrics.Store(&CachedMetrics{Body: body, LastAttempt: now, LastSuccess: now})
}

func (c *Cache) recordMetricsFailure() {
	snapshot := *c.metrics.Load()
	snapshot.LastAttempt = time.Now()
	c.metrics.Store(&snapshot)
}

// freshness helpers for the typed snapshots.
func readinessIsCurrent(s CachedReadiness, now time.Time) bool {
	return !s.LastSuccess.IsZero() && now.Sub(s.LastSuccess) <= snapshotStaleAfter && s.LastSuccess.Equal(s.LastAttempt)
}

func metricsIsCurrent(s CachedMetrics, now time.Time) bool {
	return !s.LastSuccess.IsZero() && now.Sub(s.LastSuccess) <= snapshotStaleAfter && s.LastSuccess.Equal(s.LastAttempt)
}

// attachFreshness sets the snapshot age headers on a response.
func attachFreshness(w http.ResponseWriter, success time.Time, fresh bool, now time.Time) {
	age := "unknown"
	if !success.IsZero() {
		age = strconv.FormatInt(int64(now.Sub(success)/time.Second), 10)
	}
	w.Header().Set(headerSnapshotAge, age)
	if fresh {
		w.Header().Set(headerSnapshotFresh, "1")
	} else {
		w.Header().Set(headerSnapshotFresh, "0")
	}
}

// Run refreshes readiness every five seconds and metrics every fifteen until
// ctx ends. The first refresh happens immediately so a brand-new process
// serves real state instead of "unavailable" for its first interval.
func (c *Cache) Run(ctx context.Context, state *State, log *slog.Logger) {
	c.refreshReadiness(ctx, state, log)
	c.refreshMetrics(ctx, state, log)
	readiness := time.NewTicker(readinessRefreshEvery)
	metrics := time.NewTicker(metricsRefreshEvery)
	defer readiness.Stop()
	defer metrics.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-readiness.C:
			c.refreshReadiness(ctx, state, log)
		case <-metrics.C:
			c.refreshMetrics(ctx, state, log)
		}
	}
}

func (c *Cache) refreshReadiness(ctx context.Context, state *State, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	health, err := CollectReadiness(ctx, state)
	if err != nil {
		log.Warn("observability readiness refresh failed", "error", err)
		c.recordReadinessFailure()
		return
	}
	c.recordReadiness(health)
}

func (c *Cache) refreshMetrics(ctx context.Context, state *State, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	body, err := CollectMetrics(ctx, state)
	if err != nil {
		log.Warn("observability metrics refresh failed", "error", err)
		c.recordMetricsFailure()
		return
	}
	c.recordMetrics(body)
}
