package protocols

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func TestCohereTokensRefuseMalformedFractionalAndUnboundedCounts(t *testing.T) {
	for _, raw := range []string{"", "null", "true", `"5"`, "+1", "01", "1.", "1e", "1e+", "1 2", "NaN", "-1", "0.5", "1.0001", "5e-1", "9223372036854775808", "9.223372036854775808e18", "1e999999999999999999", "1e-99999999999999999", "\u00a05"} {
		t.Run(raw, func(t *testing.T) {
			if count, ok := cohereTokens(json.RawMessage(raw)); ok {
				t.Fatalf("accepted %q as %d", raw, count)
			}
		})
	}
}

func TestCohereTokensAcceptIntegralDecimalCounts(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]int64{"0": 0, "-0": 0, "11": 11, "11.0": 11, "1.1e1": 11, "1100e-2": 11, "9223372036854775807": 9223372036854775807} {
		t.Run(raw, func(t *testing.T) {
			if count, ok := cohereTokens(json.RawMessage(raw)); !ok || count != want {
				t.Fatalf("cohereTokens(%q) = %d, %t, want %d", raw, count, ok, want)
			}
		})
	}
}

func TestCohereUsageBillsTheBilledUnits(t *testing.T) {
	t.Parallel()
	body := `{"id":"cohere-1","finish_reason":"COMPLETE","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"usage":{"billed_units":{"input_tokens":11.0,"output_tokens":7},"tokens":{"input_tokens":213,"output_tokens":7}}}`
	c, err := Decode(wire.CohereChat, wire.Chat, []byte(body), "route", "")
	if err != nil {
		t.Fatal(err)
	}
	assertUsageJSON(t, c.Usage, `{"input_tokens":11,"output_tokens":7,"total_tokens":18}`)
	for _, usage := range []string{`{}`, `{"tokens":{"input_tokens":1,"output_tokens":1}}`, `{"billed_units":{"input_tokens":1}}`} {
		invalid := `{"id":"cohere-1","finish_reason":"COMPLETE","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"usage":` + usage + `}`
		if _, err := Decode(wire.CohereChat, wire.Chat, []byte(invalid), "route", ""); err == nil {
			t.Fatalf("accepted usage without billed units: %s", usage)
		}
	}
}
