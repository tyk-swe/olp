package access

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeUniquePromotesEmbeddedStructMembers(t *testing.T) {
	type supply struct {
		DailyCostLimit *string `json:"daily_cost_limit,omitempty"`
	}
	type slot struct {
		Name string `json:"name"`
		supply
	}
	decode := func(body string, input any) error {
		r := httptest.NewRequest("PUT", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		return DecodeUnique(r, input, 1<<20)
	}

	var input struct {
		Slot slot `json:"slot"`
	}
	if err := decode(`{"slot":{"name":"default","daily_cost_limit":"1.00"}}`, &input); err != nil {
		t.Fatalf("a promoted member was refused: %v", err)
	}
	if input.Slot.DailyCostLimit == nil || *input.Slot.DailyCostLimit != "1.00" {
		t.Fatalf("the promoted member did not decode: %+v", input.Slot)
	}

	var nullable struct {
		Slot slot `json:"slot"`
	}
	if err := decode(`{"slot":{"name":"default","daily_cost_limit":null}}`, &nullable); err != nil {
		t.Fatalf("a promoted null member was refused: %v", err)
	}

	var unknown struct {
		Slot slot `json:"slot"`
	}
	if err := decode(`{"slot":{"name":"default","supply":{"daily_cost_limit":"1.00"}}}`, &unknown); err == nil {
		t.Fatal("the embedded type's own name must not become a member")
	}
}
