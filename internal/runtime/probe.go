package runtime

import (
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

// Bounds of a connection's active health probe interval, in seconds.
const (
	MinProbeIntervalSeconds = 60
	MaxProbeIntervalSeconds = 86400
)

// HealthProbe opts a connection into active health probing: one bounded
// synthetic request per probed model each interval, across the whole fleet.
// Probes cost money, so a connection without one is never probed.
type HealthProbe struct {
	IntervalSeconds int64 `json:"interval_seconds"`
}

// Validate checks the probe interval.
func (p *HealthProbe) Validate(field string) error {
	if p.IntervalSeconds < MinProbeIntervalSeconds || p.IntervalSeconds > MaxProbeIntervalSeconds {
		return access.Invalid(field+".interval_seconds", "Probe at most once a minute and at least once a day.")
	}
	return nil
}

// Interval is the time between two probes of one model.
func (p *HealthProbe) Interval() time.Duration {
	return time.Duration(p.IntervalSeconds) * time.Second
}
