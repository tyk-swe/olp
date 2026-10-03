//go:build bench

package loadgen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestReportJSON(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(mock(t))
		cfg.Rate, cfg.StreamShare, cfg.APIKey, cfg.URL = 40, 0.5, "super-secret", "http://user:password@gateway"
		report := run(t, cfg)
		report.Name = "s2"
		path := filepath.Join(t.TempDir(), "report.json")
		if err := report.WriteJSON(path); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "super-secret") || strings.Contains(string(data), "password") {
			t.Fatalf("the report carries a credential:\n%s", data)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		get := func(keys ...string) any {
			var v any = doc
			for _, k := range keys {
				m, ok := v.(map[string]any)
				if !ok {
					t.Fatalf("%v: %v is not an object", keys, v)
				}
				if v, ok = m[k]; !ok {
					t.Fatalf("the report has no %v", keys)
				}
			}
			return v
		}
		for _, path := range [][]string{
			{"rates", "target_rps"}, {"rates", "offered_rps"}, {"rates", "throughput_rps"}, {"rates", "ratio"}, {"rates", "success_rate"}, {"rates", "error_rate"},
			{"requests", "scheduled"}, {"requests", "sent"}, {"requests", "dropped"}, {"requests", "late"}, {"requests", "warmup_sent"}, {"requests", "peak_in_flight"},
			{"errors", "by_kind"}, {"errors", "status_codes", "200"},
			{"latency", "all", "p50_ms"}, {"latency", "all", "p90_ms"}, {"latency", "all", "p95_ms"}, {"latency", "all", "p99_ms"}, {"latency", "all", "p99_9_ms"}, {"latency", "all", "max_ms"},
			{"latency", "ttft", "p99_ms"}, {"latency", "unary", "count"}, {"latency", "stream", "count"}, {"latency", "service", "p99_ms"}, {"latency", "send_lag", "max_ms"},
			{"config", "rate"}, {"config", "prompt_tokens"}, {"valid"}, {"started_at"}, {"elapsed_seconds"}, {"name"},
		} {
			get(path...)
		}
		if get("name") != "s2" || get("config", "url") != "http://gateway" || get("valid") != true {
			t.Fatalf("%v %v %v", get("name"), get("config", "url"), get("valid"))
		}
		if p99 := get("latency", "ttft", "p99_ms").(float64); p99 < 49 || p99 > 51 {
			t.Fatalf("ttft p99 %v ms", p99)
		}
		// The file round-trips.
		var back Report
		if err := json.Unmarshal(data, &back); err != nil || back.Requests != report.Requests || back.Latency.All != report.Latency.All {
			t.Fatalf("%v", err)
		}
	})
}

func TestReportText(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(serve(95 * time.Millisecond))
		cfg.MaxInFlight, cfg.MaxBacklog = 2, -1
		text := run(t, cfg).Text()
		for _, want := range []string{"dropped 80", "INVALID", "p99", "send lag"} {
			if !strings.Contains(text, want) {
				t.Errorf("the summary lacks %q:\n%s", want, text)
			}
		}
		if text := run(t, base(serve(0))).Text(); !strings.Contains(text, "valid run") || strings.Contains(text, "INVALID") {
			t.Errorf("a valid run reads as invalid:\n%s", text)
		}
	})
}

func TestCompareIsTheDifferenceFromTheBaseline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		direct := base(mock(t))
		direct.Rate, direct.StreamShare = 40, 0.5
		through := base(delayed(mock(t), 4*time.Millisecond))
		through.Rate, through.StreamShare = 40, 0.5
		baseline, gateway := run(t, direct), run(t, through)
		o := Compare(baseline, gateway)
		for name, d := range map[string]Delta{"unary": o.Unary, "stream": o.Stream, "ttft": o.TTFT} {
			if !d.Present {
				t.Fatalf("%s is absent", name)
			}
			within(t, name+" p50", d.P50Ms, 4, 0.1)
			within(t, name+" p95", d.P95Ms, 4, 0.1)
			within(t, name+" p99", d.P99Ms, 4, 0.1)
		}
		// With no streams on one side there is nothing to subtract.
		unary := base(mock(t))
		if o := Compare(run(t, unary), gateway); o.Stream.Present || o.TTFT.Present || !o.Unary.Present {
			t.Fatalf("%+v", o)
		}
	})
}

// TestSummaryPercentilesOfAKnownDistribution pins each percentile to the
// quantile it is named for: the figures a target is judged on are these, and a
// summary that took p95 from the 90th would still look plausible.
func TestSummaryPercentilesOfAKnownDistribution(t *testing.T) {
	rec := newRecorder(time.Second)
	for i := 1; i <= 100; i++ {
		rec.observe(rec.all, time.Duration(i)*time.Millisecond)
	}
	s := summarize(rec.all)
	if s.Count != 100 {
		t.Fatalf("%d samples", s.Count)
	}
	for name, v := range map[string][2]float64{
		"min": {s.MinMs, 1}, "mean": {s.MeanMs, 50.5}, "p50": {s.P50Ms, 50}, "p90": {s.P90Ms, 90}, "p95": {s.P95Ms, 95},
		"p99": {s.P99Ms, 99}, "p99.9": {s.P999Ms, 100}, "max": {s.MaxMs, 100},
	} {
		near(t, name, v[0], v[1])
	}
	if got := summarize(newHistogram()); got != (Summary{}) {
		t.Errorf("an empty histogram summarizes to %+v", got)
	}
}

// TestValidityThresholds pins the two limits that decide whether a run
// delivered its schedule: at most 1% of sends late, and at least 99% of the
// target rate offered. Each is tried on both sides of its limit.
func TestValidityThresholds(t *testing.T) {
	cfg, err := Config{URL: "http://x", Model: "m", Rate: 100, Duration: time.Second}.normalize()
	if err != nil {
		t.Fatal(err)
	}
	// A run of 100 requests, due at 10 ms intervals, of which late were sent
	// late and the last left at lastSend.
	build := func(late int64, lastSend time.Duration) *Report {
		rec := newRecorder(cfg.Duration)
		rec.counts.Scheduled, rec.counts.Sent, rec.counts.Succeeded, rec.counts.Late = 100, 100, 100, late
		rec.lastSend = lastSend
		return rec.report(cfg, time.Now(), time.Second, false, 0, 0)
	}
	problems := func(r *Report) string { return strings.Join(r.Problems, "; ") }

	if r := build(1, 990*time.Millisecond); !r.Valid {
		t.Errorf("1 late send in 100 is 1%%, which the limit allows: %v", r.Problems)
	}
	if r := build(2, 990*time.Millisecond); r.Valid || !strings.Contains(problems(r), "2 of 100 requests were sent more than") {
		t.Errorf("2 late sends in 100 is above the limit: valid %v, %v", r.Valid, r.Problems)
	}
	// Offered is the sends over the span from the start to the last send plus
	// an interval: 100 over 1.005 s is 99.5% of the rate, and over 1.0152 s
	// it is 98.5%.
	if r := build(0, 995*time.Millisecond); !r.Valid || r.Rates.Ratio < 0.994 || r.Rates.Ratio > 0.996 {
		t.Errorf("99.5%% of the rate is within the limit: valid %v, ratio %.4f, %v", r.Valid, r.Rates.Ratio, r.Problems)
	}
	if r := build(0, 1005*time.Millisecond); r.Valid || !strings.Contains(problems(r), "of the 100.0 target") || r.Rates.Ratio < 0.984 || r.Rates.Ratio > 0.986 {
		t.Errorf("98.5%% of the rate is below the limit: valid %v, ratio %.4f, %v", r.Valid, r.Rates.Ratio, r.Problems)
	}
}
