package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/tests/fixtures"
)

func businessExposition(m *telemetry.BusinessMetrics) string {
	var body strings.Builder
	m.WritePrometheus(&body)
	return body.String()
}

func priceInputs() *usage.RoutingInputs {
	rate := func(v string) *string { return &v }
	currency := "usd"
	return &usage.RoutingInputs{Prices: []usage.RoutingPrice{
		{Price: usage.Price{ProviderKind: "openai_compatible", Model: modelA, Operation: "generation", InputPerMillion: rate("1"), OutputPerMillion: rate("2"), Currency: currency}, EffectiveAt: time.Now()},
	}, RefreshedAt: time.Now()}
}

func TestBusinessMetricsUnaryRecordsTokensAndCost(t *testing.T) {
	h := newHarness(t, Config{})
	h.rt.inputs = priceInputs()
	h.gateway.Business = telemetry.NewBusinessMetrics(5000, nil)
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
	h.sink.await(t, 1)
	out := businessExposition(h.gateway.Business)
	if !strings.Contains(out, `olp_tokens_total{route="team-chat",provider_kind="openai_compatible",direction="input"} 2`) {
		t.Fatalf("no input token series:\n%s", out)
	}
	if !strings.Contains(out, `olp_tokens_total{route="team-chat",provider_kind="openai_compatible",direction="output"} 3`) {
		t.Fatalf("no output token series:\n%s", out)
	}
	if !strings.Contains(out, `olp_cost_total{route="team-chat",currency="usd"} 8e-06`) {
		t.Fatalf("no priced cost series:\n%s", out)
	}
	if strings.Contains(out, "olp_time_to_first_token_seconds_count{") || strings.Contains(out, "olp_output_tokens_per_second_count{") {
		t.Fatalf("unary request emitted streaming metrics:\n%s", out)
	}
	if strings.Contains(out, "project=") || strings.Contains(out, "end_user=") || strings.Contains(out, `key="`) {
		t.Fatalf("tenant labels must be absent by default:\n%s", out)
	}
}

func TestBusinessMetricsStreamRecordsTokensRateAndTTFT(t *testing.T) {
	h := newHarness(t, Config{})
	h.gateway.Business = telemetry.NewBusinessMetrics(5000, nil)
	data, err := fixtures.Files.ReadFile("streams/openai-chat.sse")
	if err != nil {
		t.Fatal(err)
	}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if err := testutil.Stream(w, r, data, 5, 0); err != nil {
			t.Error(err)
		}
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	h.sink.await(t, 1)
	out := businessExposition(h.gateway.Business)
	if !strings.Contains(out, `olp_tokens_total{route="team-chat",provider_kind="openai_compatible",direction="input"} 3`) {
		t.Fatalf("no streamed input tokens:\n%s", out)
	}
	if !strings.Contains(out, `olp_tokens_total{route="team-chat",provider_kind="openai_compatible",direction="output"} 2`) {
		t.Fatalf("no streamed output tokens:\n%s", out)
	}
	if !strings.Contains(out, "olp_time_to_first_token_seconds_count{") {
		t.Fatalf("no TTFT series:\n%s", out)
	}
	if !strings.Contains(out, "olp_output_tokens_per_second_count{") {
		t.Fatalf("no output rate series:\n%s", out)
	}
}

func TestBusinessMetricsWithoutObservedUsageEmitsNone(t *testing.T) {
	h := newHarness(t, Config{})
	h.gateway.Business = telemetry.NewBusinessMetrics(5000, nil)
	h.mock.set("a", completionUsageMissing())
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
	h.sink.await(t, 1)
	out := businessExposition(h.gateway.Business)
	if strings.Contains(out, "olp_tokens_total{") || strings.Contains(out, "olp_cost_total{") || strings.Contains(out, "olp_output_tokens_per_second_bucket{") {
		t.Fatalf("unpriced unmeasured request emitted business series:\n%s", out)
	}
}

func completionUsageMissing() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"c","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}
}

func TestBusinessMetricsTenantLabels(t *testing.T) {
	h := newHarness(t, Config{})
	h.gateway.Business = telemetry.NewBusinessMetrics(5000, []string{"project", "key", "end_user"})
	project := uuid.NewString()
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	route.ProjectID = &project
	snapshot.Routes[routeSlug] = route
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &project
	h.rt.keys[fullKey] = authority
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
	h.sink.await(t, 1)
	out := businessExposition(h.gateway.Business)
	if !strings.Contains(out, fmt.Sprintf(`project="%s"`, project)) {
		t.Fatalf("no project tenant label:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf(`key="%s"`, h.keyID)) {
		t.Fatalf("no key tenant label:\n%s", out)
	}
	if !strings.Contains(out, `end_user=""`) {
		t.Fatalf("no end_user tenant label:\n%s", out)
	}
}

func TestBusinessMetricsCallerPaidBudgetExemptCosts(t *testing.T) {
	h := newHarness(t, Config{})
	h.rt.inputs = priceInputs()
	h.gateway.Business = telemetry.NewBusinessMetrics(5000, nil)
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
	env := h.sink.await(t, 1)[0]
	env.Attempts[0].BudgetExempt = true
	business := telemetry.NewBusinessMetrics(5000, nil)
	h.gateway.Business = business
	h.gateway.recordBusiness(env, &execution{request: request{startedAt: env.StartedAt}})
	out := businessExposition(business)
	if !strings.Contains(out, `olp_cost_total{route="team-chat",currency="usd"} 8e-06`) {
		t.Fatalf("budget-exempt priced usage emitted no cost:\n%s", out)
	}
}

func TestBusinessMetricsIdentityFloodBounded(t *testing.T) {
	m := telemetry.NewBusinessMetrics(200, nil)
	for i := range 100000 {
		input, output := int64(1), int64(1)
		m.Tokens(telemetry.BusinessLabels{Route: fmt.Sprintf("route-%d", i), ProviderKind: "openai"}, &input, &output)
	}
	out := businessExposition(m)
	if !strings.Contains(out, "olp_metrics_series_active 200\n") || !strings.Contains(out, "olp_metrics_series_overflow_total 199800\n") {
		t.Fatalf("series admission or overflow count is incorrect:\n%s", out)
	}
	if strings.Count(out, "olp_tokens_total{") != 200 {
		t.Fatalf("series were not capped:\n%d", strings.Count(out, "olp_tokens_total{"))
	}
}
