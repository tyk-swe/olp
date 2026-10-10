package usage

import (
	"errors"
	"math"
	"time"
)

type signalTransition struct {
	active        bool
	incident      int64
	lastTriggerAt *time.Time
	trigger       bool
	recover       bool
}

func transitionSignal(active bool, incident int64, lastTriggerAt *time.Time, desired bool, now time.Time, cooldown time.Duration) (signalTransition, error) {
	next := signalTransition{active: desired, incident: incident, lastTriggerAt: lastTriggerAt}
	if desired && !active {
		if incident == math.MaxInt64 {
			return signalTransition{}, errors.New("notification incident counter exhausted")
		}
		next.incident++
	}
	next.recover = active && !desired
	if desired && (lastTriggerAt == nil || !now.Before(lastTriggerAt.Add(cooldown))) {
		next.trigger = true
		next.lastTriggerAt = &now
	}
	return next, nil
}
