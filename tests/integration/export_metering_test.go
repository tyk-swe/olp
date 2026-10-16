//go:build integration

package integration_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// postSink is the create-sink request idiom for this file.
func postSink(h *accessHarness, owner *browser, body map[string]any, want int) map[string]any {
	return h.want(owner, "POST", "/api/v1/observability/sinks", body,
		map[string]string{"Idempotency-Key": uuid.NewString()}, want)
}

func TestMeteringSinkManagement(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Metering")
	other := createProject(h, owner, "Metering other")

	// The pairing matrix: cloudevents carries only metering, csv_gzip only
	// focus, and focus never appears on another encoding.
	postSink(h, owner, map[string]any{"name": "ce-requests", "type": "https",
		"destination": "https://events.example.test", "streams": []string{"requests"}, "format": "cloudevents"}, 422)
	postSink(h, owner, map[string]any{"name": "focus-json", "type": "s3",
		"destination": "https://objects.example.test/bucket", "streams": []string{"focus"}, "format": "jsonl"}, 422)
	postSink(h, owner, map[string]any{"name": "csv-requests", "type": "s3",
		"destination": "https://objects.example.test/bucket", "streams": []string{"requests"}, "format": "csv_gzip"}, 422)
	postSink(h, owner, map[string]any{"name": "stripe-requests", "type": "stripe_meter",
		"destination": "https://api.stripe.com", "streams": []string{"requests"}, "format": "stripe",
		"credential": map[string]any{"api_key": "rk_test_0123456789"}, "settings": map[string]any{
			"event_name": "olp.tokens", "customers": map[string]any{project: "cus_a"}}}, 422)

	// Focus sinks reject an outcome filter but accept project and route scope.
	postSink(h, owner, map[string]any{"name": "focus-outcome", "type": "s3",
		"destination": "https://objects.example.test/bucket", "streams": []string{"focus"}, "format": "csv_gzip",
		"filter": map[string]any{"outcome": "success"}}, 422)
	focus := postSink(h, owner, map[string]any{"name": "focus", "type": "s3",
		"destination": "https://objects.example.test/bucket", "streams": []string{"focus"}, "format": "csv_gzip",
		"settings": map[string]any{"cost_basis": "charge", "start_date": time.Now().UTC().Format("2006-01-02")}}, 201)
	focusSettings := focus["settings"].(map[string]any)
	if focusSettings["cost_basis"] != "charge" {
		t.Fatalf("focus settings %v", focusSettings)
	}

	// The datasets ledger endpoint sits next to the sink detail: a fresh sink
	// reports an empty page, and its focus stream status reads zero.
	datasets := h.want(owner, "GET", "/api/v1/observability/sinks/"+focus["export_sink_id"].(string)+"/focus-datasets", nil, nil, 200)
	if datasets["items"] == nil || len(datasets["items"].([]any)) != 0 {
		t.Fatalf("focus-datasets for a fresh sink: %v", datasets)
	}
	var focusStream map[string]any
	for _, st := range focus["status"].([]any) {
		if st.(map[string]any)["stream"] == "focus" {
			focusStream = st.(map[string]any)
		}
	}
	if focusStream == nil || focusStream["delivered_days"] != float64(0) {
		t.Fatalf("focus stream status: %v", focus["status"])
	}

	// A cloudevents sink on the metering stream is accepted and its resolved
	// settings are reported back.
	ce := postSink(h, owner, map[string]any{"name": "cloudevents", "type": "https",
		"destination": "https://events.example.test/metering", "streams": []string{"metering"},
		"format": "cloudevents", "settings": map[string]any{"source": "https://billing.example.test/olp"}}, 201)
	if ce["settings"].(map[string]any)["source"] != "https://billing.example.test/olp" {
		t.Fatalf("source setting %v", ce["settings"])
	}

	// Stripe needs an api_key credential on an enabled sink, and nothing else.
	postSink(h, owner, map[string]any{"name": "stripe-nocred", "type": "stripe_meter",
		"destination": "https://api.stripe.com", "streams": []string{"metering"}, "format": "stripe",
		"settings": map[string]any{"event_name": "olp.tokens", "customers": map[string]any{project: "cus_a"}}}, 422)
	postSink(h, owner, map[string]any{"name": "stripe-badkey", "type": "stripe_meter",
		"destination": "https://api.stripe.com", "streams": []string{"metering"}, "format": "stripe",
		"credential": map[string]any{"api_key": "pk_live_notallowed"}, "settings": map[string]any{
			"event_name": "olp.tokens", "customers": map[string]any{project: "cus_a"}}}, 422)
	postSink(h, owner, map[string]any{"name": "stripe-two-keys", "type": "stripe_meter",
		"destination": "https://api.stripe.com", "streams": []string{"metering"}, "format": "stripe",
		"credential": map[string]any{"api_key": "rk_test_0123456789", "signing_secret": "0123456789abcdef0123456789abcdef"},
		"settings":   map[string]any{"event_name": "olp.tokens", "customers": map[string]any{project: "cus_a"}}}, 422)
	postSink(h, owner, map[string]any{"name": "https-api-key", "type": "https",
		"destination": "https://events.example.test", "streams": []string{"requests"}, "format": "json",
		"credential": map[string]any{"api_key": "sk_test_0123456789"}}, 422)

	stripe := postSink(h, owner, map[string]any{"name": "stripe", "type": "stripe_meter",
		"destination": "https://api.stripe.com", "streams": []string{"metering"}, "format": "stripe",
		"credential": map[string]any{"api_key": "rk_test_0123456789"},
		"settings":   map[string]any{"event_name": "olp.tokens", "customers": map[string]any{project: "cus_ABC123"}}}, 201)
	stripeSettings := stripe["settings"].(map[string]any)
	// Resolved defaults are persisted so readers never guess.
	if stripeSettings["value"] != "charge" || stripeSettings["customer_key"] != "stripe_customer_id" ||
		stripeSettings["value_key"] != "value" {
		t.Fatalf("stripe settings %v", stripeSettings)
	}
	if stripe["credential"] != nil || strings.Contains(fmt.Sprint(stripe), "rk_test_0123456789") {
		t.Fatalf("credential material visible in response: %v", stripe)
	}

	// A project-scoped Stripe sink maps only its own project.
	postSink(h, owner, map[string]any{"name": "stripe-foreign", "type": "stripe_meter", "project_id": project,
		"destination": "https://api.stripe.com", "streams": []string{"metering"}, "format": "stripe",
		"credential": map[string]any{"api_key": "rk_test_0123456789"},
		"settings":   map[string]any{"event_name": "olp.tokens", "customers": map[string]any{other: "cus_x"}}}, 422)
	postSink(h, owner, map[string]any{"name": "stripe-self", "type": "stripe_meter", "project_id": project,
		"destination": "https://api.stripe.com", "streams": []string{"metering"}, "format": "stripe",
		"credential": map[string]any{"api_key": "rk_test_0123456789"},
		"settings":   map[string]any{"event_name": "olp.tokens", "customers": map[string]any{project: "cus_self"}}}, 201)

	// cost_basis is immutable; other settings fields may be repatched.
	focusID := focus["export_sink_id"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+focusID,
		map[string]any{"settings": map[string]any{"cost_basis": "cost"}}, etagHeader(focus), 422)
	updated := h.want(owner, "PATCH", "/api/v1/observability/sinks/"+focusID,
		map[string]any{"settings": map[string]any{"cost_basis": "charge"}}, etagHeader(focus), 200)
	if updated["settings"].(map[string]any)["cost_basis"] != "charge" {
		t.Fatalf("patched settings %v", updated["settings"])
	}

	// Removing the credential from an enabled Stripe sink is refused.
	stripeID := stripe["export_sink_id"].(string)
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+stripeID,
		map[string]any{"credential": nil}, etagHeader(stripe), 422)
	// A weak replacement credential is refused too.
	h.want(owner, "PATCH", "/api/v1/observability/sinks/"+stripeID,
		map[string]any{"credential": map[string]any{"api_key": "pk_live_weak"}}, etagHeader(stripe), 422)
}

// Metering formats exist but never carry payloads, so capture cannot
// point at them and never lists them as choices.

// An eligible sink still takes a policy.
