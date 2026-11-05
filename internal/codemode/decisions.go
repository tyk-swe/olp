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
	SkipBand        SkipClass = "band"
	SkipWeight      SkipClass = "weight"
	SkipTree        SkipClass = "tree"
	SkipUnavailable SkipClass = "unavailable"
)

type Candidate struct {
	AccountID string
	Available bool
	Cooldown  bool
	Position  int
	Score     float64
}

func ChooseCandidate(candidates []Candidate) (Candidate, bool) {
	if len(candidates) == 0 {
		return Candidate{}, false
	}
	return slices.MaxFunc(candidates, func(left, right Candidate) int {
		if left.Position != right.Position {
			return cmp.Compare(right.Position, left.Position)
		}
		if left.Available != right.Available {
			if left.Available {
				return 1
			}
			return -1
		}
		if left.Score != right.Score {
			return cmp.Compare(left.Score, right.Score)
		}
		return cmp.Compare(right.AccountID, left.AccountID)
	}), true
}

type SkippedAccount struct {
	AccountID string    `json:"account_id"`
	Class     SkipClass `json:"class"`
}

func SkippedAccounts(candidates []Candidate, chosen Candidate) []SkippedAccount {
	skipped := make([]SkippedAccount, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.AccountID == chosen.AccountID {
			continue
		}
		class := SkipWeight
		switch {
		case !candidate.Available && candidate.Cooldown:
			class = SkipCooldown
		case !candidate.Available:
			class = SkipUnavailable
		case candidate.Position > chosen.Position:
			class = SkipTree
		}
		skipped = append(skipped, SkippedAccount{AccountID: candidate.AccountID, Class: class})
	}
	slices.SortFunc(skipped, func(left, right SkippedAccount) int {
		return cmp.Compare(left.AccountID, right.AccountID)
	})
	return skipped
}

func FreshestEvidence(allowance *Allowance) (*AllowanceEvidence, *time.Time) {
	if allowance == nil {
		return nil, nil
	}
	windows := allowanceWindows(*allowance)
	if len(windows) == 0 {
		return nil, nil
	}
	freshest := slices.MaxFunc(windows, func(left, right AllowanceWindow) int {
		return left.ObservedAt.Compare(right.ObservedAt)
	})
	evidence := freshest.EffectiveEvidence()
	return &evidence, &freshest.ObservedAt
}

type Decision struct {
	ID                 string             `json:"id"`
	AttemptID          *string            `json:"attempt_id"`
	ProjectID          string             `json:"project_id"`
	RouteID            string             `json:"route_id"`
	RouteRevisionID    string             `json:"route_revision_id"`
	APIKeyID           string             `json:"api_key_id"`
	BindingID          string             `json:"binding_id"`
	RootID             string             `json:"root_id"`
	ChosenAccountID    string             `json:"chosen_account_id"`
	Reason             DecisionReason     `json:"reason"`
	Band               *string            `json:"band"`
	Evidence           *AllowanceEvidence `json:"evidence"`
	EvidenceObservedAt *time.Time         `json:"evidence_observed_at"`
	Skipped            []SkippedAccount   `json:"skipped"`
	RecoveryEnvelope   *string            `json:"recovery_envelope"`
	RecoveryOrdinal    *int               `json:"recovery_ordinal"`
	CreatedAt          time.Time          `json:"created_at"`
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
	if d.Band != nil && !slices.Contains([]string{"healthy", "unknown", "low", "exhausted"}, *d.Band) {
		return fmt.Errorf("invalid code decision band")
	}
	if (d.Evidence == nil) != (d.EvidenceObservedAt == nil) ||
		d.Evidence != nil && !slices.Contains([]AllowanceEvidence{AllowanceEvidencePassive, AllowanceEvidenceActive}, *d.Evidence) {
		return fmt.Errorf("invalid code decision evidence")
	}
	seen := map[string]bool{d.ChosenAccountID: true}
	for _, skipped := range d.Skipped {
		if skipped.AccountID == "" || seen[skipped.AccountID] ||
			!slices.Contains([]SkipClass{SkipCooldown, SkipBand, SkipWeight, SkipTree, SkipUnavailable}, skipped.Class) {
			return fmt.Errorf("invalid code decision skipped account")
		}
		seen[skipped.AccountID] = true
	}
	if d.RecoveryOrdinal != nil && (*d.RecoveryOrdinal < 1 || *d.RecoveryOrdinal > math.MaxInt32) {
		return fmt.Errorf("invalid code decision recovery ordinal")
	}
	if d.RecoveryEnvelope != nil && !recoveryEnvelopePattern.MatchString(*d.RecoveryEnvelope) {
		return fmt.Errorf("invalid code decision recovery envelope")
	}
	return nil
}
