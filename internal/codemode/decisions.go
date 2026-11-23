package codemode

import (
	"cmp"
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"
)

var recoveryEnvelopePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,99}$`)

type DecisionReason string

const (
	DecisionChosen     DecisionReason = "chosen"
	DecisionPin        DecisionReason = "pin"
	DecisionConnection DecisionReason = "connection"
)

type SkipClass string

const (
	SkipCooldown    SkipClass = "cooldown"
	SkipWeight      SkipClass = "weight"
	SkipTree        SkipClass = "tree"
	SkipUnavailable SkipClass = "unavailable"
	SkipBusy        SkipClass = "busy"
)

type Candidate struct {
	AccountID string
	Available bool
	Cooldown  bool
	Busy      bool
	Position  int
	Score     float64
}

func (c Candidate) Eligible() bool {
	return c.Available && !c.Busy
}

func candidatePreferred(left, right Candidate) int {
	if left.Position != right.Position {
		return cmp.Compare(right.Position, left.Position)
	}
	if left.Eligible() != right.Eligible() {
		if left.Eligible() {
			return 1
		}
		return -1
	}
	if left.Score != right.Score {
		return cmp.Compare(left.Score, right.Score)
	}
	return cmp.Compare(right.AccountID, left.AccountID)
}

func ChooseCandidate(candidates []Candidate) (Candidate, bool) {
	if len(candidates) == 0 {
		return Candidate{}, false
	}
	return slices.MaxFunc(candidates, candidatePreferred), true
}

type SkippedAccount struct {
	AccountID string    `json:"account_id"`
	Class     SkipClass `json:"class"`
}

func SkippedAccounts(candidates []Candidate, chosen Candidate) []SkippedAccount {
	skipped := slices.SortedFunc(func(yield func(SkippedAccount) bool) {
		for _, candidate := range candidates {
			if candidate.AccountID == chosen.AccountID {
				continue
			}
			class := func() SkipClass {
				switch {
				case candidate.Busy:
					return SkipBusy
				case !candidate.Available && candidate.Cooldown:
					return SkipCooldown
				case !candidate.Eligible():
					return SkipUnavailable
				case candidate.Position > chosen.Position:
					return SkipTree
				default:
					return SkipWeight
				}
			}()
			if !yield(SkippedAccount{AccountID: candidate.AccountID, Class: class}) {
				return
			}
		}
	}, func(left, right SkippedAccount) int {
		return cmp.Compare(left.AccountID, right.AccountID)
	})
	if skipped == nil {
		return []SkippedAccount{}
	}
	return skipped
}

type Decision struct {
	ID               string           `json:"id"`
	AttemptID        *string          `json:"attempt_id"`
	ProjectID        string           `json:"project_id"`
	RouteID          string           `json:"route_id"`
	RouteRevisionID  string           `json:"route_revision_id"`
	APIKeyID         string           `json:"api_key_id"`
	BindingID        string           `json:"binding_id"`
	RootID           string           `json:"root_id"`
	ChosenAccountID  string           `json:"chosen_account_id"`
	Reason           DecisionReason   `json:"reason"`
	Skipped          []SkippedAccount `json:"skipped"`
	RecoveryEnvelope *string          `json:"recovery_envelope"`
	RecoveryOrdinal  *int             `json:"recovery_ordinal"`
	CreatedAt        time.Time        `json:"created_at"`
}

func (d Decision) Validate() error {
	if d.ChosenAccountID == "" {
		return fmt.Errorf("invalid code decision")
	}
	if !slices.Contains([]DecisionReason{DecisionChosen, DecisionPin, DecisionConnection}, d.Reason) {
		return fmt.Errorf("invalid code decision reason")
	}
	if d.Reason == DecisionConnection {
		if d.AttemptID != nil {
			return fmt.Errorf("invalid code decision attempt")
		}
	} else if d.AttemptID == nil || *d.AttemptID == "" {
		return fmt.Errorf("invalid code decision attempt")
	}
	skippedAccounts := slices.SortedFunc(slices.Values(d.Skipped), func(left, right SkippedAccount) int {
		return cmp.Compare(left.AccountID, right.AccountID)
	})
	for index, skipped := range skippedAccounts {
		if skipped.AccountID == "" || skipped.AccountID == d.ChosenAccountID ||
			index > 0 && skippedAccounts[index-1].AccountID == skipped.AccountID ||
			!slices.Contains([]SkipClass{SkipCooldown, SkipWeight, SkipTree, SkipUnavailable, SkipBusy}, skipped.Class) {
			return fmt.Errorf("invalid code decision skipped account")
		}
	}
	if d.RecoveryOrdinal != nil && (*d.RecoveryOrdinal < 1 || *d.RecoveryOrdinal > math.MaxInt32) {
		return fmt.Errorf("invalid code decision recovery ordinal")
	}
	if d.RecoveryEnvelope != nil && !recoveryEnvelopePattern.MatchString(*d.RecoveryEnvelope) {
		return fmt.Errorf("invalid code decision recovery envelope")
	}
	return nil
}
