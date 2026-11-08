package main

import (
	"context"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/metric"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/usage"
)

type observed struct {
	region        string
	servesGateway bool
	pool          *pgxpool.Pool
	limiter       *coordination.Client
	runtime       *release.Manager
	policy        *egress.Policy
	gateway       *gateway.Server
	emitter       *usage.Emitter
	spool         *media.Spool
	mediaGaps     *atomic.Uint64
	loss          *usage.LossCounters
	export        func(context.Context) []prometheus.Metric
}

func observabilitySources(o observed) *observability.Sources {
	sources := &observability.Sources{
		Region:        o.region,
		Store:         o.pool,
		ServesGateway: o.servesGateway,
		MediaGaps:     o.mediaGaps,
		Loss:          o.loss,
		Export:        o.export,
	}
	if o.limiter != nil {
		sources.Limiter = o.limiter
	}
	if o.runtime != nil {
		sources.Runtime = runtimeSource{manager: o.runtime, policy: o.policy}
	}
	if o.gateway != nil {
		sources.Gateway = o.gateway
	}
	if o.emitter != nil {
		sources.Emitter = o.emitter
	}
	if o.spool != nil {
		sources.Spool = o.spool
	}
	return sources
}

type runtimeSource struct {
	manager *release.Manager
	policy  *egress.Policy
}

func (r runtimeSource) HasHardLimits() bool { return r.manager.HasHardLimits() }

func (r runtimeSource) Probe() observability.RuntimeProbe {
	probe := observability.RuntimeProbe{AllTransports: true, AuthorityStale: r.manager.Authority().Stale}
	current := r.manager.Release()
	if current == nil || current.ID == "" || current.Snapshot() == nil {
		return probe
	}
	ordinal := current.Snapshot().Generation().Ordinal
	probe.Generation = &ordinal
	for _, provider := range current.Snapshot().Providers() {
		if provider.Connector().Validate(r.policy) != nil {
			probe.AllTransports = false
			return probe
		}
	}
	return probe
}

var (
	authorityAge      = metric.Gauge("olp_api_key_authority_age_seconds", "Seconds since the current authority read began; infinity before the first successful read.")
	desiredGeneration = metric.Gauge("olp_runtime_desired_generation", "Latest observed desired runtime generation.")
)

func runtimeMetrics(rt *release.Manager) prometheus.Collector {
	return metric.Collector(func() []prometheus.Metric {
		if rt == nil {
			return []prometheus.Metric{authorityAge.Sample(math.Inf(1)), desiredGeneration.Sample(0)}
		}
		status := rt.Authority()
		age := math.Inf(1)
		if status.Loaded && !status.ReadAt.IsZero() {
			age = time.Since(status.ReadAt).Seconds()
		}
		return []prometheus.Metric{authorityAge.Sample(age), desiredGeneration.Sample(float64(rt.DesiredGeneration()))}
	})
}
