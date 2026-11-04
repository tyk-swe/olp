package mcpservers

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestServerSummaryPreservesMetadata(t *testing.T) {
	price, secret := "1.25", "private-id"
	for _, definition := range []Definition{
		{ID: "empty", ToolPrices: map[string]string{}},
		{
			ID: "server", ProjectID: "project", Name: "tools", Transport: "stdio", Enabled: true,
			Endpoint: "https://tools.example.com", HasCredential: true, ETag: "etag", RevisionID: "revision", Revision: 3,
			Catalog: &Catalog{Digest: "catalog-digest"}, CertifiedAt: time.Now().UTC(), RetiredAt: new(time.Now().UTC()),
			UnitPrice: &price, ToolPrices: map[string]string{"lookup": "2"}, CallsPerMinute: 42, MaxConcurrency: 4,
			PerKeyCallsPerMinute: 21, PerKeyMaxConcurrency: 2, OAuth: &routing.MCPOAuth{ClientID: "client"},
			Stdio: &routing.MCPStdioBinding{Executable: "tools", Digest: "executable-digest"}, Delegation: &routing.MCPDelegation{},
			CredentialID: &secret, DelegationKeyID: &secret, EntraClientSecretID: &secret,
		},
	} {
		t.Run(definition.ID, func(t *testing.T) {
			detail, err := json.Marshal(definition)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(summarize(definition))
			if err != nil {
				t.Fatal(err)
			}
			var fields, summary map[string]json.RawMessage
			if err := json.Unmarshal(detail, &fields); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &summary); err != nil {
				t.Fatal(err)
			}
			if len(summary) != len(fields)-1 || summary["catalog"] != nil {
				t.Fatalf("summary must omit only the catalog: %s", encoded)
			}
			for name, value := range fields {
				if name != "catalog" && string(summary[name]) != string(value) {
					t.Errorf("summary field %s = %s, want %s", name, summary[name], value)
				}
			}
		})
	}
}

func TestRegistrationUpdateDistinguishesOmittedAndNullFields(t *testing.T) {
	price := "1.25"
	previous := &Definition{
		UnitPrice: &price, ToolPrices: map[string]string{"lookup": "2"}, OAuth: &routing.MCPOAuth{ClientID: "client"},
		CallsPerMinute: 42, MaxConcurrency: 4, PerKeyCallsPerMinute: 21, PerKeyMaxConcurrency: 2,
	}
	for _, tc := range []struct {
		name      string
		body      string
		wantPrice *string
		wantTools map[string]string
		wantOAuth *routing.MCPOAuth
		wantCalls int64
	}{
		{"omitted", `{}`, previous.UnitPrice, previous.ToolPrices, previous.OAuth, previous.CallsPerMinute},
		{"null", `{"unit_price":null,"tool_prices":null,"oauth":null,"calls_per_minute":null}`, nil, map[string]string{}, nil, routing.DefaultToolCallsPerMinute},
		{"explicit", `{"unit_price":"0","tool_prices":{"lookup":"3"},"oauth":{"client_id":"new"},"calls_per_minute":12}`, new("0"), map[string]string{"lookup": "3"}, &routing.MCPOAuth{ClientID: "new"}, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var input Input
			if err := json.Unmarshal([]byte(tc.body), &input); err != nil {
				t.Fatal(err)
			}
			merged := mergeAbsentPricingAndLimits(input, previous)
			if !reflect.DeepEqual(merged.UnitPrice, tc.wantPrice) || !reflect.DeepEqual(merged.ToolPrices, tc.wantTools) ||
				!reflect.DeepEqual(merged.OAuth, tc.wantOAuth) || merged.CallsPerMinute == nil || *merged.CallsPerMinute != tc.wantCalls {
				t.Fatalf("unexpected merged registration: %+v", merged)
			}
			if merged.MaxConcurrency == nil || *merged.MaxConcurrency != previous.MaxConcurrency ||
				merged.PerKeyCallsPerMinute == nil || *merged.PerKeyCallsPerMinute != previous.PerKeyCallsPerMinute ||
				merged.PerKeyMaxConcurrency == nil || *merged.PerKeyMaxConcurrency != previous.PerKeyMaxConcurrency {
				t.Fatalf("omitted limits were not preserved: %+v", merged)
			}
		})
	}
}
