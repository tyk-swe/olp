package codemode

import (
	"slices"
	"time"
)

type Band string

const (
	BandHealthy   Band = "healthy"
	BandUnknown   Band = "unknown"
	BandLow       Band = "low"
	BandExhausted Band = "exhausted"
)

const (
	bandFreshness    = 15 * time.Minute
	bandLowRemaining = 10.0
)

type BandAssessment struct {
	Band         Band
	Evidence     *AllowanceEvidence
	ObservedAt   *time.Time
	SoonestReset *time.Time
}

func AssessBand(allowance *Allowance, now time.Time) BandAssessment {
	if allowance == nil {
		return BandAssessment{Band: BandUnknown}
	}
	fresh := slices.DeleteFunc(slices.Clone(allowanceWindows(*allowance)), func(window AllowanceWindow) bool {
		return !bandWindow(window) || !bandFresh(window, now)
	})
	if len(fresh) == 0 {
		return BandAssessment{Band: BandUnknown}
	}
	latest := slices.MaxFunc(fresh, func(left, right AllowanceWindow) int {
		return left.ObservedAt.Compare(right.ObservedAt)
	})
	evidence := latest.EffectiveEvidence()
	assessment := BandAssessment{Band: BandHealthy, Evidence: &evidence, ObservedAt: &latest.ObservedAt}
	if slices.ContainsFunc(fresh, bandExhausted) {
		return BandAssessment{Band: BandExhausted, Evidence: assessment.Evidence, ObservedAt: assessment.ObservedAt}
	}
	tightest, known := bandTightest(fresh)
	if !known || tightest > bandLowRemaining {
		return assessment
	}
	return BandAssessment{Band: BandLow, Evidence: assessment.Evidence, ObservedAt: assessment.ObservedAt, SoonestReset: bandSoonestReset(fresh, now)}
}

func bandWindow(window AllowanceWindow) bool {
	return window.EffectiveUnit() == AllowanceUnitPercent || window.EffectiveUnit() == AllowanceUnitCount
}

func bandFresh(window AllowanceWindow, now time.Time) bool {
	if window.ResetsAt != nil && window.ResetsAt.After(now) {
		return true
	}
	if window.ResetsAt != nil && window.ResetsAt.After(window.ObservedAt) {
		return false
	}
	return now.Sub(window.ObservedAt) <= bandFreshness
}

func bandExhausted(window AllowanceWindow) bool {
	if window.EffectiveUnit() == AllowanceUnitPercent {
		return window.RemainingPercent == 0
	}
	return window.RemainingValue != nil && *window.RemainingValue == 0
}

func bandRemaining(window AllowanceWindow) (float64, bool) {
	if window.EffectiveUnit() == AllowanceUnitPercent {
		return window.RemainingPercent, true
	}
	if window.UsedValue == nil || window.RemainingValue == nil || *window.UsedValue+*window.RemainingValue <= 0 {
		return 0, false
	}
	return *window.RemainingValue / (*window.UsedValue + *window.RemainingValue) * 100, true
}

func bandTightest(fresh []AllowanceWindow) (float64, bool) {
	tightest, known := 0.0, false
	for _, window := range fresh {
		remaining, ok := bandRemaining(window)
		if ok && (!known || remaining < tightest) {
			tightest, known = remaining, true
		}
	}
	return tightest, known
}

func bandSoonestReset(fresh []AllowanceWindow, now time.Time) *time.Time {
	var soonest *time.Time
	for _, window := range fresh {
		remaining, ok := bandRemaining(window)
		if !ok || remaining > bandLowRemaining || window.ResetsAt == nil || !window.ResetsAt.After(now) {
			continue
		}
		if soonest == nil || window.ResetsAt.Before(*soonest) {
			soonest = window.ResetsAt
		}
	}
	return soonest
}

func bandRank(band Band) int {
	switch band {
	case BandHealthy:
		return 3
	case BandLow:
		return 1
	case BandExhausted:
		return 0
	default:
		return 2
	}
}
