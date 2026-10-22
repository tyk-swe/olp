package limits

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrDebitConflict  = errors.New("execution debit identity or evidence conflicts")
	ErrDebitState     = errors.New("execution debit transition refused")
	ErrDebitAuthority = errors.New("execution debit authority unavailable")
)

// ExecutionAffinity contains reviewed identities only, never arguments, model
// input, credentials or provider responses. The execution owner validates the
// referenced published revisions before preparing and again before dispatch.
type ExecutionAffinity struct {
	ToolAlias          string `json:"tool_alias,omitempty"`
	InputDigest        string `json:"input_digest,omitempty"`
	TranscriptDigest   string `json:"transcript_digest,omitempty"`
	ReleaseID          string `json:"release_id"`
	RevisionID         string `json:"revision_id"`
	RouteRevisionID    string `json:"route_revision_id,omitempty"`
	ProviderID         string `json:"provider_id,omitempty"`
	ProviderRevisionID string `json:"provider_revision_id,omitempty"`
	TargetID           string `json:"target_id,omitempty"`
	SlotID             string `json:"slot_id,omitempty"`
	CredentialID       string `json:"credential_id,omitempty"`
	PricingRevisionID  string `json:"pricing_revision_id,omitempty"`
	RateCardRevisionID string `json:"rate_card_revision_id,omitempty"`
}

// DebitIntent is a finite, immutable pre-dispatch promise. ClaimID is a private
// fencing capability held only by the preparing process, never a retry token.
type DebitIntent struct {
	ID            string            `json:"id"`
	APIKeyID      string            `json:"api_key_id"`
	ProjectID     string            `json:"project_id"`
	RunID         string            `json:"run_id,omitempty"`
	StepID        string            `json:"step_id,omitempty"`
	Kind          string            `json:"kind"`
	ClaimID       string            `json:"-"`
	Affinity      ExecutionAffinity `json:"affinity"`
	Currency      string            `json:"currency"`
	CostBound     string            `json:"cost_bound"`
	ChargeBound   string            `json:"charge_bound"`
	PreparedUntil time.Time         `json:"prepared_until"`
}

// DebitOwner is an existing accounting boundary resolved by the execution
// owner from fresh authority. Record uncapped applicable owners too, so a cap
// added after acceptance observes the same exposure. It is not a new account.
type DebitOwner struct {
	ID    string
	Kind  string
	Basis Basis
}

type DebitState string

const (
	DebitPrepared    DebitState = "prepared"
	DebitReady       DebitState = "ready"
	DebitDispatched  DebitState = "dispatched"
	DebitUnknown     DebitState = "outcome_unknown"
	DebitSettled     DebitState = "settled"
	DebitReleased    DebitState = "released"
	DebitNotAccepted DebitState = "not_accepted"
	DebitConflict    DebitState = "conflict"
)

func canonicalID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}

func (i DebitIntent) validate() error {
	if !canonicalID(i.ID) || !canonicalID(i.APIKeyID) || (i.ProjectID != "" && !canonicalID(i.ProjectID)) || ((i.Kind == "tool" || i.Kind == "agent") && i.ProjectID == "") || !canonicalID(i.ClaimID) || !canonicalID(i.Affinity.ReleaseID) || !canonicalID(i.Affinity.RevisionID) || (i.RunID == "") != (i.StepID == "") {
		return ErrDebitConflict
	}
	for _, id := range []string{i.RunID, i.StepID, i.Affinity.RouteRevisionID, i.Affinity.ProviderID, i.Affinity.ProviderRevisionID, i.Affinity.TargetID, i.Affinity.SlotID, i.Affinity.CredentialID, i.Affinity.PricingRevisionID, i.Affinity.RateCardRevisionID} {
		if id != "" && !canonicalID(id) {
			return ErrDebitConflict
		}
	}
	if !slices.Contains([]string{"api", "model", "tool", "agent"}, i.Kind) || !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(i.Currency) || !validDebitAmount(i.CostBound) || !validDebitAmount(i.ChargeBound) || i.PreparedUntil.IsZero() {
		return ErrDebitConflict
	}
	for _, digest := range []string{i.Affinity.InputDigest, i.Affinity.TranscriptDigest} {
		if digest != "" {
			value, err := hex.DecodeString(digest)
			if err != nil || len(value) != 32 || strings.ToLower(digest) != digest {
				return ErrDebitConflict
			}
		}
	}
	if i.Affinity.ToolAlias != "" && !regexp.MustCompile(`^t_[a-f0-9]{32}_[a-f0-9]{32}$`).MatchString(i.Affinity.ToolAlias) {
		return ErrDebitConflict
	}
	return nil
}

func validDebitAmount(raw string) bool {
	if !validDecimal(raw) {
		return false
	}
	whole, _, _ := strings.Cut(raw, ".")
	return len(strings.TrimLeft(whole, "0")) <= 16
}

func debitDigest(i DebitIntent, owners []DebitOwner) [32]byte {
	// ClaimID is a fence, not logical identity. A repeated caller ID can find the
	// same intent but cannot acquire another process's right to dispatch it.
	data, _ := json.Marshal(struct {
		Intent DebitIntent
		Owners []DebitOwner
	}{i, owners})
	return sha256.Sum256(data)
}

// Both bases are recorded even when only one is currently capped. A later
// policy basis change must not make accepted work disappear from admission.
func normalizedDebitOwners(owners []DebitOwner) ([]DebitOwner, error) {
	if len(owners) == 0 || len(owners) > 128 {
		return nil, ErrDebitConflict
	}
	kinds := map[string]string{}
	seen := map[string]bool{}
	for _, o := range owners {
		if !canonicalID(o.ID) || !o.Basis.valid() || !slices.Contains([]string{"key", "group", "aggregate", "end_user", "supply"}, o.Kind) {
			return nil, ErrDebitConflict
		}
		if kind, ok := kinds[o.ID]; ok && kind != o.Kind {
			return nil, ErrDebitConflict
		}
		kinds[o.ID] = o.Kind
		key := o.ID + ":" + string(o.Basis.normalized())
		if seen[key] {
			return nil, ErrDebitConflict
		}
		seen[key] = true
	}
	ids := make([]string, 0, len(kinds))
	for id := range kinds {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]DebitOwner, 0, len(ids)*2)
	for _, id := range ids {
		for _, basis := range []Basis{BasisCharge, BasisCost} {
			out = append(out, DebitOwner{ID: id, Kind: kinds[id], Basis: basis})
		}
	}
	return out, nil
}
