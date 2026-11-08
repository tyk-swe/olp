package observability

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tyk-swe/olp/internal/metric"
)

const (
	refreshTimeout        = 4 * time.Second
	readinessRefreshEvery = 5 * time.Second
	metricsRefreshEvery   = 15 * time.Second
	snapshotStaleAfter    = 30 * time.Second
)

const (
	headerSnapshotAge   = "X-Olp-Observability-Snapshot-Age-Seconds"
	headerSnapshotFresh = "X-Olp-Observability-Snapshot-Fresh"
)

type snapshot[T any] struct {
	Value       T
	LastAttempt time.Time
	LastSuccess time.Time
}

func (s snapshot[T]) current(now time.Time) bool {
	return !s.LastSuccess.IsZero() && now.Sub(s.LastSuccess) <= snapshotStaleAfter && s.LastSuccess.Equal(s.LastAttempt)
}

func (s snapshot[T]) age(now time.Time) float64 {
	if s.LastSuccess.IsZero() {
		return math.Inf(1)
	}
	return float64(max(now.Sub(s.LastSuccess), 0) / time.Second)
}

func (s snapshot[T]) attachFreshness(w http.ResponseWriter, now time.Time) {
	age := "unknown"
	if !s.LastSuccess.IsZero() {
		age = strconv.FormatInt(int64(now.Sub(s.LastSuccess)/time.Second), 10)
	}
	w.Header().Set(headerSnapshotAge, age)
	w.Header().Set(headerSnapshotFresh, strconv.Itoa(int(metric.Flag(s.current(now)))))
}

type Cache struct {
	readiness atomic.Pointer[snapshot[*Health]]
	metrics   atomic.Pointer[snapshot[[]prometheus.Metric]]
}

func NewCache() *Cache {
	c := &Cache{}
	c.readiness.Store(&snapshot[*Health]{})
	c.metrics.Store(&snapshot[[]prometheus.Metric]{})
	return c
}

func (c *Cache) readinessSnapshot() snapshot[*Health] { return *c.readiness.Load() }

func (c *Cache) metricsSnapshot() snapshot[[]prometheus.Metric] { return *c.metrics.Load() }

func record[T any](latest *atomic.Pointer[snapshot[T]], value T, err error) {
	now := time.Now()
	if err != nil {
		previous := latest.Load()
		latest.Store(&snapshot[T]{Value: previous.Value, LastAttempt: now, LastSuccess: previous.LastSuccess})
		return
	}
	latest.Store(&snapshot[T]{Value: value, LastAttempt: now, LastSuccess: now})
}

func (c *Cache) Run(ctx context.Context, state *State, log *slog.Logger) {
	c.refreshReadiness(ctx, state, log)
	c.refreshMetrics(ctx, state)
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
			c.refreshMetrics(ctx, state)
		}
	}
}

func (c *Cache) refreshReadiness(ctx context.Context, state *State, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	health, err := CollectReadiness(ctx, state)
	if err != nil {
		log.Warn("observability readiness refresh failed", "error", err)
	}
	record(&c.readiness, health, err)
}

func (c *Cache) refreshMetrics(ctx context.Context, state *State) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	record(&c.metrics, CollectMetrics(ctx, state), nil)
}
