//go:build bench

package loadgen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
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
