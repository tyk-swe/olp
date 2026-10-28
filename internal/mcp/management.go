package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
)

type Management struct {
	Access *access.Server
}

type ToolCall struct {
	ID                 string                `json:"id"`
	DispatchID         string                `json:"dispatch_id"`
	APIKeyID           string                `json:"api_key_id"`
	ProjectID          string                `json:"project_id"`
	ToolsetID          string                `json:"toolset_id"`
	ToolsetRevisionID  string                `json:"toolset_revision_id"`
	ServerID           string                `json:"server_id"`
	ServerRevisionID   string                `json:"server_revision_id"`
	ToolAlias          string                `json:"tool_alias"`
	InputDigest        string                `json:"input_digest"`
	ResultDigest       *string               `json:"result_digest"`
	Status             Status                `json:"status"`
	IsError            bool                  `json:"is_error"`
	StartedAt          time.Time             `json:"started_at"`
	CompletedAt        *time.Time            `json:"completed_at"`
	CreatedAt          time.Time             `json:"created_at"`
	CallDigest         *string               `json:"call_digest"`
	DebitState         limits.DebitState     `json:"debit_state"`
	CostBound          string                `json:"cost_bound"`
	ChargeBound        string                `json:"charge_bound"`
	ObservedCost       string                `json:"observed_cost"`
	ObservedCharge     string                `json:"observed_charge"`
	RemainingCost      string                `json:"remaining_cost"`
	RemainingCharge    string                `json:"remaining_charge"`
	UnitPrice          *string               `json:"unit_price"`
	PricingState       PricingState          `json:"pricing_state"`
	Currency           *string               `json:"currency"`
	GuardrailDecisions []guardrails.Evidence `json:"guardrail_decisions"`
}

const toolCallColumns = "c.id::text,c.dispatch_id::text,c.api_key_id::text,c.project_id::text,c.toolset_id::text,c.toolset_revision_id::text,c.server_id::text,c.server_revision_id::text,c.tool_alias,c.input_digest,c.result_digest,c.status,c.is_error,c.started_at,c.completed_at,c.created_at,c.call_digest,d.state,d.cost_bound::text,d.charge_bound::text,d.observed_cost::text,d.observed_charge::text,(CASE WHEN d.closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,d.cost_bound-d.observed_cost) END)::text,(CASE WHEN d.closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,d.charge_bound-d.observed_charge) END)::text,c.unit_price::text,c.pricing_state,c.currency,c.guardrail_decisions"

const toolCallFrom = " FROM olp.mcp_tool_calls c JOIN olp.execution_debits d ON d.id=c.dispatch_id"

type toolCallRow interface {
	Scan(dest ...any) error
}

func scanToolCall(row toolCallRow) (ToolCall, error) {
	var call ToolCall
	err := row.Scan(&call.ID, &call.DispatchID, &call.APIKeyID, &call.ProjectID, &call.ToolsetID, &call.ToolsetRevisionID, &call.ServerID, &call.ServerRevisionID, &call.ToolAlias, &call.InputDigest, &call.ResultDigest, &call.Status, &call.IsError, &call.StartedAt, &call.CompletedAt, &call.CreatedAt, &call.CallDigest, &call.DebitState, &call.CostBound, &call.ChargeBound, &call.ObservedCost, &call.ObservedCharge, &call.RemainingCost, &call.RemainingCharge, &call.UnitPrice, &call.PricingState, &call.Currency, &call.GuardrailDecisions)
	if call.GuardrailDecisions == nil {
		call.GuardrailDecisions = []guardrails.Evidence{}
	}
	return call, err
}

func loadToolCall(ctx context.Context, q access.Queryer, id string) (ToolCall, error) {
	return scanToolCall(q.QueryRow(ctx, "SELECT "+toolCallColumns+toolCallFrom+" WHERE c.id=$1", id))
}

func validateToolCallFilters(apiKeyID, toolsetID string) error {
	if apiKeyID != "" && !canonicalUUID(apiKeyID) {
		return access.Invalid("api_key_id", "Use a canonical UUID.")
	}
	if toolsetID != "" && !canonicalUUID(toolsetID) {
		return access.Invalid("toolset_id", "Use a canonical UUID.")
	}
	return nil
}

func (m *Management) Register(mux *http.ServeMux) {
	m.Access.Route(mux, "GET /api/v1/mcp-tool-calls", m.list)
	m.Access.Route(mux, "GET /api/v1/mcp-tool-calls/{call_id}", m.get)
	m.Access.Route(mux, "POST /api/v1/mcp-tool-calls/{call_id}/resolve", m.resolve)
}

type ResolveOutcome string

const (
	ResolveSettled     ResolveOutcome = "settled"
	ResolveNotAccepted ResolveOutcome = "not_accepted"
)

func (o ResolveOutcome) status() Status { return Status(o) }

type resolveRequest struct {
	Outcome        ResolveOutcome `json:"outcome"`
	EvidenceDigest string         `json:"evidence_digest"`
	ObservedCost   *string        `json:"observed_cost"`
	ObservedCharge *string        `json:"observed_charge"`
}

var resolveAmountPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func resolveAmount(raw *string, field string) (string, error) {
	if raw == nil || *raw == "" {
		return "0", nil
	}
	if !resolveAmountPattern.MatchString(*raw) {
		return "", access.Invalid(field, "Use a non-negative decimal amount.")
	}
	return *raw, nil
}

func resolveAmountIsZero(amount string) bool {
	return strings.Trim(amount, "0.") == ""
}

func checkResolveBounds(call ToolCall, observedCost, observedCharge string) error {
	if !limits.WithinBound(call.CostBound, call.ObservedCost, observedCost) {
		return access.Invalid("observed_cost", "The observed cost exceeds the held debit bound.")
	}
	if !limits.WithinBound(call.ChargeBound, call.ObservedCharge, observedCharge) {
		return access.Invalid("observed_charge", "The observed charge exceeds the held debit bound.")
	}
	return nil
}

func validateResolveInput(outcome ResolveOutcome, evidenceDigest string) error {
	if outcome != ResolveSettled && outcome != ResolveNotAccepted {
		return access.Invalid("outcome", "Use settled or not_accepted.")
	}
	if _, ok := parseEvidenceDigest(evidenceDigest); !ok {
		return access.Invalid("evidence_digest", "Use a lowercase SHA-256 hex digest of the reviewed evidence.")
	}
	return nil
}

func storedResolutionEvidence(ctx context.Context, q access.Queryer, call ToolCall) ([]byte, bool, error) {
	var evidence []byte
	var err error
	switch call.Status {
	case StatusSettled:
		err = q.QueryRow(ctx, `SELECT evidence_digest FROM olp.execution_debit_results WHERE dispatch_id=$1 AND final ORDER BY created_at DESC LIMIT 1`, call.DispatchID).Scan(&evidence)
	case StatusNotAccepted:
		err = q.QueryRow(ctx, `SELECT evidence_digest FROM olp.execution_debit_resolutions WHERE dispatch_id=$1`, call.DispatchID).Scan(&evidence)
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return evidence, true, nil
}

func (m *Management) resolve(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "call_id")
	if err != nil {
		return access.Reply{}, err
	}
	var request resolveRequest
	if err = access.Decode(r, &request); err != nil {
		return access.Reply{}, err
	}
	if err = validateResolveInput(request.Outcome, request.EvidenceDigest); err != nil {
		return access.Reply{}, err
	}
	observedCost, err := resolveAmount(request.ObservedCost, "observed_cost")
	if err != nil {
		return access.Reply{}, err
	}
	observedCharge, err := resolveAmount(request.ObservedCharge, "observed_charge")
	if err != nil {
		return access.Reply{}, err
	}
	if request.Outcome == ResolveNotAccepted && (!resolveAmountIsZero(observedCost) || !resolveAmountIsZero(observedCharge)) {
		return access.Reply{}, access.Invalid("observed_cost", "A not accepted finding requires zero observed amounts.")
	}
	evidence, _ := parseEvidenceDigest(request.EvidenceDigest)
	call, err := loadToolCall(r.Context(), m.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&call.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if call.Status == request.Outcome.status() {
		stored, found, storedErr := storedResolutionEvidence(r.Context(), m.Access.Pool, call)
		if storedErr != nil {
			return access.Reply{}, storedErr
		}
		if found && bytes.Equal(stored, evidence) && limits.AmountEqual(call.ObservedCost, observedCost) && limits.AmountEqual(call.ObservedCharge, observedCharge) {
			return access.Detail(call, call.ID), nil
		}
		return access.Reply{}, access.Fail(409, "debit_already_resolved", "This held debit was already resolved by different evidence.")
	}
	if call.Status != StatusOutcomeUnknown {
		return access.Reply{}, access.Fail(409, "tool_call_not_unknown", "Only a tool call with an unknown outcome can be resolved.")
	}
	if request.Outcome == ResolveSettled {
		if err = checkResolveBounds(call, observedCost, observedCharge); err != nil {
			return access.Reply{}, err
		}
	}
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	claim, replay, err := m.Access.Replay(r, tx, p, request)
	if err != nil {
		return access.Reply{}, err
	}
	if replay != nil {
		return access.Commit(r, tx, *replay)
	}
	switch request.Outcome {
	case ResolveSettled:
		resultDigest := sha256.Sum256([]byte("olp:mcp-tool-resolution:" + call.DispatchID + ":" + request.EvidenceDigest))
		err = limits.SettleToolDebitTx(r.Context(), tx, limits.DebitResult{
			DispatchID: call.DispatchID, ResultDigest: resultDigest[:], EvidenceDigest: evidence,
			EventID: call.ID, Cost: observedCost, Charge: observedCharge, Final: true, Complete: true,
		})
	case ResolveNotAccepted:
		err = limits.ResolveNotAcceptedTx(r.Context(), tx, limits.NonAcceptanceProof{
			DispatchID: call.DispatchID, ActorID: p.UserID(), EvidenceDigest: evidence,
		})
	}
	if err != nil {
		if errors.Is(err, limits.ErrDebitState) || errors.Is(err, limits.ErrDebitConflict) {
			return access.Reply{}, access.Fail(409, "debit_already_resolved", "This held debit was already resolved by different evidence.")
		}
		return access.Reply{}, err
	}
	tag, err := tx.Exec(r.Context(), `UPDATE olp.mcp_tool_calls SET status=$2 WHERE id=$1 AND status='outcome_unknown'`, call.ID, string(request.Outcome.status()))
	if err != nil {
		return access.Reply{}, err
	}
	if tag.RowsAffected() != 1 {
		return access.Reply{}, access.Fail(409, "debit_already_resolved", "This held debit was already resolved by different evidence.")
	}
	if err = access.AuditForProject(r.Context(), tx, r, p.Actor(), "mcp_tool_call.resolve", "mcp_tool_call", call.ID, "success", &call.ProjectID); err != nil {
		return access.Reply{}, err
	}
	updated, err := loadToolCall(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	reply := access.Detail(updated, updated.ID)
	if err = m.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}

func (m *Management) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	key := r.URL.Query().Get("api_key_id")
	toolset := r.URL.Query().Get("toolset_id")
	if err := validateToolCallFilters(key, toolset); err != nil {
		return access.Reply{}, err
	}
	rows, err := m.Access.Pool.Query(r.Context(), "SELECT "+toolCallColumns+toolCallFrom+" WHERE c.id<$1 AND ($2 OR c.project_id=ANY($3::uuid[])) AND ($4='' OR c.api_key_id=$4::uuid) AND ($5='' OR c.toolset_id=$5::uuid) ORDER BY c.id DESC LIMIT $6", page.Before, p.AllProjects, p.ProjectIDs(), key, toolset, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []ToolCall{}
	for rows.Next() {
		call, err := scanToolCall(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, call)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(call ToolCall) string { return call.ID }), nil
}

func (m *Management) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "call_id")
	if err != nil {
		return access.Reply{}, err
	}
	call, err := loadToolCall(r.Context(), m.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&call.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(call, call.ID), nil
}
