package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestGeminiStreamingAccountingPreservesPartialEvidenceAndScopedAllowances(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates int64
		candidate  string
		usage      string
		complete   bool
		observed   bool
		actual     int64
	}{
		{"interrupted intermediate", 1, `[{"index":0,"content":{"role":"model","parts":[{"text":"partial"}]}}]`, `{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}`, false, true, 3},
		{"interrupted above estimate", 1, `[{"index":0,"content":{"role":"model","parts":[{"text":"partial"}]}}]`, `{"promptTokenCount":200,"candidatesTokenCount":100,"totalTokenCount":300}`, false, true, 300},
		{"unseen requested candidate", 2, `[{"index":0,"finishReason":"STOP","content":{"role":"model","parts":[{"text":"first"}]}}]`, `{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}`, false, true, 3},
		{"terminal totals", 1, `[{"index":0,"finishReason":"STOP","content":{"role":"model","parts":[{"text":"done"}]}}]`, `{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}`, true, true, 3},
		{"literal zero", 1, `[{"index":0,"finishReason":"STOP","content":{"role":"model","parts":[]}}]`, `{"promptTokenCount":0,"candidatesTokenCount":0,"totalTokenCount":0}`, true, true, 0},
		{"missing counts", 1, `[{"index":0,"finishReason":"STOP","content":{"role":"model","parts":[{"text":"done"}]}}]`, `null`, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHeaderFixture(t)
			for index := range f.rt.inputs.Prices {
				f.rt.inputs.Prices[index].ProviderKind = "gemini"
			}
			for id, provider := range f.rt.release.Snapshot.Providers {
				provider.Kind = "gemini"
				provider.Limits = &runtime.Limits{TokensPerMinute: perMinute(100000)}
				provider.ParameterDefaults = map[string]json.RawMessage{"generationConfig": json.RawMessage(fmt.Sprintf(`{"candidateCount":%d}`, tc.candidates))}
				f.rt.release.Snapshot.Providers[id] = provider
			}
			f.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, `data: {"candidates":`+tc.candidate+`,"usageMetadata":`+tc.usage+"}\n\n")
			})
			key := f.key(access.KeyPolicy{TokensPerMinute: perMinute(100000)})
			f.send("gemini", key, true)
			reconciliations := 1
			if tc.observed {
				reconciliations++
			}
			f.window.settled(t, reconciliations)
			env := f.sink.last(t)
			if len(env.Attempts) != 1 {
				t.Fatalf("unexpected retry of accepted generation: %+v", env)
			}
			fact := env.Attempts[0]
			if fact.UsageObserved != tc.observed || fact.UsageComplete != tc.complete || fact.BillingUncertain == tc.complete {
				t.Fatalf("native accounting completeness lost: %+v", fact)
			}
			if tc.observed && (fact.Usage == nil || fact.Usage.TotalTokens != tc.actual) {
				t.Fatalf("observed counters lost: %+v", fact.Usage)
			}
			if fact.Price == nil {
				t.Fatal("fixture lost its native pricing scope")
			}
			if _, priced := fact.Price.Cost(attemptUsage(&fact, env.Operation), fact.StartedAt, attemptSuccessful(&fact), nil); priced != tc.complete {
				t.Fatalf("native billing completeness lost: priced=%v complete=%v", priced, tc.complete)
			}
			f.window.mu.Lock()
			defer f.window.mu.Unlock()
			var reserved, held int64
			for _, tokens := range f.window.reserved {
				reserved += tokens
			}
			for _, counters := range f.window.counters {
				held += counters.tokens
			}
			// Rate allowances preserve admission estimates and can increase for
			// reported work. They do not refund estimates from native counters.
			if floor := max(reserved, tc.actual*int64(len(f.window.reserved))); reserved == 0 || held < floor {
				t.Fatalf("scoped allowance lost: held=%d reserved=%d observed=%d floor=%d", held, reserved, tc.actual, floor)
			}
		})
	}
}
