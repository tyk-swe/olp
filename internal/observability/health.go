package observability

import "time"

// HealthStats summarises gateway attempts against one provider.
type HealthStats struct {
	Attempts        int64
	Successes       int64
	RateLimits      int64
	ServerErrors    int64
	TransportErrors int64
	TotalLatency    time.Duration
	LastAttemptAt   time.Time
}

// HealthSource reports attempt statistics; nil when this process serves no
// inference.
type HealthSource interface {
	ProviderHealth(window time.Duration) map[string]HealthStats
}
