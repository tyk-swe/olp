package mcpservers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

func validInput() Input {
	enabled := true
	return Input{Name: "tools", Enabled: &enabled}
}

func TestValidateAndNormalizeTrimsName(t *testing.T) {
	in := validInput()
	in.Name = "  tools  "
	if err := ValidateAndNormalize(&in, &egress.Policy{}); err != nil {
		t.Fatalf("ValidateAndNormalize: %v", err)
	}
	if in.Name != "tools" {
		t.Fatalf("Name = %q, want trimmed", in.Name)
	}
}

func TestValidateAndNormalizeRejections(t *testing.T) {
	price := func(v string) *string { return &v }
	limit := func(v int64) *int64 { return &v }
	cases := []struct {
		name   string
		mutate func(*Input)
		policy *egress.Policy
	}{
		{"missing enabled", func(in *Input) { in.Enabled = nil }, &egress.Policy{}},
		{"blank name", func(in *Input) { in.Name = "   " }, &egress.Policy{}},
		{"missing egress policy", func(*Input) {}, nil},
		{"unknown transport", func(in *Input) { in.Transport = "carrier_pigeon" }, &egress.Policy{}},
		{"negative unit price", func(in *Input) { in.UnitPrice = price("-1") }, &egress.Policy{}},
		{"malformed unit price", func(in *Input) { in.UnitPrice = price("1.2.3") }, &egress.Policy{}},
		{"tool price with blank name", func(in *Input) { in.ToolPrices = map[string]string{"": "1"} }, &egress.Policy{}},
		{"calls above ceiling", func(in *Input) { in.CallsPerMinute = limit(routing.MaxToolCallsPerMinute + 1) }, &egress.Policy{}},
		{"zero calls limit", func(in *Input) { in.CallsPerMinute = limit(0) }, &egress.Policy{}},
		{"concurrency above ceiling", func(in *Input) { in.MaxConcurrency = limit(routing.MaxToolConcurrency + 1) }, &egress.Policy{}},
		{"stdio binding with endpoint", func(in *Input) {
			in.Stdio = &routing.MCPStdioBinding{Executable: "tool", Digest: strings.Repeat("0", 64)}
			in.Endpoint = "https://example.com/mcp"
		}, &egress.Policy{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mutate(&in)
			if err := ValidateAndNormalize(&in, tc.policy); err == nil {
				t.Fatalf("ValidateAndNormalize accepted %s", tc.name)
			}
		})
	}
}

func TestValidateAndNormalizeAcceptsPricedToolsAtCeilings(t *testing.T) {
	in := validInput()
	price := "0.25"
	calls := routing.MaxToolCallsPerMinute
	concurrency := routing.MaxToolConcurrency
	in.UnitPrice = &price
	in.ToolPrices = map[string]string{"lookup": "1.5"}
	in.CallsPerMinute = &calls
	in.MaxConcurrency = &concurrency
	if err := ValidateAndNormalize(&in, &egress.Policy{}); err != nil {
		t.Fatalf("ValidateAndNormalize rejected values at the ceilings: %v", err)
	}
}

func TestValidateAndNormalizeRejectsTooManyToolPrices(t *testing.T) {
	in := validInput()
	in.ToolPrices = make(map[string]string, 129)
	for i := range 129 {
		in.ToolPrices[fmt.Sprintf("tool-%d", i)] = "1"
	}
	if err := ValidateAndNormalize(&in, &egress.Policy{}); err == nil {
		t.Fatal("ValidateAndNormalize accepted more tool prices than the tool bound")
	}
}

func TestCredentialParsing(t *testing.T) {
	for _, raw := range []string{``, `null`, ` null `} {
		got, err := Credential(json.RawMessage(raw))
		if err != nil || got != nil {
			t.Errorf("Credential(%q) = %q, %v; want nil, nil", raw, got, err)
		}
	}
	got, err := Credential(json.RawMessage(`"token"`))
	if err != nil || string(got) != "token" {
		t.Errorf("Credential(string) = %q, %v", got, err)
	}
	for _, raw := range []string{`""`, `42`, `["token"]`, `"line\nbreak"`, `"` + strings.Repeat("a", 4097) + `"`} {
		if _, err := Credential(json.RawMessage(raw)); err == nil {
			t.Errorf("Credential accepted %s", raw)
		}
	}
}
