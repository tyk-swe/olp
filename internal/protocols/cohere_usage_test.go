package protocols

import (
	"encoding/json"
	"testing"
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
