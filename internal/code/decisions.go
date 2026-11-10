package code

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
	Band      Band
	ResetAt   *time.Time
}

func (c Candidate) Eligible() bool {
	return c.Available && c.Band != BandExhausted
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
	if leftRank, rightRank := bandRank(left.Band), bandRank(right.Band); leftRank != rightRank {
		return cmp.Compare(leftRank, rightRank)
	}
	if left.Score != right.Score {
		return cmp.Compare(left.Score, right.Score)
	}
	if left.Band == BandLow && right.Band == BandLow {
		switch {
		case left.ResetAt == nil && right.ResetAt != nil:
			return -1
		case left.ResetAt != nil && right.ResetAt == nil:
			return 1
		case left.ResetAt != nil && right.ResetAt != nil:
			if compared := right.ResetAt.Compare(*left.ResetAt); compared != 0 {
				return compared
			}
		}
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
				case !candidate.Available && candidate.Cooldown:
					return SkipCooldown
				case !candidate.Eligible():
					return SkipUnavailable
				case candidate.Position > chosen.Position:
					return SkipTree
				case bandRank(candidate.Band) < bandRank(chosen.Band):
					return SkipBand
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

type DecisionScope struct {
	ID              string
	ProjectID       string
	RouteID         string
	RouteRevisionID string
	APIKeyID        string
	BindingID       string
	RootID          string
	ChosenAccountID string
	Skipped         []SkippedAccount
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
	Band               *Band              `json:"band"`
	Evidence           *AllowanceEvidence `json:"evidence"`
	EvidenceObservedAt *time.Time         `json:"evidence_observed_at"`
	Skipped            []SkippedAccount   `json:"skipped"`
	RecoveryEnvelope   *string            `json:"recovery_envelope"`
	RecoveryOrdinal    *int               `json:"recovery_ordinal"`
	CreatedAt          time.Time          `json:"created_at"`
}

func NewAttemptDecision(scope DecisionScope, attemptID string, reason DecisionReason, assessment *BandAssessment, recovery *Recovery) (Decision, error) {
	if reason != DecisionChosen && reason != DecisionPin {
		return Decision{}, fmt.Errorf("invalid attempt decision reason")
	}
	var band *Band
	var evidence *AllowanceEvidence
	var observedAt *time.Time
	if reason == DecisionChosen && assessment != nil {
		value := assessment.Band
		band, evidence, observedAt = &value, assessment.Evidence, assessment.ObservedAt
	}
	var envelope *string
	var ordinal *int
	if recovery != nil {
		ordinal = &recovery.Number
		if recovery.Envelope != "" {
			envelope = &recovery.Envelope
		}
	}
	d := Decision{
		ID: scope.ID, AttemptID: &attemptID, ProjectID: scope.ProjectID, RouteID: scope.RouteID,
		RouteRevisionID: scope.RouteRevisionID, APIKeyID: scope.APIKeyID, BindingID: scope.BindingID,
		RootID: scope.RootID, ChosenAccountID: scope.ChosenAccountID, Reason: reason,
		Band: band, Evidence: evidence, EvidenceObservedAt: observedAt, Skipped: scope.Skipped,
		RecoveryEnvelope: envelope, RecoveryOrdinal: ordinal,
	}
	return d, d.Validate()
}

func NewConnectionDecision(scope DecisionScope) (Decision, error) {
	d := Decision{
		ID: scope.ID, ProjectID: scope.ProjectID, RouteID: scope.RouteID,
		RouteRevisionID: scope.RouteRevisionID, APIKeyID: scope.APIKeyID, BindingID: scope.BindingID,
		RootID: scope.RootID, ChosenAccountID: scope.ChosenAccountID, Reason: DecisionConnection,
		Skipped: scope.Skipped,
	}
	return d, d.Validate()
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
	if d.Band != nil && !slices.Contains([]Band{BandHealthy, BandUnknown, BandLow, BandExhausted}, *d.Band) {
		return fmt.Errorf("invalid code decision band")
	}
	if (d.Evidence == nil) != (d.EvidenceObservedAt == nil) ||
		d.Evidence != nil && !slices.Contains([]AllowanceEvidence{AllowanceEvidencePassive, AllowanceEvidenceActive}, *d.Evidence) {
		return fmt.Errorf("invalid code decision evidence")
	}
	skippedAccounts := slices.SortedFunc(slices.Values(d.Skipped), func(left, right SkippedAccount) int {
		return cmp.Compare(left.AccountID, right.AccountID)
	})
	for index, skipped := range skippedAccounts {
		if skipped.AccountID == "" || skipped.AccountID == d.ChosenAccountID ||
			index > 0 && skippedAccounts[index-1].AccountID == skipped.AccountID ||
			!slices.Contains([]SkipClass{SkipCooldown, SkipBand, SkipWeight, SkipTree, SkipUnavailable}, skipped.Class) {
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
