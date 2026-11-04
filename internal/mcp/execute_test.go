package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestToolLookupFailureMapping(t *testing.T) {
	cases := []struct {
		found toolLookup
		want  *apiError
	}{
		{toolFound, nil},
		{toolDrifted, errToolDefinitionChanged},
		{toolInvalidSchema, errToolSchemaInvalid},
		{toolServerMissing, errToolNotFound},
		{toolMissing, errToolNotFound},
	}
	for _, tc := range cases {
		if got := toolLookupFailure(tc.found); got != tc.want {
			t.Errorf("toolLookupFailure(%v) = %v, want %v", tc.found, got, tc.want)
		}
	}
}

func TestCompletedToolStatus(t *testing.T) {
	if got := completedToolStatus(mcpservers.ToolResult{}); got != StatusSucceeded {
		t.Errorf("successful result status = %q, want %q", got, StatusSucceeded)
	}
	if got := completedToolStatus(mcpservers.ToolResult{IsError: true}); got != StatusUpstreamError {
		t.Errorf("error result status = %q, want %q", got, StatusUpstreamError)
	}
}

func TestRepeatOutcomePrecedence(t *testing.T) {
	cases := []struct {
		repeat    *RetainedCall
		hasResult bool
		want      repeatKind
	}{
		{&RetainedCall{Conflict: true, Status: StatusSucceeded}, true, repeatConflict},
		{&RetainedCall{Status: StatusGuardrailBlocked}, true, repeatGuardrailBlocked},
		{&RetainedCall{Status: StatusInProgress}, true, repeatInProgress},
		{&RetainedCall{Status: StatusOutcomeUnknown}, true, repeatOutcomeUnknown},
		{&RetainedCall{Status: StatusSucceeded}, true, repeatRetainedResult},
		{&RetainedCall{Status: StatusSucceeded}, false, repeatResultNotRetained},
	}
	for _, tc := range cases {
		if got := repeatOutcome(tc.repeat, tc.hasResult); got != tc.want {
			t.Errorf("repeatOutcome(%+v, %v) = %v, want %v", tc.repeat, tc.hasResult, got, tc.want)
		}
	}
}

func TestRepeatFailureCodesAreStablePerKind(t *testing.T) {
	seen := map[string]repeatKind{}
	for _, kind := range []repeatKind{repeatConflict, repeatInProgress, repeatOutcomeUnknown, repeatResultNotRetained} {
		code, message := repeatFailure(kind)
		if code == "" || message == "" {
			t.Fatalf("repeatFailure(%v) returned empty code or message", kind)
		}
		if other, dup := seen[code]; dup {
			t.Fatalf("repeatFailure code %q shared by kinds %v and %v", code, other, kind)
		}
		seen[code] = kind
	}
	fallback, _ := repeatFailure(repeatRetainedResult)
	notRetained, _ := repeatFailure(repeatResultNotRetained)
	if fallback == "" || fallback != notRetained {
		t.Fatalf("repeatFailure fallback code %q, want the not-retained code %q", fallback, notRetained)
	}
}

func TestValidCallIDBounds(t *testing.T) {
	if !validCallID("call-1") {
		t.Error("validCallID rejected an ordinary identifier")
	}
	if !validCallID(strings.Repeat("a", maxCallIDBytes)) {
		t.Error("validCallID rejected an identifier at the byte bound")
	}
	for _, raw := range []string{"", "   ", strings.Repeat("a", maxCallIDBytes+1), "call\x01", "call\x7f"} {
		if validCallID(raw) {
			t.Errorf("validCallID accepted %q", raw)
		}
	}
}

func TestCallIDFailureDistinguishesMissingFromInvalid(t *testing.T) {
	if got := callIDFailure(nil); got != errCallIDRequired {
		t.Errorf("callIDFailure(nil) = %v, want required", got)
	}
	invalid := ""
	if got := callIDFailure(&invalid); got != errCallIDInvalid {
		t.Errorf("callIDFailure(empty) = %v, want invalid", got)
	}
	valid := "call-1"
	if got := callIDFailure(&valid); got != nil {
		t.Errorf("callIDFailure(valid) = %v, want nil", got)
	}
	if got := executeCallIDFailure(""); got != errCallIDRequired {
		t.Errorf("executeCallIDFailure(empty) = %v, want required", got)
	}
}

func TestNormalizedArgumentsDefaultsToEmptyObject(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(``), json.RawMessage(`null`)} {
		if got := normalizedArguments(raw); string(got) != `{}` {
			t.Errorf("normalizedArguments(%q) = %q, want {}", raw, got)
		}
	}
	supplied := json.RawMessage(`{"a":1}`)
	if got := normalizedArguments(supplied); string(got) != string(supplied) {
		t.Errorf("normalizedArguments rewrote supplied arguments to %q", got)
	}
}

func TestNegotiateProtocolFallsBackToPreferred(t *testing.T) {
	supported := mcpservers.SupportedProtocols()
	if len(supported) == 0 {
		t.Fatal("no supported protocols reported")
	}
	for _, version := range supported {
		if got := negotiateProtocol(version); got != version {
			t.Errorf("negotiateProtocol(%q) = %q, want the requested version", version, got)
		}
	}
	if got := negotiateProtocol("1900-01-01"); got != supported[0] {
		t.Errorf("negotiateProtocol(unsupported) = %q, want preferred %q", got, supported[0])
	}
}

func TestSettleToolCall(t *testing.T) {
	reservation := &Reservation{AttemptID: "attempt", DispatchID: "dispatch"}
	in := executeInput{tool: routing.MCPToolsetTool{ServerID: "server", ServerRevisionID: "revision", Alias: "lookup"}}
	decisions := []guardrails.Evidence{{Phase: guardrails.Output, Outcome: "evaluated"}}
	for _, tc := range []struct {
		name    string
		isError bool
		refusal error
		status  Status
	}{
		{name: "success", status: StatusSucceeded},
		{name: "upstream error", isError: true, status: StatusUpstreamError},
		{name: "guardrail refusal", refusal: guardrails.ErrBlocked, status: StatusGuardrailBlocked},
	} {
		for _, settleErr := range []error{nil, errors.New("settlement failed")} {
			t.Run(fmt.Sprintf("%s/settlement_failed=%t", tc.name, settleErr != nil), func(t *testing.T) {
				ledger := settlementLedger{t: t, reservation: reservation, settleErr: settleErr, settled: make(chan AttemptEvidence, 1), unknown: make(chan AttemptEvidence, 1)}
				server := &Server{Ledger: ledger, Guardrails: func(_ context.Context, evaluation GuardrailEvaluation) (guardrails.Result, error) {
					if evaluation.Phase != guardrails.Output || evaluation.RequestID != reservation.AttemptID {
						t.Fatalf("unexpected guardrail evaluation: %+v", evaluation)
					}
					return guardrails.Result{Changed: true, Segments: []guardrails.Segment{{ID: "content0", Text: "redacted"}}, Decisions: decisions}, tc.refusal
				}}
				original := mcpservers.ToolResult{Content: []mcpservers.ToolContent{{Type: "text", Text: "original"}}, IsError: tc.isError}
				dispatched := dispatchResult{result: original, started: time.Now(), completed: time.Now(), listChanged: true, certification: certificationCertified}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				outcome := server.settleToolCall(ctx, in, reservation, dispatched, &toolGuardrailState{masks: &guardrails.Masking{}})
				if len(ledger.settled) != 1 {
					t.Fatal("expected one settlement")
				}
				evidence := <-ledger.settled
				wantResult := func() mcpservers.ToolResult {
					if tc.refusal != nil {
						return original
					}
					return mcpservers.ToolResult{Content: []mcpservers.ToolContent{{Type: "text", Text: "redacted"}}, IsError: tc.isError}
				}()
				encoded, err := json.Marshal(wantResult)
				if err != nil {
					t.Fatal(err)
				}
				if evidence.Status != tc.status || evidence.ResultDigest != digestBytes(encoded) || evidence.IsError != tc.isError ||
					evidence.ServerID != in.tool.ServerID || evidence.ServerRevisionID != in.tool.ServerRevisionID ||
					!evidence.StartedAt.Equal(dispatched.started) || !evidence.CompletedAt.Equal(dispatched.completed) ||
					!reflect.DeepEqual(evidence.GuardrailDecisions, decisions) {
					t.Fatalf("incorrect settlement evidence: %+v", evidence)
				}
				if outcome.reservation != reservation || !outcome.listChanged || outcome.certification != dispatched.certification {
					t.Fatalf("lost execution metadata: %+v", outcome)
				}
				retained, found := server.retainedResultFor(reservation.AttemptID)
				if settleErr != nil {
					if !outcome.outcomeUnknown || outcome.blocked != nil || outcome.unknownMessage == "" || found || len(ledger.unknown) != 1 {
						t.Fatalf("failed settlement was exposed as a completed call: %+v, retained=%t", outcome, found)
					}
					unknown := <-ledger.unknown
					if unknown.Status != StatusOutcomeUnknown || unknown.ResultDigest != evidence.ResultDigest || unknown.IsError != evidence.IsError ||
						unknown.ServerID != evidence.ServerID || unknown.ServerRevisionID != evidence.ServerRevisionID ||
						!unknown.StartedAt.Equal(evidence.StartedAt) || !unknown.CompletedAt.Equal(evidence.CompletedAt) ||
						!reflect.DeepEqual(unknown.GuardrailDecisions, evidence.GuardrailDecisions) || !reflect.DeepEqual(unknown.IdentityDecisions, evidence.IdentityDecisions) {
						t.Fatalf("unknown outcome lost settlement evidence: %+v", unknown)
					}
					return
				}
				if outcome.outcomeUnknown || len(ledger.unknown) != 0 {
					t.Fatalf("successful settlement was marked unknown: %+v", outcome)
				}
				if tc.refusal != nil {
					if outcome.blocked == nil || outcome.blocked.code != "guardrail_blocked" || found {
						t.Fatalf("blocked result was exposed: %+v, retained=%t", outcome, found)
					}
					return
				}
				if outcome.blocked != nil || !found || !reflect.DeepEqual(outcome.result, wantResult) || !reflect.DeepEqual(retained, wantResult) {
					t.Fatalf("settled result was not returned and retained: %+v, retained=%+v", outcome, retained)
				}
			})
		}
	}
}

type settlementLedger struct {
	t           *testing.T
	reservation *Reservation
	settleErr   error
	settled     chan AttemptEvidence
	unknown     chan AttemptEvidence
}

func (l settlementLedger) Reserve(context.Context, ReserveInput) (ReserveResult, error) {
	l.t.Fatal("unexpected reservation during settlement")
	return ReserveResult{}, nil
}

func (l settlementLedger) MarkDispatched(context.Context, *Reservation, AttemptEvidence) error {
	l.t.Fatal("unexpected dispatch during settlement")
	return nil
}

func (l settlementLedger) ReleaseUndispatched(context.Context, *Reservation) error {
	l.t.Fatal("unexpected release during settlement")
	return nil
}

func (l settlementLedger) RefuseUndispatched(context.Context, *Reservation, AttemptEvidence) error {
	l.t.Fatal("unexpected refusal before dispatch during settlement")
	return nil
}

func (l settlementLedger) Settle(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error {
	if ctx.Err() != nil || reservation != l.reservation || len(l.settled) != 0 {
		l.t.Fatal("settlement must run once for the dispatched reservation despite request cancellation")
	}
	l.settled <- evidence
	return l.settleErr
}

func (l settlementLedger) MarkUnknown(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error {
	if ctx.Err() != nil || reservation != l.reservation || len(l.unknown) != 0 || len(l.settled) != 1 || l.settleErr == nil {
		l.t.Fatal("unknown outcome must follow failed settlement despite request cancellation")
	}
	l.unknown <- evidence
	return nil
}
