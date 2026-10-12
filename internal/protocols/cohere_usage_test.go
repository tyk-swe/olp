package protocols

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestCohereTokensPreserveExactIntegralNumbers(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{"0", 0}, {"-0.0", 0}, {"5.0", 5}, {" 5.0\t", 5},
		{"1.2300e2", 123}, {"1000e-3", 1}, {"0.01e2", 1},
		{"9007199254740993", 9007199254740993}, {"9007199254740993.0", 9007199254740993},
		{"9223372036854775807", math.MaxInt64}, {"9.223372036854775807e18", math.MaxInt64},
		{"0e999999999999999999999999999999", 0},
		{"1." + strings.Repeat("0", 10000), 1},
	} {
		t.Run(tc.raw[:min(len(tc.raw), 40)], func(t *testing.T) {
			got, ok := cohereTokens(json.RawMessage(tc.raw))
			if !ok || got != tc.want {
				t.Fatalf("count=%d, valid=%v want %d", got, ok, tc.want)
			}
		})
	}
}

func TestCohereTokensRefuseMalformedFractionalAndUnboundedCounts(t *testing.T) {
	for _, raw := range []string{"", "null", "true", `"5"`, "+1", "01", "1.", "1e", "1e+", "1 2", "NaN", "-1", "0.5", "1.0001", "5e-1", "9223372036854775808", "9.223372036854775808e18", "1e999999999999999999", "1e-99999999999999999", "\u00a05"} {
		t.Run(raw, func(t *testing.T) {
			if count, ok := cohereTokens(json.RawMessage(raw)); ok {
				t.Fatalf("accepted %q as %d", raw, count)
			}
		})
	}
}

func TestCohereUsageRetainsExactCountsAndRefusesSumOverflow(t *testing.T) {
	u, err := cohereUsage(json.RawMessage(`{"billed_units":{"input_tokens":9007199254740993,"output_tokens":0}}`))
	if err != nil || u == nil || u.InputTokens != 9007199254740993 || u.TotalTokens != 9007199254740993 {
		t.Fatalf("rounded billed count: %+v %v", u, err)
	}
	for _, raw := range []string{
		`{"billed_units":{"input_tokens":9223372036854775808,"output_tokens":0}}`,
		`{"billed_units":{"input_tokens":9223372036854775807,"output_tokens":1}}`,
	} {
		if u, err := cohereUsage(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted out-of-range billed count: %+v", u)
		}
	}
}
