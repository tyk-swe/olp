package codemode

import (
	"cmp"
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

func AssessBand(allowance *Allowance, model string, now time.Time) BandAssessment {
	if allowance == nil {
		return BandAssessment{Band: BandUnknown}
	}
	windows := slices.DeleteFunc(slices.Clone(allowanceWindows(*allowance)), func(window AllowanceWindow) bool {
		return model == "" || window.LimitName != model || window.EffectiveEvidence() != AllowanceEvidencePassive || !bandFresh(window, now) || window.Validate() != nil
	})
	if len(windows) == 0 {
		return BandAssessment{Band: BandUnknown}
	}
	for index, window := range windows {
		if window.MeterID != windows[0].MeterID || window.LimitID != windows[0].LimitID || slices.ContainsFunc(windows[index+1:], func(other AllowanceWindow) bool { return window.Window == other.Window }) {
			return BandAssessment{Band: BandUnknown}
		}
	}
	if exhausted := slices.IndexFunc(windows, func(window AllowanceWindow) bool {
		return bandExhausted(window)
	}); exhausted >= 0 {
		return bandFromWindow(BandExhausted, windows[exhausted], nil)
	}
	fresh := slices.DeleteFunc(slices.Clone(windows), func(window AllowanceWindow) bool {
		return window.EffectiveUnit() != AllowanceUnitPercent || !bandFresh(window, now)
	})
	if len(fresh) == 0 {
		return BandAssessment{Band: BandUnknown}
	}
	tightest := slices.MinFunc(fresh, func(left, right AllowanceWindow) int {
		return cmp.Compare(left.RemainingPercent, right.RemainingPercent)
	})
	if tightest.RemainingPercent > bandLowRemaining {
		return bandFromWindow(BandHealthy, tightest, nil)
	}
	return bandFromWindow(BandLow, tightest, bandSoonestReset(fresh, now))
}

func bandFromWindow(band Band, window AllowanceWindow, soonestReset *time.Time) BandAssessment {
	evidence := window.EffectiveEvidence()
	return BandAssessment{Band: band, Evidence: &evidence, ObservedAt: &window.ObservedAt, SoonestReset: soonestReset}
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
	switch window.EffectiveUnit() {
	case AllowanceUnitPercent:
		return window.RemainingPercent == 0
	case AllowanceUnitCount:
		return window.RemainingValue != nil && *window.RemainingValue == 0
	default:
		return false
	}
}

func bandSoonestReset(fresh []AllowanceWindow, now time.Time) *time.Time {
	lows := slices.DeleteFunc(slices.Clone(fresh), func(window AllowanceWindow) bool {
		return window.RemainingPercent > bandLowRemaining || window.ResetsAt == nil || !window.ResetsAt.After(now)
	})
	if len(lows) == 0 {
		return nil
	}
	return slices.MinFunc(lows, func(left, right AllowanceWindow) int {
		return left.ResetsAt.Compare(*right.ResetsAt)
	}).ResetsAt
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
