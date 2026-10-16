//go:build integration

package database

import (
	"encoding/json"
)

// lineCase is one (price, usage, started, successful, discount) pricing input.
type lineCase struct {
	name       string
	price      string
	usage      string
	successful bool
	discount   string
}

// The corpus walks every rule olp.price_usage_lines shares with
// olp.price_usage: share fallbacks, tiers, windows, batch, media, per-request
// and tool fees, discounts and the conditions that make an attempt unpriced.
var lineCases = []lineCase{
	{"plain input output",
		`{"input_per_million":3,"output_per_million":12}`,
		`{"input_tokens":1000,"output_tokens":250,"complete":true}`, true, "1"},
	{"cached input own rate",
		`{"input_per_million":3,"cached_input_per_million":1}`,
		`{"input_tokens":1000,"cached_input_tokens":400,"complete":true}`, true, "1"},
	{"cached input fallback",
		`{"input_per_million":3}`,
		`{"input_tokens":1000,"cached_input_tokens":400,"complete":true}`, true, "1"},
	{"generic cache write",
		`{"input_per_million":3,"cache_write_input_per_million":5}`,
		`{"input_tokens":1000,"cache_write_input_tokens":100,"complete":true}`, true, "1"},
	{"5m and 1h writes own rates",
		`{"input_per_million":3,"cache_write_input_per_million":5,"cache_write_5m_input_per_million":6,"cache_write_1h_input_per_million":7}`,
		`{"input_tokens":1000,"cache_write_input_tokens":100,"cache_write_5m_input_tokens":60,"cache_write_1h_input_tokens":20,"complete":true}`, true, "1"},
	{"5m 1h fallback to generic write",
		`{"input_per_million":3,"cache_write_input_per_million":5}`,
		`{"input_tokens":1000,"cache_write_input_tokens":100,"cache_write_5m_input_tokens":60,"cache_write_1h_input_tokens":20,"complete":true}`, true, "1"},
	{"audio shares own rates",
		`{"input_per_million":3,"audio_input_per_million":9,"cached_audio_input_per_million":4,"output_per_million":12,"audio_output_per_million":30}`,
		`{"input_tokens":1000,"audio_input_tokens":500,"cached_audio_input_tokens":100,"output_tokens":250,"audio_output_tokens":150,"complete":true}`, true, "1"},
	{"audio fallback to text rates",
		`{"input_per_million":3,"output_per_million":12}`,
		`{"input_tokens":1000,"audio_input_tokens":500,"cached_audio_input_tokens":100,"output_tokens":250,"audio_output_tokens":150,"complete":true}`, true, "1"},
	{"image shares own rates",
		`{"input_per_million":3,"image_input_per_million":8,"cached_image_input_per_million":2,"output_per_million":12,"image_output_per_million":25}`,
		`{"input_tokens":1000,"image_input_tokens":300,"cached_image_input_tokens":50,"output_tokens":250,"image_output_tokens":100,"complete":true}`, true, "1"},
	{"cached image falls back through cached input",
		`{"input_per_million":3,"cached_input_per_million":1,"image_input_per_million":8,"output_per_million":12,"image_output_per_million":25}`,
		`{"input_tokens":1000,"image_input_tokens":300,"cached_image_input_tokens":50,"output_tokens":250,"image_output_tokens":100,"complete":true}`, true, "1"},
	{"tier below threshold",
		`{"input_per_million":3,"output_per_million":12,"tiers":[{"above_input_tokens":5000,"input_per_million":2,"output_per_million":8}]}`,
		`{"input_tokens":4000,"output_tokens":100,"complete":true}`, true, "1"},
	{"tier exactly at threshold not exceeded",
		`{"input_per_million":3,"output_per_million":12,"tiers":[{"above_input_tokens":5000,"input_per_million":2,"output_per_million":8}]}`,
		`{"input_tokens":5000,"output_tokens":100,"complete":true}`, true, "1"},
	{"tier above threshold",
		`{"input_per_million":3,"output_per_million":12,"tiers":[{"above_input_tokens":5000,"input_per_million":2,"output_per_million":8}]}`,
		`{"input_tokens":5001,"output_tokens":100,"complete":true}`, true, "1"},
	{"highest exceeded tier wins",
		`{"input_per_million":3,"output_per_million":12,"tiers":[{"above_input_tokens":5000,"input_per_million":2,"output_per_million":8},{"above_input_tokens":20000,"input_per_million":1.5,"output_per_million":6}]}`,
		`{"input_tokens":25000,"output_tokens":100,"complete":true}`, true, "1"},
	{"tier without output rate leaves output unpriced",
		`{"input_per_million":3,"output_per_million":12,"tiers":[{"above_input_tokens":5000,"input_per_million":2}]}`,
		`{"input_tokens":6000,"output_tokens":100,"complete":true}`, true, "1"},
	{"tier without output rate null output",
		`{"input_per_million":3,"tiers":[{"above_input_tokens":5000,"input_per_million":2}]}`,
		`{"input_tokens":6000,"complete":true}`, true, "1"},
	{"per_request successful",
		`{"input_per_million":3,"per_request":"0.01"}`,
		`{"input_tokens":1000,"complete":true}`, true, "1"},
	{"per_request unsuccessful",
		`{"input_per_million":3,"per_request":"0.01"}`,
		`{"input_tokens":1000,"complete":true}`, false, "1"},
	{"per_request zero",
		`{"input_per_million":3,"per_request":"0"}`,
		`{"input_tokens":1000,"complete":true}`, true, "1"},
	{"tool calls with fees",
		`{"input_per_million":3,"tool_calls":{"search":"0.01","code":"0.02"}}`,
		`{"input_tokens":1000,"tool_calls":{"search":3,"code":2},"complete":true}`, true, "1"},
	{"tool call zero fee",
		`{"input_per_million":3,"tool_calls":{"search":"0"}}`,
		`{"input_tokens":1000,"tool_calls":{"search":3},"complete":true}`, true, "1"},
	{"tool call missing fee",
		`{"input_per_million":3,"tool_calls":{"code":"0.02"}}`,
		`{"input_tokens":1000,"tool_calls":{"search":3},"complete":true}`, true, "1"},
	{"media units priced",
		`{"input_per_million":3,"unit_price":"0.5"}`,
		`{"input_tokens":1000,"media_units":2,"complete":true}`, true, "1"},
	{"media units without unit price",
		`{"input_per_million":3}`,
		`{"input_tokens":1000,"media_units":2,"complete":true}`, true, "1"},
	{"discount scales contracted only",
		`{"input_per_million":3,"output_per_million":12}`,
		`{"input_tokens":1000,"output_tokens":250,"complete":true}`, true, "0.8"},
	{"zero rate",
		`{"input_per_million":0,"output_per_million":0}`,
		`{"input_tokens":1000,"output_tokens":250,"complete":true}`, true, "1"},
	{"negative share incomplete",
		`{"input_per_million":3,"cached_input_per_million":1}`,
		`{"input_tokens":100,"cached_input_tokens":200,"complete":true}`, true, "1"},
	{"incomplete usage",
		`{"input_per_million":3}`,
		`{"input_tokens":1000,"complete":false}`, true, "1"},
	{"missing input with output priced",
		`{"output_per_million":12}`,
		`{"output_tokens":250,"complete":true}`, true, "1"},
	{"batch multiplier",
		`{"input_per_million":3,"batch_multiplier":"0.5"}`,
		`{"input_tokens":1000,"batch":true,"complete":true}`, true, "1"},
	{"batch without multiplier incomplete",
		`{"input_per_million":3}`,
		`{"input_tokens":1000,"batch":true,"complete":true}`, true, "1"},
	{"large attempt",
		`{"input_per_million":0.000000000001}`,
		`{"input_tokens":10000000000,"complete":true}`, true, "1"},
}

// Numeric equality at the caller's twelve places: the sums must land
// exactly where round(v_charge × multiplier, 12) landed.

// A weekday-morning half-price window in New York; the same instant is or
// is not inside it depending on the local day.

// list, contracted unit price

// window + batch + discount compose on the unit price

// 4 × 0.5 window × 0.5 batch = 1 list; × 0.8 = 0.8 contracted.

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}

// shares: text 10000-2000-1500=6500 @ tier 2; cached 2000 falls to
// cached_input_per_million — but a tier replaces the whole rate
// set, so it falls through to input_per_million 2; audio 1500 @6.

// scaling reprices every line and keeps a NULL NULL.
