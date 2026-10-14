//go:build integration

package usage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// factLines reads the amounts and the price-line documents one event's fact
// stored.
type factLines struct {
	estimated   *string
	list        *string
	contracted  *string
	charge      *string
	costLines   []byte
	chargeLines []byte
}

func persistLines(t *testing.T, pool *pgxpool.Pool, event *Event) []factLines {
	t.Helper()
	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	result, err := PersistEvent(t.Context(), pool, event, payload)
	if err != nil {
		t.Fatalf("persist event: %v", err)
	}
	if result.Outcome != PersistOutcomePersisted {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	rows, err := pool.Query(t.Context(), `SELECT estimated_cost::text, list_cost::text,
	    contracted_cost::text, charge::text, cost_lines, charge_lines
	    FROM olp.attempt_usage_facts WHERE request_id = $1::uuid ORDER BY attempt_ordinal`,
		event.RequestID)
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	defer rows.Close()
	var facts []factLines
	for rows.Next() {
		var fact factLines
		if err = rows.Scan(&fact.estimated, &fact.list, &fact.contracted, &fact.charge,
			&fact.costLines, &fact.chargeLines); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		facts = append(facts, fact)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("read facts: %v", err)
	}
	return facts
}

// wantLines decodes the stored document so assertions compare JSON structure,
// not byte order.
func wantLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	if raw == nil {
		return nil
	}
	var lines []map[string]any
	if err := json.Unmarshal(raw, &lines); err != nil {
		t.Fatalf("stored lines do not decode: %v", err)
	}
	return lines
}

func asAny(lines []map[string]any) any {
	var list []any
	for _, line := range lines {
		list = append(list, line)
	}
	return list
}

// jsonEqual compares two decoded JSON documents structurally.
func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// TestIntegrationPriceLineColumns records the SKU decomposition of every
// stored cost and charge: cost lines carry the list and contracted unit
// prices, charge lines the consumer's card spelling, and a fact without an
// amount carries no lines.
func TestIntegrationPriceLineColumns(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	_, project, projectKey := seedParityProjectKey(t, pool, owner)
	provider := seedParityProvider(t, pool, owner)
	committed := seedCommittedProvider(t, pool, owner, "300", "day")
	observed := time.Now().UTC().Add(-time.Minute)
	effective := observed.Add(-time.Hour)
	seedParityPrices(t, pool, owner, effective, []parityCase{
		{model: "lines-plain", price: Price{ProviderKind: "openai", Model: "lines-plain",
			Operation: "generation", InputPerMillion: rate("3"), OutputPerMillion: rate("12"), Currency: "USD"}},
		{model: "lines-discounted", price: Price{ProviderKind: "openai", Model: "lines-discounted",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
		{model: "lines-committed", price: Price{ProviderKind: "openai", Model: "lines-committed",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
		{model: "lines-committed-card", price: Price{ProviderKind: "openai", Model: "lines-committed-card",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
		{model: "lines-markup", price: Price{ProviderKind: "openai", Model: "lines-markup",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	// A provider-scoped discount contracts costs on this provider to 0.5; the
	// project card marks the project consumer's charge up by 2 and carries an
	// explicit price for one model.
	seedParityDiscount(t, pool, owner, "openai", nil, &provider, "0.5", effective)
	_, cardRev := seedParityCard(t, pool, owner, nil, &project, "2", effective)
	seedParityCardPrice(t, pool, cardRev, Price{ProviderKind: "openai", Model: "lines-committed-card",
		Operation: "generation", InputPerMillion: rate("7"), Currency: "USD"})

	persist := func(keyID, providerID, model string, usage AttemptUsage) factLines {
		t.Helper()
		event := parityEvent(t, keyID, providerID, observed, []parityCase{{model: model, usage: usage}})
		facts := persistLines(t, pool, event)
		if len(facts) != 1 {
			t.Fatalf("%s: facts = %d, want 1", model, len(facts))
		}
		return facts[0]
	}
	full := AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000),
		OutputTokens: tokens(250)}

	t.Run("plain discounted", func(t *testing.T) {
		// The projectless owner key charges cost directly: the project card
		// never applies to it.
		fact := persist(key, provider, "lines-plain", full)
		want := []map[string]any{
			{"component": "input_per_million", "tier": nil, "unit": "1000000 Tokens",
				"quantity": "0.001", "list_unit_price": "3", "contracted_unit_price": "1.5"},
			{"component": "output_per_million", "tier": nil, "unit": "1000000 Tokens",
				"quantity": "0.00025", "list_unit_price": "12", "contracted_unit_price": "6"},
		}
		if got := wantLines(t, fact.costLines); !jsonEqual(got, asAny(want)) {
			t.Fatalf("cost_lines = %s", fact.costLines)
		}
		// No card: the charge is the contracted price, so charge lines carry
		// contracted as both unit prices.
		wantCharge := []map[string]any{
			{"component": "input_per_million", "tier": nil, "unit": "1000000 Tokens",
				"quantity": "0.001", "list_unit_price": "1.5", "contracted_unit_price": "1.5"},
			{"component": "output_per_million", "tier": nil, "unit": "1000000 Tokens",
				"quantity": "0.00025", "list_unit_price": "6", "contracted_unit_price": "6"},
		}
		if got := wantLines(t, fact.chargeLines); !jsonEqual(got, asAny(wantCharge)) {
			t.Fatalf("charge_lines = %s", fact.chargeLines)
		}
		if fact.charge == nil || *fact.charge != *fact.contracted {
			t.Fatalf("charge = %v, want cost %v", fact.charge, fact.contracted)
		}
	})

	t.Run("committed zeroes charge lines only", func(t *testing.T) {
		fact := persist(key, committed, "lines-committed",
			AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)})
		lines := wantLines(t, fact.costLines)
		if len(lines) != 1 || lines[0]["list_unit_price"] != "3" || lines[0]["contracted_unit_price"] != "3" {
			t.Fatalf("cost_lines = %s", fact.costLines)
		}
		// A commitment covers the consumer: the same lines, repriced to zero.
		charged := wantLines(t, fact.chargeLines)
		if len(charged) != 1 || charged[0]["list_unit_price"] != "0" ||
			charged[0]["contracted_unit_price"] != "0" || charged[0]["quantity"] != "0.001" {
			t.Fatalf("charge_lines = %s", fact.chargeLines)
		}
		if fact.charge == nil || *fact.charge != "0.000000000000" {
			t.Fatalf("charge = %v, want 0", fact.charge)
		}
	})

	t.Run("committed with explicit card price", func(t *testing.T) {
		fact := persist(projectKey, committed, "lines-committed-card",
			AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)})
		charged := wantLines(t, fact.chargeLines)
		if len(charged) != 1 || charged[0]["list_unit_price"] != "7" ||
			charged[0]["contracted_unit_price"] != "7" || charged[0]["quantity"] != "0.001" {
			t.Fatalf("charge_lines = %s", fact.chargeLines)
		}
		if fact.charge == nil || *fact.charge != "0.007000000000" {
			t.Fatalf("charge = %v, want card price 0.007", fact.charge)
		}
		lines := wantLines(t, fact.costLines)
		if len(lines) != 1 || lines[0]["list_unit_price"] != "3" || lines[0]["contracted_unit_price"] != "3" {
			t.Fatalf("cost_lines = %s", fact.costLines)
		}
	})

	t.Run("markup card scales contracted", func(t *testing.T) {
		fact := persist(projectKey, provider, "lines-markup",
			AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)})
		// contracted is 0.0015 after the 0.5 discount; markup 2 charges 0.003.
		charged := wantLines(t, fact.chargeLines)
		if len(charged) != 1 || charged[0]["list_unit_price"] != "3" ||
			charged[0]["contracted_unit_price"] != "3" || charged[0]["quantity"] != "0.001" {
			t.Fatalf("charge_lines = %s", fact.chargeLines)
		}
		if fact.charge == nil || *fact.charge != "0.003000000000" {
			t.Fatalf("charge = %v, want 0.003", fact.charge)
		}
	})

	t.Run("unpriced has no lines", func(t *testing.T) {
		fact := persist(key, provider, "lines-unpriced", full)
		if fact.costLines != nil || fact.chargeLines != nil {
			t.Fatalf("lines = (%s, %s)", fact.costLines, fact.chargeLines)
		}
	})

	t.Run("incomplete usage has no lines", func(t *testing.T) {
		fact := persist(key, provider, "lines-plain",
			AttemptUsage{Observed: true, Complete: false, InputTokens: tokens(1000)})
		if fact.costLines != nil || fact.chargeLines != nil || fact.list != nil {
			t.Fatalf("lines = (%s, %s)", fact.costLines, fact.chargeLines)
		}
	})
}
