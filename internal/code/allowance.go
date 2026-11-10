package code

import (
	"bytes"
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	MaxAllowanceWindows     = 128
	MaxAllowanceChargeBytes = 4096
	allowancePrimary        = "primary"
	codexLimitID            = "codex"
)

func ValidateAllowanceCharge(charge json.RawMessage) error {
	trimmed := bytes.TrimSpace(charge)
	if len(trimmed) < 2 || len(trimmed) > MaxAllowanceChargeBytes || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' || !json.Valid(trimmed) {
		return fmt.Errorf("invalid allowance charge")
	}
	return nil
}

type AllowanceUnit string

const (
	AllowanceUnitPercent AllowanceUnit = "percent"
	AllowanceUnitCount   AllowanceUnit = "count"
	AllowanceUnitCredits AllowanceUnit = "credits"
	AllowanceUnitCents   AllowanceUnit = "cents"
)

type AllowanceEvidence string

const (
	AllowanceEvidencePassive AllowanceEvidence = "passive"
	AllowanceEvidenceActive  AllowanceEvidence = "active"
)

func AllowanceWindowNames() [5]string {
	return [5]string{allowancePrimary, "secondary", "five_hour", "weekly", "monthly"}
}

var (
	limitID       = regexp.MustCompile(`^[a-z0-9][a-z0-9_.]{0,99}$`)
	creditBalance = regexp.MustCompile(`^-?[0-9]{1,20}(\.[0-9]{1,12})?$`)
)

func NormalizeLimitID(raw string) (string, bool) {
	id := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), "-", "_")
	return id, limitID.MatchString(id)
}

type CountObservation struct {
	ResetsAt   *time.Time `json:"resets_at"`
	ObservedAt time.Time  `json:"observed_at"`
}

type AllowanceWindow struct {
	LimitID          string            `json:"limit_id"`
	Window           string            `json:"window"`
	Unit             AllowanceUnit     `json:"unit"`
	Evidence         AllowanceEvidence `json:"evidence"`
	UsedPercent      float64           `json:"used_percent"`
	RemainingPercent float64           `json:"remaining_percent"`
	UsedValue        *float64          `json:"used_value"`
	RemainingValue   *float64          `json:"remaining_value"`
	WindowMinutes    *int64            `json:"window_minutes"`
	ResetsAt         *time.Time        `json:"resets_at"`
	ObservedAt       time.Time         `json:"observed_at"`
}

func (w AllowanceWindow) Normalized() AllowanceWindow {
	result := w
	if result.Unit == "" {
		result.Unit = AllowanceUnitPercent
	}
	if result.Evidence == "" {
		result.Evidence = AllowanceEvidencePassive
	}
	return result
}

func (w *AllowanceWindow) UnmarshalJSON(data []byte) error {
	type rawWindow AllowanceWindow
	var decoded rawWindow
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*w = AllowanceWindow(decoded).Normalized()
	return nil
}

func (w AllowanceWindow) Validate() error {
	normalized := w.Normalized()
	names := AllowanceWindowNames()
	if !limitID.MatchString(normalized.LimitID) || !slices.Contains(names[:], normalized.Window) || normalized.ObservedAt.IsZero() ||
		!slices.Contains([]AllowanceEvidence{AllowanceEvidencePassive, AllowanceEvidenceActive}, normalized.Evidence) ||
		normalized.WindowMinutes != nil && (*normalized.WindowMinutes < 0 || *normalized.WindowMinutes > 1<<53-1) ||
		normalized.ResetsAt != nil && (normalized.ResetsAt.Unix() <= 0 || normalized.ResetsAt.Unix() >= 253402300800) {
		return fmt.Errorf("invalid provider allowance window")
	}
	switch normalized.Unit {
	case AllowanceUnitPercent:
		if normalized.UsedValue != nil || normalized.RemainingValue != nil ||
			math.IsNaN(normalized.UsedPercent) || math.IsInf(normalized.UsedPercent, 0) || normalized.UsedPercent < 0 || normalized.UsedPercent > 1<<53-1 ||
			normalized.RemainingPercent != max(0, 100-normalized.UsedPercent) {
			return fmt.Errorf("invalid provider allowance window")
		}
	case AllowanceUnitCount, AllowanceUnitCredits, AllowanceUnitCents:
		if normalized.UsedPercent != 0 || normalized.RemainingPercent != 0 {
			return fmt.Errorf("invalid provider allowance window")
		}
		for _, value := range []*float64{normalized.UsedValue, normalized.RemainingValue} {
			if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1<<53-1) {
				return fmt.Errorf("invalid provider allowance window")
			}
		}
	default:
		return fmt.Errorf("invalid provider allowance window")
	}
	return nil
}

type Credits struct {
	HasCredits bool      `json:"has_credits"`
	Unlimited  bool      `json:"unlimited"`
	Balance    *string   `json:"balance"`
	ObservedAt time.Time `json:"observed_at"`
}

func (c Credits) Validate() error {
	if c.ObservedAt.IsZero() || c.Balance != nil && !creditBalance.MatchString(*c.Balance) {
		return fmt.Errorf("invalid provider credits")
	}
	return nil
}

type Allowance struct {
	Windows            []AllowanceWindow `json:"windows,omitempty"`
	Credits            *Credits          `json:"credits,omitempty"`
	RemainingTokens    *int64            `json:"remaining_tokens"`
	RemainingRequests  *int64            `json:"remaining_requests"`
	TokenObservation   *CountObservation `json:"token_observation,omitempty"`
	RequestObservation *CountObservation `json:"request_observation,omitempty"`
	RemainingPercent   *float64          `json:"remaining_percent"`
	ResetsAt           *time.Time        `json:"resets_at"`
	ObservedAt         time.Time         `json:"observed_at"`
}

func NewAllowance(observedAt time.Time, windows []AllowanceWindow, credits *Credits) Allowance {
	normalized := make([]AllowanceWindow, len(windows))
	for i, w := range windows {
		normalized[i] = w.Normalized()
	}
	slices.SortFunc(normalized, compareAllowanceWindows)
	var percent *float64
	var reset *time.Time
	for _, w := range normalized {
		if w.LimitID == codexLimitID && w.Window == allowancePrimary && w.Unit == AllowanceUnitPercent && w.Evidence == AllowanceEvidencePassive {
			remaining := w.RemainingPercent
			percent, reset = &remaining, w.ResetsAt
		}
	}
	return Allowance{ObservedAt: observedAt, Windows: normalized, Credits: credits, RemainingPercent: percent, ResetsAt: reset}
}

func compareAllowanceWindows(left, right AllowanceWindow) int {
	return strings.Compare(left.LimitID+":"+left.Window+":"+string(left.Evidence), right.LimitID+":"+right.Window+":"+string(right.Evidence))
}

func (a Allowance) Validate() error {
	for _, count := range []struct {
		remaining   *int64
		observation *CountObservation
	}{{a.RemainingTokens, a.TokenObservation}, {a.RemainingRequests, a.RequestObservation}} {
		if o := count.observation; o != nil && (count.remaining == nil || o.ObservedAt.IsZero() || o.ObservedAt.After(a.ObservedAt)) {
			return fmt.Errorf("invalid provider count observation")
		}
	}
	for _, n := range []*int64{a.RemainingTokens, a.RemainingRequests} {
		if n != nil && (*n < 0 || *n > 1<<53-1) {
			return fmt.Errorf("invalid provider allowance")
		}
	}
	if a.ObservedAt.IsZero() || a.RemainingPercent != nil && (math.IsNaN(*a.RemainingPercent) || math.IsInf(*a.RemainingPercent, 0) || *a.RemainingPercent < 0 || *a.RemainingPercent > 100) {
		return fmt.Errorf("invalid provider allowance")
	}
	if len(a.Windows) > MaxAllowanceWindows {
		return fmt.Errorf("too many provider allowance windows")
	}
	seen := map[string]bool{}
	for _, w := range a.Windows {
		normalized := w.Normalized()
		key := normalized.LimitID + ":" + normalized.Window + ":" + string(normalized.Evidence)
		if seen[key] || normalized.ObservedAt.After(a.ObservedAt) {
			return fmt.Errorf("invalid provider allowance observation")
		}
		if err := normalized.Validate(); err != nil {
			return err
		}
		seen[key] = true
	}
	if a.Credits != nil {
		if a.Credits.ObservedAt.After(a.ObservedAt) {
			return fmt.Errorf("invalid provider credits observation")
		}
		return a.Credits.Validate()
	}
	return nil
}

func (a Allowance) Merge(incoming Allowance) Allowance {
	tokens, tokenObservation := mergeAllowanceCount(a.RemainingTokens, allowanceCountObservation(a.RemainingTokens, a.TokenObservation, a), incoming.RemainingTokens, allowanceCountObservation(incoming.RemainingTokens, incoming.TokenObservation, incoming))
	requests, requestObservation := mergeAllowanceCount(a.RemainingRequests, allowanceCountObservation(a.RemainingRequests, a.RequestObservation, a), incoming.RemainingRequests, allowanceCountObservation(incoming.RemainingRequests, incoming.RequestObservation, incoming))
	windows := slices.SortedFunc(mergedAllowanceWindows(allowanceWindows(a), allowanceWindows(incoming)), compareAllowanceWindows)
	observedAt := a.ObservedAt
	if incoming.ObservedAt.After(observedAt) {
		observedAt = incoming.ObservedAt
	}
	credits := a.Credits
	if incoming.Credits != nil && (credits == nil || incoming.Credits.ObservedAt.After(credits.ObservedAt)) {
		credits = incoming.Credits
	}
	percent, reset := primaryPercentMirror(windows, a.RemainingPercent, a.ResetsAt)
	return Allowance{
		RemainingTokens: tokens, RemainingRequests: requests,
		TokenObservation: tokenObservation, RequestObservation: requestObservation,
		Windows: windows, Credits: credits,
		RemainingPercent: percent, ResetsAt: reset, ObservedAt: observedAt,
	}
}

func (a Allowance) WithoutActive() Allowance {
	windows := slices.Collect(func(yield func(AllowanceWindow) bool) {
		for _, w := range a.Windows {
			normalized := w.Normalized()
			if normalized.Evidence != AllowanceEvidenceActive && !yield(normalized) {
				return
			}
		}
	})
	hasActivePrimary := slices.ContainsFunc(a.Windows, func(w AllowanceWindow) bool {
		normalized := w.Normalized()
		return normalized.LimitID == codexLimitID && normalized.Window == allowancePrimary && normalized.Evidence == AllowanceEvidenceActive
	})
	fallbackPercent, fallbackReset := a.RemainingPercent, a.ResetsAt
	if hasActivePrimary {
		fallbackPercent, fallbackReset = nil, nil
	}
	percent, reset := primaryPercentMirror(windows, fallbackPercent, fallbackReset)
	return Allowance{
		Windows: windows, Credits: a.Credits,
		RemainingTokens: a.RemainingTokens, RemainingRequests: a.RemainingRequests,
		TokenObservation: a.TokenObservation, RequestObservation: a.RequestObservation,
		RemainingPercent: percent, ResetsAt: reset, ObservedAt: a.ObservedAt,
	}
}

func primaryPercentMirror(windows []AllowanceWindow, fallbackPercent *float64, fallbackReset *time.Time) (*float64, *time.Time) {
	primary := slices.IndexFunc(windows, func(w AllowanceWindow) bool {
		normalized := w.Normalized()
		return normalized.LimitID == codexLimitID && normalized.Window == allowancePrimary && normalized.Evidence == AllowanceEvidencePassive && normalized.Unit == AllowanceUnitPercent
	})
	if primary >= 0 {
		remaining := windows[primary].RemainingPercent
		return &remaining, windows[primary].ResetsAt
	}
	if slices.ContainsFunc(windows, func(w AllowanceWindow) bool {
		normalized := w.Normalized()
		return normalized.LimitID == codexLimitID && normalized.Window == allowancePrimary && normalized.Evidence == AllowanceEvidenceActive
	}) {
		return nil, nil
	}
	return fallbackPercent, fallbackReset
}

func allowanceWindows(a Allowance) []AllowanceWindow {
	if len(a.Windows) == 0 && a.RemainingPercent != nil {
		return []AllowanceWindow{{
			LimitID: codexLimitID, Window: allowancePrimary, Unit: AllowanceUnitPercent, Evidence: AllowanceEvidencePassive,
			UsedPercent: 100 - *a.RemainingPercent, RemainingPercent: *a.RemainingPercent, ResetsAt: a.ResetsAt, ObservedAt: a.ObservedAt,
		}}
	}
	return slices.Collect(func(yield func(AllowanceWindow) bool) {
		for _, w := range a.Windows {
			if !yield(w.Normalized()) {
				return
			}
		}
	})
}

func mergedAllowanceWindows(current, incoming []AllowanceWindow) iter.Seq[AllowanceWindow] {
	return func(yield func(AllowanceWindow) bool) {
		for _, old := range current {
			index := slices.IndexFunc(incoming, func(w AllowanceWindow) bool {
				return old.LimitID == w.LimitID && old.Window == w.Window && old.Evidence == w.Evidence
			})
			if index < 0 || !incoming[index].ObservedAt.After(old.ObservedAt) {
				if !yield(old) {
					return
				}
				continue
			}
			next := incoming[index]
			next.ResetsAt = mergeAllowanceReset(old.ResetsAt, next.ResetsAt, next.ObservedAt)
			if !yield(next) {
				return
			}
		}
		for _, next := range incoming {
			if slices.ContainsFunc(current, func(old AllowanceWindow) bool {
				return old.LimitID == next.LimitID && old.Window == next.Window && old.Evidence == next.Evidence
			}) {
				continue
			}
			if !yield(next) {
				return
			}
		}
	}
}

func allowanceCountObservation(value *int64, observation *CountObservation, allowance Allowance) *CountObservation {
	if observation != nil || value == nil {
		return observation
	}
	return &CountObservation{ResetsAt: allowance.ResetsAt, ObservedAt: allowance.ObservedAt}
}

func mergeAllowanceCount(current *int64, observation *CountObservation, incoming *int64, incomingObservation *CountObservation) (*int64, *CountObservation) {
	if incoming == nil || observation != nil && !incomingObservation.ObservedAt.After(observation.ObservedAt) {
		return current, observation
	}
	var reset *time.Time
	if observation != nil {
		reset = observation.ResetsAt
	}
	return incoming, &CountObservation{ObservedAt: incomingObservation.ObservedAt, ResetsAt: mergeAllowanceReset(reset, incomingObservation.ResetsAt, incomingObservation.ObservedAt)}
}

func mergeAllowanceReset(current, incoming *time.Time, observedAt time.Time) *time.Time {
	if incoming != nil {
		return incoming
	}
	if current != nil && current.After(observedAt) {
		return current
	}
	return nil
}

type HealthObservation struct {
	Status     Health
	ObservedAt time.Time
	RetryAt    *time.Time
}

func NewHealthyObservation(observedAt time.Time) HealthObservation {
	return HealthObservation{Status: HealthHealthy, ObservedAt: observedAt}
}

func NewDegradedObservation(status Health, observedAt time.Time, retryAt *time.Time) HealthObservation {
	return HealthObservation{Status: status, ObservedAt: observedAt, RetryAt: retryAt}
}

func (o HealthObservation) Validate() error {
	if !slices.Contains([]Health{HealthHealthy, HealthUnavailable, HealthQuotaLimited}, o.Status) || o.ObservedAt.IsZero() ||
		o.RetryAt != nil && (o.Status == HealthHealthy || o.RetryAt.Year() < 1 || o.RetryAt.Year() > 9999) {
		return Refuse(400, "code_health_invalid")
	}
	return nil
}

type Observation struct {
	Terminal        bool
	Successful      bool
	Outcome         *Outcome
	Usage           Usage
	ResponseID      string
	Status          int
	RetryAfter      []string
	Allowance       *Allowance
	RejectionBody   []byte
	AllowanceCharge json.RawMessage
}
