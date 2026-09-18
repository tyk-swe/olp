package process

import (
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func configureObservability(state *observability.State, rt *runtime.Manager, gw *gateway.Server, emitter *usage.Emitter, spool *media.Spool, policy *egress.Policy) {
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
			if release != nil && release.ID != "" && release.Snapshot != nil {
				ordinal := release.Snapshot.Generation.Ordinal
				probe.Generation = &ordinal
				for _, provider := range release.Snapshot.Providers {
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
		if gw.Admission != nil {
			admission := gw.Admission
			state.LimiterCounts = func() (failOpen, daily, monthly int64) {
				return admission.FailOpenTotal(),
					admission.BudgetRejections(limits.DimensionDailyCost),
					admission.BudgetRejections(limits.DimensionMonthlyCost)
			}
		}
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

func newLiveMetrics(rt *runtime.Manager, inferencePool, managementPool *observability.Pool) *observability.LiveMetrics {
	metrics := &observability.LiveMetrics{
		InferenceAdmission:  inferencePool,
		ManagementAdmission: managementPool,
	}
	if rt != nil {
		metrics.AuthorityAge = func() *float64 {
			status := rt.Authority()
			if !status.Loaded || status.ReadAt.IsZero() {
				return nil
			}
			age := time.Since(status.ReadAt).Seconds()
			return &age
		}
		metrics.DesiredGeneration = rt.DesiredGeneration
	}
	return metrics
}
