//go:build integration

package database

import (
	"encoding/json"
	"testing"
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

// TestIntegrationPriceUsageLines asserts, for every corpus case, that
// price_usage_lines is NULL exactly where price_usage reports incomplete, and
// that a complete decomposition re-derives the exact list and cost amounts at
// the same twelve-place rounding the caller stores.
func TestIntegrationPriceUsageLines(t *testing.T) {
	pool := scratchPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	const invariant = `WITH p AS (SELECT * FROM olp.price_usage($1::jsonb,$2::jsonb,$3::timestamptz,$4,$5::numeric)),
 l AS (SELECT olp.price_usage_lines($1::jsonb,$2::jsonb,$3::timestamptz,$4,$5::numeric) AS lines)
SELECT p.complete, p.list_cost::text, p.cost::text, l.lines IS NULL AS lines_null,
 COALESCE((SELECT sum((e->>'quantity')::numeric * (e->>'list_unit_price')::numeric)
  FROM jsonb_array_elements(l.lines) e), 0) AS list_sum,
 COALESCE((SELECT sum((e->>'quantity')::numeric * (e->>'contracted_unit_price')::numeric)
  FROM jsonb_array_elements(l.lines) e), 0) AS contracted_sum,
 l.lines::text FROM p,l`
	started := "2026-10-21T14:30:00Z"
	for _, tc := range lineCases {
		var complete, linesNull bool
		var listCost, cost, listSum, contractedSum *string
		var lines *string
		if err := pool.QueryRow(t.Context(), invariant,
			tc.price, tc.usage, started, tc.successful, tc.discount).
			Scan(&complete, &listCost, &cost, &linesNull, &listSum, &contractedSum, &lines); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if linesNull != !complete {
			t.Fatalf("%s: complete=%v but lines %s", tc.name, complete, deref(lines))
		}
		if !complete {
			continue
		}
		// Numeric equality at the caller's twelve places: the sums must land
		// exactly where round(v_charge × multiplier, 12) landed.
		var listOK, contractedOK bool
		if err := pool.QueryRow(t.Context(), `SELECT round($1::numeric,12) = $2::numeric,
		    round($3::numeric,12) = $4::numeric`,
			listSum, listCost, contractedSum, cost).Scan(&listOK, &contractedOK); err != nil {
			t.Fatal(err)
		}
		if !listOK {
			t.Fatalf("%s: list %s vs %s", tc.name, deref(listSum), deref(listCost))
		}
		if !contractedOK {
			t.Fatalf("%s: contracted %s vs %s", tc.name, deref(contractedSum), deref(cost))
		}
	}
}

// Window cases vary the timestamp, so they run beside the corpus rather than
// inside it.
func TestIntegrationPriceUsageLinesWindows(t *testing.T) {
	pool := scratchPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	// A weekday-morning half-price window in New York; the same instant is or
	// is not inside it depending on the local day.
	price := `{"input_per_million":4,"windows":{"time_zone":"America/New_York",
	    "periods":[{"days":[1,2,3,4,5],"start":"00:00","end":"12:00","multiplier":"0.5"}]}}`
	usage := `{"input_tokens":1000000,"complete":true}`
	for _, tc := range []struct {
		name       string
		started    string
		wantList   string
		wantNull   bool
		wantPrices [2]string // list, contracted unit price
	}{
		{"inside window Tuesday", "2026-10-20T10:00:00Z", "2.000000000000", false, [2]string{"2", "2"}},
		{"inside window boundary noon", "2026-10-20T15:59:00Z", "2.000000000000", false, [2]string{"2", "2"}},
		{"outside window noon edge", "2026-10-20T16:00:00Z", "4.000000000000", false, [2]string{"4", "4"}},
		{"outside window Saturday", "2026-10-24T10:00:00Z", "4.000000000000", false, [2]string{"4", "4"}},
	} {
		var lines *string
		var listCost string
		if err := pool.QueryRow(t.Context(), `SELECT olp.price_usage_lines($1::jsonb,$2::jsonb,$3::timestamptz,true,1)::text,
		    (SELECT list_cost::text FROM olp.price_usage($1::jsonb,$2::jsonb,$3::timestamptz,true,1))`,
			price, usage, tc.started).Scan(&lines, &listCost); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if lines == nil || listCost != tc.wantList {
			t.Fatalf("%s: lines=%v list=%s", tc.name, lines != nil, listCost)
		}
		var parsed []map[string]any
		if err := json.Unmarshal([]byte(*lines), &parsed); err != nil || len(parsed) != 1 {
			t.Fatalf("%s: %v", tc.name, *lines)
		}
		if parsed[0]["list_unit_price"] != tc.wantPrices[0] {
			t.Fatalf("%s: unit price %v want %s", tc.name, parsed[0], tc.wantPrices[0])
		}
	}
	// window + batch + discount compose on the unit price
	var lines *string
	if err := pool.QueryRow(t.Context(), `SELECT olp.price_usage_lines($1::jsonb,$2::jsonb,$3::timestamptz,true,'0.8')::text`,
		`{"input_per_million":4,"batch_multiplier":"0.5","windows":{"time_zone":"America/New_York",
		  "periods":[{"days":[1,2,3,4,5],"start":"00:00","end":"12:00","multiplier":"0.5"}]}}`,
		`{"input_tokens":1000000,"batch":true,"complete":true}`,
		"2026-10-20T10:00:00Z").Scan(&lines); err != nil {
		t.Fatal(err)
	}
	// 4 × 0.5 window × 0.5 batch = 1 list; × 0.8 = 0.8 contracted.
	want := `[{"component": "input_per_million", "tier": null, "unit": "1000000 Tokens", "quantity": "1", "list_unit_price": "1", "contracted_unit_price": "0.8"}]`
	var got, expected any
	if err := json.Unmarshal([]byte(*lines), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, expected) {
		t.Fatalf("lines %s want %s", *lines, want)
	}
}

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

// TestIntegrationPriceLinesGolden pins the exact documents for three
// representative compositions: modality fallback under a tier, per-request +
// tools + media, and a commitment-shaped scaling.
func TestIntegrationPriceLinesGolden(t *testing.T) {
	pool := scratchPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		price      string
		usage      string
		successful bool
		discount   string
		want       string
	}{
		{"modality fallback under a tier",
			`{"input_per_million":3,"cached_input_per_million":1,"audio_input_per_million":9,
			  "tiers":[{"above_input_tokens":5000,"input_per_million":2,"audio_input_per_million":6}]}`,
			`{"input_tokens":10000,"cached_input_tokens":2000,"audio_input_tokens":1500,"complete":true}`,
			true, "1",
			// shares: text 10000-2000-1500=6500 @ tier 2; cached 2000 falls to
			// cached_input_per_million — but a tier replaces the whole rate
			// set, so it falls through to input_per_million 2; audio 1500 @6.
			`[{"component":"audio_input_per_million","tier":"5000","unit":"1000000 Tokens","quantity":"0.0015","list_unit_price":"6","contracted_unit_price":"6"},
			  {"component":"input_per_million","tier":"5000","unit":"1000000 Tokens","quantity":"0.0085","list_unit_price":"2","contracted_unit_price":"2"}]`},
		{"per_request tools and media",
			`{"input_per_million":3,"per_request":"0.01","unit_price":"0.5","tool_calls":{"search":"0.02"}}`,
			`{"input_tokens":1000,"media_units":4,"tool_calls":{"search":3},"complete":true}`,
			true, "1",
			`[{"component":"input_per_million","tier":null,"unit":"1000000 Tokens","quantity":"0.001","list_unit_price":"3","contracted_unit_price":"3"},
			  {"component":"per_request","tier":null,"unit":"Requests","quantity":"1","list_unit_price":"0.01","contracted_unit_price":"0.01"},
			  {"component":"tool_calls.search","tier":null,"unit":"Calls","quantity":"3","list_unit_price":"0.02","contracted_unit_price":"0.02"},
			  {"component":"unit_price","tier":null,"unit":"Units","quantity":"4","list_unit_price":"0.5","contracted_unit_price":"0.5"}]`},
	} {
		var lines *string
		if err := pool.QueryRow(t.Context(), `SELECT olp.price_usage_lines($1::jsonb,$2::jsonb,'2026-10-21T14:30:00Z',$3,$4::numeric)::text`,
			tc.price, tc.usage, tc.successful, tc.discount).Scan(&lines); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if lines == nil {
			t.Fatalf("%s: NULL lines", tc.name)
		}
		var got, expected any
		if err := json.Unmarshal([]byte(*lines), &got); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := json.Unmarshal([]byte(tc.want), &expected); err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(got, expected) {
			t.Fatalf("%s:\n%s\nwant:\n%s", tc.name, *lines, tc.want)
		}
	}

	// scaling reprices every line and keeps a NULL NULL.
	var scaled *string
	if err := pool.QueryRow(t.Context(), `SELECT olp.scale_price_lines(
	    '[{"component":"input_per_million","tier":null,"unit":"1000000 Tokens","quantity":"0.001","list_unit_price":"3","contracted_unit_price":"2.4"},
	      {"component":"per_request","tier":null,"unit":"Requests","quantity":"1","list_unit_price":"0.01","contracted_unit_price":"0.01"}]'::jsonb, 1.5)::text`).Scan(&scaled); err != nil {
		t.Fatal(err)
	}
	want := `[{"component":"input_per_million","tier":null,"unit":"1000000 Tokens","quantity":"0.001","list_unit_price":"3.6","contracted_unit_price":"3.6"},
	         {"component":"per_request","tier":null,"unit":"Requests","quantity":"1","list_unit_price":"0.015","contracted_unit_price":"0.015"}]`
	var got, expected any
	_ = json.Unmarshal([]byte(*scaled), &got)
	_ = json.Unmarshal([]byte(want), &expected)
	if !jsonEqual(got, expected) {
		t.Fatalf("scaled %s want %s", *scaled, want)
	}
	var nulled *string
	if err := pool.QueryRow(t.Context(), `SELECT olp.scale_price_lines(NULL::jsonb, 1.5)::text,
	    olp.scale_price_lines('[]'::jsonb, NULL)::text`).Scan(&nulled, new(*string)); err != nil {
		t.Fatal(err)
	}
	if nulled != nil {
		t.Fatalf("scale_price_lines(NULL) = %s", *nulled)
	}
}
