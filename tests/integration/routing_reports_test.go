//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

// repDecimal reads a report's exact decimal.
func repDecimal(t *testing.T, value any) *big.Rat {
	t.Helper()
	text, _ := value.(string)
	rat, ok := new(big.Rat).SetString(text)
	if !ok {
		t.Fatalf("decimal %v", value)
	}
	return rat
}

func repSame(t *testing.T, label string, got any, want string) {
	t.Helper()
	if repDecimal(t, got).Cmp(repDecimal(t, want)) != 0 {
		t.Fatalf("%s = %v, want %s", label, got, want)
	}
}

// repSelected seeds one priced caller attempt that a selector chose,
// optionally with the cost of its usage on the target the selector avoided.
func (f *repFixture) repSelected(started time.Time, selector *string, cost string, baseline *string) {
	id := access.NewID()
	f.request(repRequest{ID: id, StartedAt: started, Route: "tiered", Operation: "generation", Surface: "openai", StatusCode: repInt(200), AttemptCount: 1})
	f.fact(repFact{RequestID: id, StartedAt: started, Ordinal: 1, ObservedAt: started.Add(time.Second), Route: "tiered",
		ProviderID: f.P1, Model: "small", Operation: "generation", Surface: "openai", Charge: "billable",
		Observed: true, Complete: true, Input: repCount(10), Output: repCount(5), Cost: &cost, Currency: repText("USD"), CountRequest: true})
	f.exec(`UPDATE olp.attempt_usage_facts SET selector = $2, baseline_cost = $3::numeric WHERE request_id = $1`, id, selector, baseline)
}

// Usage reports compare each selector with the most expensive target it
// avoided, counting only attempts that both prices describe.
func TestSelectorSavingsCompareOnlyPricedAttempts(t *testing.T) {
	f := repSetup(t)
	at := f.Base.Add(time.Hour)
	f.repSelected(at, repText("short"), "0.002", repText("0.010"))
	f.repSelected(at.Add(time.Minute), repText("short"), "0.004", nil)
	f.repSelected(at.Add(2*time.Minute), nil, "0.009", nil)

	report, err := usage.ReadSelectorSavings(context.Background(), f.pool, usage.Filters{Start: f.Base, End: f.Base.Add(3 * time.Hour), AllProjects: true})
	if err != nil || len(report.Items) != 1 {
		t.Fatalf("report %+v, %v", report, err)
	}
	item := report.Items[0]
	if item.Route != "tiered" || item.Selector != "short" || item.Attempts != 2 || item.ComparedAttempts != 1 {
		t.Fatalf("selector savings %+v", item)
	}
	repSame(t, "estimated_cost", item.Cost, "0.002")
	repSame(t, "baseline_cost", item.BaselineCost, "0.010")
	repSame(t, "savings", item.Savings, "0.008")

	query := url.Values{"start": {f.Base.Format(time.RFC3339)}, "end": {f.Base.Add(3 * time.Hour).Format(time.RFC3339)}, "route": {"tiered"}}
	body := f.h.want(f.Owner, http.MethodGet, "/api/v1/usage/selector-savings?"+query.Encode(), nil, nil, 200)
	if items, _ := body["items"].([]any); len(items) != 1 || body["currency"] != "USD" {
		t.Fatalf("selector savings over HTTP %v", body)
	}
}

// A shadow experiment pairs each shadow request with the caller's request it
// mirrored, comparing status, latency, first byte, tokens and cost.
func TestShadowExperimentsPairEachMirrorWithItsPrimary(t *testing.T) {
	f := repSetup(t)
	at := f.Base.Add(time.Hour)
	primary, shadow := access.NewID(), access.NewID()
	f.request(repRequest{ID: primary, StartedAt: at, Route: "tiered", Operation: "generation", Surface: "openai", StatusCode: repInt(200), AttemptCount: 1, Latency: repInt(100), FirstByte: repInt(40)})
	f.fact(repFact{RequestID: primary, StartedAt: at, Ordinal: 1, ObservedAt: at.Add(time.Second), Route: "tiered",
		ProviderID: f.P1, Model: "live", Operation: "generation", Surface: "openai", Charge: "billable",
		Observed: true, Complete: true, Input: repCount(10), Output: repCount(5), Cost: repText("0.003"), Currency: repText("USD"), CountRequest: true})
	mirrored := at.Add(2 * time.Second)
	f.request(repRequest{ID: shadow, StartedAt: mirrored, Route: "tiered", Operation: "generation", Surface: "openai", StatusCode: repInt(500), ErrorClass: repText("upstream_server"), AttemptCount: 1, Latency: repInt(300)})
	f.exec(`UPDATE olp.requests SET api_key_id = NULL, origin = 'shadow', parent_request_id = $2 WHERE id = $1`, shadow, primary)
	f.fact(repFact{RequestID: shadow, StartedAt: mirrored, Ordinal: 1, ObservedAt: mirrored.Add(time.Second), Route: "tiered",
		ProviderID: f.P2, Model: "candidate", Operation: "generation", Surface: "openai", Charge: "billable",
		Observed: true, Complete: true, Input: repCount(10), Output: repCount(7), Cost: repText("0.001"), Currency: repText("USD"), CountRequest: true})
	f.exec(`UPDATE olp.attempt_usage_facts SET api_key_id = NULL WHERE request_id = $1`, shadow)

	report, err := usage.ReadExperiments(context.Background(), f.pool, usage.Filters{Start: f.Base, End: f.Base.Add(3 * time.Hour), AllProjects: true})
	if err != nil || len(report.Items) != 1 {
		t.Fatalf("report %+v, %v", report, err)
	}
	item := report.Items[0]
	p, s := item.Primary, item.Shadow
	if item.Route != "tiered" || item.ProviderID != f.P2 || item.UpstreamModel != "candidate" || item.Pairs != 1 {
		t.Fatalf("experiment %+v", item)
	}
	if p.Successes != 1 || *p.MeanLatencyMS != 100 || *p.MeanFirstByteMS != 40 || p.InputTokens != "10" || p.OutputTokens != "5" {
		t.Fatalf("primary side %+v", p)
	}
	if s.Successes != 0 || *s.MeanLatencyMS != 300 || s.MeanFirstByteMS != nil || s.OutputTokens != "7" || s.UnpricedRequests != 0 {
		t.Fatalf("shadow side %+v", s)
	}
	repSame(t, "primary cost", *p.EstimatedCost, "0.003")
	repSame(t, "shadow cost", *s.EstimatedCost, "0.001")

	// A viewer confined to a project sees no experiment on a route outside it.
	scoped, err := usage.ReadExperiments(context.Background(), f.pool, usage.Filters{Start: f.Base, End: f.Base.Add(3 * time.Hour), AllowedProjects: []string{access.NewID()}})
	if err != nil || len(scoped.Items) != 0 {
		t.Fatalf("scoped report %+v, %v", scoped, err)
	}
	query := url.Values{"start": {f.Base.Format(time.RFC3339)}, "end": {f.Base.Add(3 * time.Hour).Format(time.RFC3339)}}
	body := f.h.want(f.Owner, http.MethodGet, "/api/v1/usage/shadow-experiments?"+query.Encode(), nil, nil, 200)
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("experiments over HTTP %v", body)
	}
}
