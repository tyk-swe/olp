package debits

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/pricingpolicy"
	"github.com/tyk-swe/olp/internal/toolidentity"
)

var (
	ErrConflict  = errors.New("execution debit identity or evidence conflicts")
	ErrState     = errors.New("execution debit transition refused")
	ErrAuthority = errors.New("execution debit authority unavailable")
)

type Affinity struct {
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

type Kind string

const (
	KindAPI   Kind = "api"
	KindModel Kind = "model"
	KindTool  Kind = "tool"
	KindAgent Kind = "agent"
)

func (k Kind) valid() bool {
	return k == KindAPI || k == KindModel || k == KindTool || k == KindAgent
}

func (k Kind) priced() bool { return k == KindTool || k == KindAgent }

type Intent struct {
	ID            string    `json:"id"`
	APIKeyID      string    `json:"api_key_id"`
	ProjectID     string    `json:"project_id"`
	RunID         string    `json:"run_id,omitempty"`
	StepID        string    `json:"step_id,omitempty"`
	Kind          Kind      `json:"kind"`
	ClaimID       string    `json:"-"`
	Affinity      Affinity  `json:"affinity"`
	Currency      string    `json:"currency"`
	CostBound     string    `json:"cost_bound"`
	ChargeBound   string    `json:"charge_bound"`
	PreparedUntil time.Time `json:"prepared_until"`
}

type State string

const (
	Prepared    State = "prepared"
	Ready       State = "ready"
	Dispatched  State = "dispatched"
	Unknown     State = "outcome_unknown"
	Settled     State = "settled"
	Released    State = "released"
	NotAccepted State = "not_accepted"
	Conflict    State = "conflict"
)

func canonicalID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}

func optionalID(id string) bool { return id == "" || canonicalID(id) }

func digestText(digest string) bool {
	if digest == "" {
		return true
	}
	value, err := hex.DecodeString(digest)
	return err == nil && len(value) == 32 && strings.ToLower(digest) == digest
}

func (a Affinity) valid() bool {
	optional := []string{a.RouteRevisionID, a.ProviderID, a.ProviderRevisionID, a.TargetID, a.SlotID, a.CredentialID, a.PricingRevisionID, a.RateCardRevisionID}
	return canonicalID(a.ReleaseID) && canonicalID(a.RevisionID) && !slices.ContainsFunc(optional, invalidOptionalID) &&
		digestText(a.InputDigest) && digestText(a.TranscriptDigest) &&
		(a.ToolAlias == "" || toolidentity.ValidAlias(a.ToolAlias))
}

func invalidOptionalID(id string) bool { return !optionalID(id) }

func (i Intent) validate() error {
	identified := canonicalID(i.ID) && canonicalID(i.APIKeyID) && canonicalID(i.ClaimID) &&
		optionalID(i.ProjectID) && optionalID(i.RunID) && optionalID(i.StepID) &&
		(!i.Kind.priced() || i.ProjectID != "") && (i.RunID == "") == (i.StepID == "")
	bounded := i.Kind.valid() && pricingpolicy.ValidCurrency(i.Currency) &&
		validAmount(i.CostBound) && validAmount(i.ChargeBound) && !i.PreparedUntil.IsZero()
	if !identified || !bounded || !i.Affinity.valid() {
		return ErrConflict
	}
	return nil
}

func validAmount(raw string) bool {
	if !limits.ValidDecimal(raw) {
		return false
	}
	whole, _, _ := strings.Cut(raw, ".")
	return len(strings.TrimLeft(whole, "0")) <= 16
}

type identity struct {
	Intent Intent
	Owners []limits.Owner
}

func intentDigest(i Intent, owners []limits.Owner) ([32]byte, error) {
	data, err := json.Marshal(identity{i, owners})
	if err != nil {
		return [32]byte{}, errors.Join(ErrConflict, err)
	}
	return sha256.Sum256(data), nil
}

func normalizedOwners(owners []limits.Owner) ([]limits.Owner, error) {
	if len(owners) == 0 || len(owners) > 128 {
		return nil, ErrConflict
	}
	kinds := make(map[string]limits.OwnerKind, len(owners))
	seen := make(map[limits.Owner]bool, len(owners))
	for _, o := range owners {
		pair := limits.Owner{ID: o.ID, Basis: o.Basis.Normalized()}
		kind, known := kinds[o.ID]
		if !canonicalID(o.ID) || !o.Basis.Valid() || !o.Kind.Valid() || known && kind != o.Kind || seen[pair] {
			return nil, ErrConflict
		}
		kinds[o.ID], seen[pair] = o.Kind, true
	}
	ids := make([]string, 0, len(kinds))
	for id := range kinds {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]limits.Owner, 0, len(ids)*2)
	for _, id := range ids {
		out = append(out, limits.Owner{ID: id, Kind: kinds[id], Basis: limits.BasisCharge}, limits.Owner{ID: id, Kind: kinds[id], Basis: limits.BasisCost})
	}
	return out, nil
}
