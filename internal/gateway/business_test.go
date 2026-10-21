package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/internal/usage"
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
	h.gateway.recordBusiness(env, &call{request: request{startedAt: env.StartedAt}})
	out := businessExposition(business)
	if !strings.Contains(out, `olp_cost_total{route="team-chat",currency="usd"} 8e-06`) {
		t.Fatalf("budget-exempt priced usage emitted no cost:\n%s", out)
	}
}
