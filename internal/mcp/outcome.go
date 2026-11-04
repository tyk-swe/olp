package mcp

import (
	"time"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
)

type Failure struct {
	Code      string
	Message   string
	Guardrail *guardrails.Error
}

func refusal(code, message string) *Failure { return &Failure{Code: code, Message: message} }

type Receipt struct {
	AttemptID  string
	DispatchID string
}

func receipt(reservation *accounting.Reservation) Receipt {
	return Receipt{AttemptID: reservation.AttemptID, DispatchID: reservation.DispatchID}
}

type Outcome interface{ outcome() }
type Execution struct {
	Outcome       Outcome
	ListChanged   bool
	Certification sessions.Certification
}
type Completed struct {
	Receipt Receipt
	Result  upstream.ToolResult
}
type Refused struct {
	Failure    *Failure
	RetryAfter time.Duration
}
type Blocked struct {
	Receipt Receipt
	Failure *Failure
}
type Uncertain struct {
	Receipt Receipt
	Message string
}
type Replayed struct {
	Receipt Receipt
	Result  upstream.ToolResult
}
type ReplayUnavailable struct {
	Call   accounting.RetainedCall
	Reason repeatKind
}

func (Completed) outcome()         {}
func (Refused) outcome()           {}
func (Blocked) outcome()           {}
func (Uncertain) outcome()         {}
func (Replayed) outcome()          {}
func (ReplayUnavailable) outcome() {}

var (
	failurePermissionDenied  = refusal("permission_denied", "This API key may no longer use the toolset.")
	failureLedger            = refusal("debit_ledger_unavailable", "The durable debit ledger is unavailable, so the call was not dispatched.")
	failureDefinitionChanged = refusal("tool_definition_changed", "The upstream tool definitions differ from the pinned published definitions, so the call was not dispatched. Publish a reviewed revision to use the changed definitions.")
	failureGuardrailBlocked  = refusal("guardrail_blocked", "The tool call could not satisfy the configured guardrails.")
	failureCallIDRequired    = refusal("call_id_required", "Supply a caller call identifier for this tool call.")
	failureCallIDInvalid     = refusal("invalid_call_id", "Supply a caller call identifier of 1-256 bytes without control characters.")
)
