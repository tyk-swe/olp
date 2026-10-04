package codemode

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

const MaxAllowanceWindows = 128

var limitID = regexp.MustCompile(`^[a-z0-9][a-z0-9_.]{0,99}$`)
var creditBalance = regexp.MustCompile(`^-?[0-9]{1,20}(\.[0-9]{1,12})?$`)

type CountObservation struct {
	ResetsAt   *time.Time `json:"resets_at"`
	ObservedAt time.Time  `json:"observed_at"`
}

type AllowanceWindow struct {
	LimitID          string     `json:"limit_id"`
	Window           string     `json:"window"`
	UsedPercent      float64    `json:"used_percent"`
	RemainingPercent float64    `json:"remaining_percent"`
	WindowMinutes    *int64     `json:"window_minutes"`
	ResetsAt         *time.Time `json:"resets_at"`
	ObservedAt       time.Time  `json:"observed_at"`
}

type Credits struct {
	HasCredits bool      `json:"has_credits"`
	Unlimited  bool      `json:"unlimited"`
	Balance    *string   `json:"balance"`
	ObservedAt time.Time `json:"observed_at"`
}

func (w AllowanceWindow) Validate() error {
	if !limitID.MatchString(w.LimitID) || w.Window != "primary" && w.Window != "secondary" ||
		math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) || w.UsedPercent < 0 || w.UsedPercent > 1<<53-1 ||
		w.RemainingPercent != max(0, 100-w.UsedPercent) || w.ObservedAt.IsZero() ||
		w.WindowMinutes != nil && (*w.WindowMinutes < 0 || *w.WindowMinutes > 1<<53-1) ||
		w.ResetsAt != nil && (w.ResetsAt.Unix() <= 0 || w.ResetsAt.Unix() >= 253402300800) {
		return fmt.Errorf("invalid provider allowance window")
	}
	return nil
}

func (c Credits) Validate() error {
	if c.ObservedAt.IsZero() || c.Balance != nil && !creditBalance.MatchString(*c.Balance) {
		return fmt.Errorf("invalid provider credits")
	}
	return nil
}

// Normalize orders windows by limit and window, and mirrors the Codex
// primary window into the top-level remaining percentage and reset time.
func (a *Allowance) Normalize() {
	slices.SortFunc(a.Windows, func(x, y AllowanceWindow) int {
		return strings.Compare(x.LimitID+":"+x.Window, y.LimitID+":"+y.Window)
	})
	for _, w := range a.Windows {
		if w.LimitID == "codex" && w.Window == "primary" {
			remaining := w.RemainingPercent
			a.RemainingPercent, a.ResetsAt = &remaining, w.ResetsAt
		}
	}
}

func (a Allowance) validateObservations() error {
	if len(a.Windows) > MaxAllowanceWindows {
		return fmt.Errorf("too many provider allowance windows")
	}
	seen := map[string]bool{}
	for _, w := range a.Windows {
		key := w.LimitID + ":" + w.Window
		if seen[key] || w.ObservedAt.After(a.ObservedAt) {
			return fmt.Errorf("invalid provider allowance observation")
		}
		if err := w.Validate(); err != nil {
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

// Outcome is bounded diagnostic metadata, independent of token settlement.
type Outcome struct {
	Origin         string
	Kind           string
	UpstreamStatus *int
	ObservedAt     time.Time
}

func (o Outcome) Validate() error {
	if o.ObservedAt.IsZero() || o.UpstreamStatus != nil && (*o.UpstreamStatus < 100 || *o.UpstreamStatus > 599) {
		return fmt.Errorf("invalid code outcome")
	}
	switch o.Origin {
	case "upstream":
		switch o.Kind {
		case "headers":
			if o.UpstreamStatus == nil {
				return fmt.Errorf("missing upstream status")
			}
		case "completed", "incomplete", "failed", "rejected":
		default:
			return fmt.Errorf("invalid upstream outcome")
		}
		if o.Kind == "completed" && o.UpstreamStatus != nil && (*o.UpstreamStatus < 200 || *o.UpstreamStatus > 299) ||
			o.Kind == "rejected" && (o.UpstreamStatus == nil || *o.UpstreamStatus < 400) {
			return fmt.Errorf("invalid upstream outcome status")
		}
	case "gateway":
		if o.UpstreamStatus != nil || o.Kind != "interrupted" && o.Kind != "transport_error" && o.Kind != "rejected" {
			return fmt.Errorf("invalid gateway outcome")
		}
	case "client":
		if o.UpstreamStatus != nil || o.Kind != "canceled" {
			return fmt.Errorf("invalid client outcome")
		}
	default:
		return fmt.Errorf("invalid code outcome origin")
	}
	return nil
}
