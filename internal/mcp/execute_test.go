package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/mcpservers"
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
