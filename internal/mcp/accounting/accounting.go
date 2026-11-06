package accounting

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/guardrails"
)

var ErrUnpricedToolCall = errors.New("tool call has no price for an enforced budget ceiling")

const (
	StatusInProgress    Status = "in_progress"
	OrphanDispatchAge          = 2 * time.Minute
	callIdentityBinding        = 24 * time.Hour
)

type PricingState string

const (
	PricingPriced   PricingState = "priced"
	PricingFree     PricingState = "free"
	PricingUnpriced PricingState = "unpriced"
)

func PricingStateFor(price *string) PricingState {
	if price == nil {
		return PricingUnpriced
	}
	if resolveAmountIsZero(*price) {
		return PricingFree
	}
	return PricingPriced
}

type Status string

const (
	StatusDispatched       Status = "dispatched"
	StatusSucceeded        Status = "succeeded"
	StatusUpstreamError    Status = "upstream_error"
	StatusOutcomeUnknown   Status = "outcome_unknown"
	StatusSettled          Status = "settled"
	StatusNotAccepted      Status = "not_accepted"
	StatusRefusedUnpriced  Status = "refused_unpriced"
	StatusGuardrailBlocked Status = "guardrail_blocked"
)

type ReserveInput struct {
	Authority         keyauthority.Authority
	Approval          *runs.WorkflowApproval
	RunID             string
	Ordinal           int
	ToolsetID         string
	ToolsetRevisionID string
	ReleaseID         string
	ToolAlias         string
	InputDigest       string
	CallDigest        string
	UnitPrice         *string
	ServerID          string
	ServerRevisionID  string
}

type Reservation struct {
	Approval             *runs.WorkflowApproval
	Authority            keyauthority.Authority
	DispatchID           string
	ClaimID              string
	RunID                string
	StepID               string
	AttemptID            string
	APIKeyID             string
	ProjectID            string
	ToolsetID            string
	ToolsetRevisionID    string
	ToolAlias            string
	InputDigest          string
	CallDigest           string
	AgentPrincipalID     string
	MemberPrincipalID    string
	DelegationPairDigest string
	UnitPrice            *string
	Currency             string
}

func (r *Reservation) Amount() string {
	if r == nil || r.UnitPrice == nil {
		return "0"
	}
	return *r.UnitPrice
}

type AttemptEvidence struct {
	IdentityDecisions  []IdentityDecision
	ServerID           string
	ServerRevisionID   string
	Status             Status
	IsError            bool
	ResultDigest       string
	StartedAt          time.Time
	CompletedAt        time.Time
	GuardrailDecisions []guardrails.Evidence
}

type Ledger interface {
	Reserve(ctx context.Context, in ReserveInput) (ReserveResult, error)
	MarkDispatched(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error
	ReleaseUndispatched(ctx context.Context, reservation *Reservation) error
	RefuseUndispatched(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error
	MarkUnknown(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error
	Settle(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error
}

type IdentityHook string
type IdentityOutcome string

const (
	RegisteredServerHook  IdentityHook    = "mcp_security"
	UserPermissionHook    IdentityHook    = "mcp_end_user_permission"
	DelegatedSigningHook  IdentityHook    = "mcp_jwt_signer"
	EntraOBOHook          IdentityHook    = "agent_365"
	IdentityPassed        IdentityOutcome = "passed"
	IdentityDenied        IdentityOutcome = "denied"
	IdentityNotConfigured IdentityOutcome = "not_configured"
)

type IdentityDecision struct {
	Hook    IdentityHook    `json:"hook"`
	Outcome IdentityOutcome `json:"outcome"`
}

func identityEvidence(decisions []IdentityDecision) []IdentityDecision {
	if decisions == nil {
		return []IdentityDecision{}
	}
	return decisions
}

type RetainedCall struct {
	AttemptID  string
	DispatchID string
	Status     Status
	Conflict   bool
}

type ReserveResult struct {
	Reservation *Reservation
	Repeat      *RetainedCall
}

func parseEvidenceDigest(digest string) ([]byte, bool) {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != digest {
		return nil, false
	}
	return decoded, true
}

func cloneUnitPrice(price *string) *string {
	if price == nil {
		return nil
	}
	return new(*price)
}
func canonicalUUID(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}
