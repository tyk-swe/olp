package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/messages"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/workload"
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
	if got := completedToolStatus(upstream.ToolResult{}); got != accounting.StatusSucceeded {
		t.Errorf("successful result status = %q, want %q", got, accounting.StatusSucceeded)
	}
	if got := completedToolStatus(upstream.ToolResult{IsError: true}); got != accounting.StatusUpstreamError {
		t.Errorf("error result status = %q, want %q", got, accounting.StatusUpstreamError)
	}
}

func TestRepeatOutcomePrecedence(t *testing.T) {
	cases := []struct {
		repeat    *accounting.RetainedCall
		hasResult bool
		want      repeatKind
	}{
		{&accounting.RetainedCall{Conflict: true, Status: accounting.StatusSucceeded}, true, repeatConflict},
		{&accounting.RetainedCall{Status: accounting.StatusGuardrailBlocked}, true, repeatGuardrailBlocked},
		{&accounting.RetainedCall{Status: accounting.StatusInProgress}, true, repeatInProgress},
		{&accounting.RetainedCall{Status: accounting.StatusOutcomeUnknown}, true, repeatOutcomeUnknown},
		{&accounting.RetainedCall{Status: accounting.StatusSucceeded}, true, repeatRetainedResult},
		{&accounting.RetainedCall{Status: accounting.StatusSucceeded}, false, repeatResultNotRetained},
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
	if got := executeCallIDFailure(""); got != failureCallIDRequired {
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
	supported := messages.SupportedProtocols()
	if len(supported) == 0 {
		t.Fatal("no supported protocols reported")
	}
	for _, version := range supported {
		if got := messages.NegotiateProtocol(version); got != version {
			t.Errorf("messages.NegotiateProtocol(%q) = %q, want the requested version", version, got)
		}
	}
	if got := messages.NegotiateProtocol("1900-01-01"); got != supported[0] {
		t.Errorf("messages.NegotiateProtocol(unsupported) = %q, want preferred %q", got, supported[0])
	}
}

func TestExecuteSettlementAndReplay(t *testing.T) {
	decisions := []guardrails.Evidence{{Phase: guardrails.Output, Outcome: "evaluated"}}
	for _, tc := range []struct {
		name    string
		isError bool
		refusal error
		status  accounting.Status
	}{
		{name: "success", status: accounting.StatusSucceeded},
		{name: "upstream error", isError: true, status: accounting.StatusUpstreamError},
		{name: "guardrail refusal", refusal: guardrails.ErrBlocked, status: accounting.StatusGuardrailBlocked},
	} {
		for _, settlementFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/settlement_failed=%t", tc.name, settlementFails), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				fixture := newExecutionFixture(t, executionScenario{isError: tc.isError, settlementFails: settlementFails, cancel: cancel,
					guard: func(_ context.Context, evaluation GuardrailEvaluation) (guardrails.Result, error) {
						if evaluation.Phase == guardrails.Input {
							return guardrails.Result{Segments: evaluation.Segments}, nil
						}
						if evaluation.Phase != guardrails.Output || evaluation.RequestID != "attempt" {
							t.Fatalf("unexpected guardrail evaluation: %+v", evaluation)
						}
						return guardrails.Result{Changed: true, Segments: slices.Collect(func(yield func(guardrails.Segment) bool) {
							for _, segment := range evaluation.Segments {
								if segment.ID == "content0" {
									if !yield(guardrails.Segment{ID: segment.ID, Text: "redacted"}) {
										return
									}
									continue
								}
								if !yield(segment) {
									return
								}
							}
						}), Decisions: decisions}, tc.refusal
					},
				})
				execution := fixture.executor.Execute(ctx, fixture.request)
				if len(fixture.ledger.settled) != 1 || len(fixture.peer.calls) != 1 || len(fixture.ledger.dispatched) != 1 {
					t.Fatalf("call was not dispatched and settled exactly once: %+v", execution)
				}
				evidence := <-fixture.ledger.settled
				dispatched := <-fixture.ledger.dispatched
				expectedText := func() string {
					if tc.refusal != nil {
						return "original"
					}
					return "redacted"
				}()
				expected := upstream.ToolResult{Content: []upstream.ToolContent{{Type: "text", Text: expectedText}}, StructuredContent: json.RawMessage(`{"value":42}`), IsError: tc.isError}
				encoded, err := json.Marshal(expected)
				if err != nil {
					t.Fatal(err)
				}
				if evidence.Status != tc.status || evidence.ResultDigest != digestBytes(encoded) || evidence.IsError != tc.isError || evidence.ServerID != fixture.request.Tool.ServerID || evidence.ServerRevisionID != fixture.request.Tool.ServerRevisionID || !evidence.StartedAt.Equal(dispatched.StartedAt) || evidence.CompletedAt.Before(evidence.StartedAt) || !reflect.DeepEqual(evidence.GuardrailDecisions, decisions) {
					t.Fatalf("incorrect settlement evidence: %+v", evidence)
				}
				if !execution.ListChanged || execution.Certification != sessions.Certified {
					t.Fatalf("lost certification metadata: %+v", execution)
				}
				if settlementFails {
					uncertain, ok := execution.Outcome.(Uncertain)
					if !ok || uncertain.Message == "" || len(fixture.ledger.unknown) != 1 {
						t.Fatalf("failed settlement exposed a result: %+v", execution)
					}
					unknown := <-fixture.ledger.unknown
					if unknown.Status != accounting.StatusOutcomeUnknown || unknown.ResultDigest != evidence.ResultDigest || unknown.IsError != evidence.IsError || !reflect.DeepEqual(unknown.GuardrailDecisions, evidence.GuardrailDecisions) || !reflect.DeepEqual(unknown.IdentityDecisions, evidence.IdentityDecisions) {
						t.Fatalf("uncertain outcome lost evidence: %+v", unknown)
					}
				} else if tc.refusal != nil {
					blocked, ok := execution.Outcome.(Blocked)
					if !ok || blocked.Failure.Code != "guardrail_blocked" {
						t.Fatalf("guardrail refusal exposed a result: %+v", execution)
					}
				} else {
					completed, ok := execution.Outcome.(Completed)
					if !ok || !reflect.DeepEqual(completed.Result, expected) {
						t.Fatalf("wrong completed result: %+v", execution)
					}
					completed.Result.Content[0].Text = "caller modified result"
					completed.Result.StructuredContent[0] = '['
				}
				repeated := fixture.executor.Execute(t.Context(), fixture.request)
				if len(fixture.peer.calls) != 1 {
					t.Fatal("replay dispatched the tool again")
				}
				if settlementFails || tc.refusal != nil {
					if _, ok := repeated.Outcome.(ReplayUnavailable); !ok {
						t.Fatalf("replay exposed an unavailable result: %+v", repeated)
					}
					return
				}
				replayed, ok := repeated.Outcome.(Replayed)
				if !ok || !reflect.DeepEqual(replayed.Result, expected) {
					t.Fatalf("replay lost or shared the retained result: %+v", repeated)
				}
			})
		}
	}
}

func TestExecuteRefusesBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		scenario   executionScenario
	}{
		{name: "catalog drift", code: "tool_definition_changed", scenario: executionScenario{drift: true}},
		{name: "authorization revoked", code: "permission_denied", scenario: executionScenario{denySecondCheck: true}},
		{name: "personal grant revoked", code: "user_grant_required", scenario: executionScenario{revokedGrant: true}},
		{name: "dispatch record failed", code: "debit_ledger_unavailable", scenario: executionScenario{dispatchFails: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newExecutionFixture(t, tc.scenario)
			result := fixture.executor.Execute(t.Context(), fixture.request)
			refused, ok := result.Outcome.(Refused)
			if !ok || refused.Failure.Code != tc.code || len(fixture.peer.calls) != 0 || len(fixture.ledger.released) != 1 {
				t.Fatalf("call crossed the dispatch boundary: %+v", result)
			}
		})
	}
}

func TestExecuteTransportFailureIsNeverRetried(t *testing.T) {
	fixture := newExecutionFixture(t, executionScenario{callFails: true})
	first := fixture.executor.Execute(t.Context(), fixture.request)
	if _, ok := first.Outcome.(Uncertain); !ok {
		t.Fatalf("transport failure was not uncertain: %+v", first)
	}
	repeated := fixture.executor.Execute(t.Context(), fixture.request)
	unavailable, ok := repeated.Outcome.(ReplayUnavailable)
	if !ok || unavailable.Call.Status != accounting.StatusOutcomeUnknown || len(fixture.peer.calls) != 1 || len(fixture.ledger.settled) != 0 || len(fixture.ledger.released) != 0 {
		t.Fatalf("uncertain dispatch was retried, released or settled: %+v", repeated)
	}
}

type executionScenario struct {
	isError, settlementFails, drift, denySecondCheck, revokedGrant, dispatchFails, callFails bool
	cancel                                                                                   context.CancelFunc
	guard                                                                                    func(context.Context, GuardrailEvaluation) (guardrails.Result, error)
}
type executionFixture struct {
	executor *Executor
	request  CallRequest
	ledger   *executionLedger
	peer     *executionPeer
}

func newExecutionFixture(t *testing.T, scenario executionScenario) executionFixture {
	t.Helper()
	project := "project"
	ledger := &executionLedger{t: t, reservation: &accounting.Reservation{AttemptID: "attempt", DispatchID: "dispatch"}, scenario: scenario,
		settled: make(chan accounting.AttemptEvidence, 2), unknown: make(chan accounting.AttemptEvidence, 2), dispatched: make(chan accounting.AttemptEvidence, 2), released: make(chan struct{}, 2)}
	peer := &executionPeer{t: t, ledger: ledger, scenario: scenario, calls: make(chan json.RawMessage, 2)}
	catalog, err := upstream.CertifyCatalog(t.Context(), peer, nil)
	if err != nil {
		t.Fatal(err)
	}
	digest := func() string {
		if scenario.drift {
			return "changed"
		}
		return catalog.Digest
	}()
	policy := func() ToolPolicies {
		if scenario.guard == nil {
			return nil
		}
		engine, err := guardrails.Compile([]guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrails.Output, Action: guardrails.Redact}}, map[string]guardrails.Revision{
			"revision": {ID: "revision", GuardrailID: "guard", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["original"]}`), TimeoutMS: 1000, OnError: guardrails.FailClosed},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return executionPolicies{engine: engine}
	}()
	executor := NewExecutor(ExecutorOptions{Authority: executionAuthority{denySecond: scenario.denySecondCheck, checks: make(chan struct{}, 4)},
		Grants: executionGrants{revoked: scenario.revokedGrant}, Connect: func(context.Context, string, []byte, upstream.TransportOptions) (upstream.Upstream, error) {
			return peer, nil
		},
		Ledger: ledger, Limiter: executionLimiter{}, Auth: secrets.NewAuthKey(make([]byte, 32), "installation"), Guardrails: scenario.guard,
	})
	return executionFixture{executor: executor, ledger: ledger, peer: peer, request: CallRequest{Authority: keyauthority.Authority{ID: "principal", ProjectID: &project},
		ToolsetID: "toolset", ToolsetRevisionID: "toolset-revision", CallID: "call", Arguments: json.RawMessage(`{}`), Policies: policy,
		Tool: routing.MCPToolsetTool{ServerID: "server", ServerRevisionID: "revision", Name: "lookup", Alias: "lookup", CatalogDigest: digest, OAuth: func() *routing.MCPOAuth {
			if scenario.revokedGrant {
				return &routing.MCPOAuth{}
			}
			return nil
		}()},
	}}
}

type executionPolicies struct{ engine *guardrails.Engine }

func (p executionPolicies) ToolGuardrailEngine(string, string) *guardrails.Engine { return p.engine }

type executionLimiter struct{}

func (executionLimiter) Reserve(context.Context, limits.Request) (*limits.Lease, error) {
	return nil, nil
}

type executionAuthority struct {
	denySecond bool
	checks     chan struct{}
}

func (a executionAuthority) AuthorizeMCPTool(keyauthority.Authority, string, string, string, string, string) error {
	a.checks <- struct{}{}
	if a.denySecond && len(a.checks) == 2 {
		return errors.New("authority revoked")
	}
	return nil
}
func (executionAuthority) AdmitTaskCapabilityAttempt(context.Context, keyauthority.Authority, string, string, string, *string, string) error {
	return nil
}
func (executionAuthority) VerifyEntraUser(context.Context, string, keyauthority.Authority, string, string) (workload.UserAssertion, error) {
	return workload.UserAssertion{}, errors.New("not configured")
}

type executionGrants struct{ revoked bool }

func (executionGrants) Resolve(context.Context, keyauthority.Authority, routing.MCPToolsetTool) ([]byte, error) {
	return []byte("credential"), nil
}
func (executionGrants) ResolveEntra(context.Context, keyauthority.Authority, routing.MCPToolsetTool, workload.UserAssertion) ([]byte, error) {
	return nil, errors.New("not configured")
}
func (executionGrants) Delegate(context.Context, keyauthority.Authority, routing.MCPToolsetTool) ([]byte, error) {
	return nil, errors.New("not configured")
}
func (g executionGrants) CheckGrant(context.Context, keyauthority.Authority, routing.MCPToolsetTool) error {
	if g.revoked {
		return errors.New("grant revoked")
	}
	return nil
}

type executionPeer struct {
	t        *testing.T
	ledger   *executionLedger
	scenario executionScenario
	calls    chan json.RawMessage
}

func (p *executionPeer) Protocol() string { return messages.SupportedProtocols()[0] }
func (p *executionPeer) ListTools(context.Context, string) (upstream.ToolList, error) {
	return upstream.ToolList{Tools: []upstream.ListedTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, nil
}
func (p *executionPeer) ListChanged() bool     { return true }
func (p *executionPeer) Close(context.Context) {}
func (p *executionPeer) CallTool(_ context.Context, name string, arguments json.RawMessage) (upstream.ToolResult, error) {
	if name != "lookup" || len(p.ledger.dispatched) != 1 {
		p.t.Fatal("tool called without its durable dispatch record")
	}
	p.calls <- arguments
	if p.scenario.cancel != nil {
		p.scenario.cancel()
	}
	if p.scenario.callFails {
		return upstream.ToolResult{}, errors.New("response lost")
	}
	return upstream.ToolResult{Content: []upstream.ToolContent{{Type: "text", Text: "original"}}, StructuredContent: json.RawMessage(`{"value":42}`), IsError: p.scenario.isError}, nil
}

type executionLedger struct {
	t                            *testing.T
	reservation                  *accounting.Reservation
	scenario                     executionScenario
	prior                        *accounting.RetainedCall
	settled, unknown, dispatched chan accounting.AttemptEvidence
	released                     chan struct{}
}

func (l *executionLedger) Reserve(context.Context, accounting.ReserveInput) (accounting.ReserveResult, error) {
	if l.prior != nil {
		return accounting.ReserveResult{Repeat: l.prior}, nil
	}
	return accounting.ReserveResult{Reservation: l.reservation}, nil
}
func (l *executionLedger) MarkDispatched(_ context.Context, _ *accounting.Reservation, evidence accounting.AttemptEvidence) error {
	if l.scenario.dispatchFails {
		return errors.New("dispatch record failed")
	}
	l.dispatched <- evidence
	l.prior = &accounting.RetainedCall{AttemptID: "attempt", DispatchID: "dispatch", Status: accounting.StatusDispatched}
	return nil
}
func (l *executionLedger) ReleaseUndispatched(context.Context, *accounting.Reservation) error {
	l.released <- struct{}{}
	return nil
}
func (l *executionLedger) RefuseUndispatched(context.Context, *accounting.Reservation, accounting.AttemptEvidence) error {
	l.released <- struct{}{}
	return nil
}
func (l *executionLedger) Settle(ctx context.Context, reservation *accounting.Reservation, evidence accounting.AttemptEvidence) error {
	if ctx.Err() != nil || reservation != l.reservation || len(l.settled) != 0 {
		l.t.Fatal("settlement must run once despite request cancellation")
	}
	l.settled <- evidence
	if l.scenario.settlementFails {
		return errors.New("settlement failed")
	}
	l.prior = &accounting.RetainedCall{AttemptID: "attempt", DispatchID: "dispatch", Status: evidence.Status}
	return nil
}
func (l *executionLedger) MarkUnknown(ctx context.Context, reservation *accounting.Reservation, evidence accounting.AttemptEvidence) error {
	if ctx.Err() != nil || reservation != l.reservation || len(l.unknown) != 0 {
		l.t.Fatal("uncertain outcome must be recorded despite request cancellation")
	}
	l.unknown <- evidence
	l.prior = &accounting.RetainedCall{AttemptID: "attempt", DispatchID: "dispatch", Status: accounting.StatusOutcomeUnknown}
	return nil
}
