package main

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/metric"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/usage"
)

func configureObservability(state *observability.State, rt *release.Manager, gw *gateway.Server, emitter *usage.Emitter, spool *media.Spool, policy *egress.Policy) {
	if rt != nil {
		state.Runtime = func() observability.RuntimeProbe {
			probe := observability.RuntimeProbe{AllTransports: true}
			status := rt.Authority()
			probe.AuthorityStale = status.Stale
			if status.Loaded && !status.ReadAt.IsZero() {
				age := time.Since(status.ReadAt)
				probe.AuthorityAge = &age
			}
			release := rt.Release()
			if release != nil && release.ID != "" && release.Snapshot() != nil {
				ordinal := release.Snapshot().Generation().Ordinal
				probe.Generation = &ordinal
				for _, provider := range release.Snapshot().Providers() {
					p := provider
					if err := p.Connector().Validate(policy); err != nil {
						probe.AllTransports = false
						break
					}
				}
			}
			return probe
		}
		state.HardLimits = rt.HasHardLimits
	}
	if gw != nil {
		state.Circuits = gw.OpenCircuits
		state.Shadows = gw.ShadowCounts
		state.LimiterCounts = gw.LimiterCounts()
	}
	if emitter != nil {
		state.Emitter = func() *usage.Snapshot {
			snapshot := emitter.Snapshot()
			return &snapshot
		}
	}
	if spool != nil {
		state.Spool = func() (capacity, used *int64) {
			capacityBytes, usedBytes := spool.CapacityBytes(), spool.UsedBytes()
			return &capacityBytes, &usedBytes
		}
	}
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
