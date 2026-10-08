package access_test

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

type supplyFields struct {
	DailyCostLimit *string        `json:"daily_cost_limit"`
	PriorityShares map[string]int `json:"priority_shares"`
}
type slotFields struct {
	supplyFields
	Name string `json:"name"`
}

func decodeCanonical(body string, target any) error {
	request := httptest.NewRequest("PUT", "/", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return access.DecodeUnique(request, target, 4096)
}
func TestDecodeUniqueFlattensEmbeddedJSONFields(t *testing.T) {
	var slot slotFields
	if err := decodeCanonical(`{"name":"default","daily_cost_limit":"1.25","priority_shares":{"high":20}}`, &slot); err != nil {
		t.Fatal(err)
	}
	if slot.DailyCostLimit == nil || *slot.DailyCostLimit != "1.25" || slot.PriorityShares["high"] != 20 {
		t.Fatalf("embedded policy lost: %+v", slot)
	}
	if err := decodeCanonical(`{"name":"default","daily_cost_limit":null,"priority_shares":null}`, &slot); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"DailyCostLimit":"1"}`, `{"supplyFields":{"daily_cost_limit":"1"}}`, `{"name":null}`} {
		if decodeCanonical(body, &slot) == nil {
			t.Fatalf("noncanonical JSON accepted: %s", body)
		}
	}
}
func TestDecodeUniqueHonorsTaggedAnonymousAndDominantFields(t *testing.T) {
	var nested struct {
		supplyFields `json:"supply"`
	}
	if err := decodeCanonical(`{"supply":{"daily_cost_limit":"2"}}`, &nested); err != nil {
		t.Fatal(err)
	}
	if decodeCanonical(`{"daily_cost_limit":"2"}`, &nested) == nil {
		t.Fatal("tagged carrier flattened")
	}
	var dominant struct {
		supplyFields
		DailyCostLimit int `json:"daily_cost_limit"`
	}
	if err := decodeCanonical(`{"daily_cost_limit":2}`, &dominant); err != nil {
		t.Fatal(err)
	}
	if decodeCanonical(`{"daily_cost_limit":null}`, &dominant) == nil {
		t.Fatal("embedded pointer relaxed explicit scalar validation")
	}
	type left struct {
		Value string `json:"value"`
	}
	type right struct {
		Value string `json:"value"`
	}

	// Deliberately ambiguous wire names are built dynamically: vet should still
	// reject accidental duplicate JSON tags in real request declarations.
	ambiguous := reflect.New(reflect.StructOf([]reflect.StructField{
		{Name: "Left", Type: reflect.TypeFor[left](), Anonymous: true},
		{Name: "Right", Type: reflect.TypeFor[right](), Anonymous: true},
	})).Interface()

	if decodeCanonical(`{"value":"ambiguous"}`, ambiguous) == nil {
		t.Fatal("ambiguous promoted name accepted")
	}
}

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
		return access.DecodeUnique(r, input, 1<<20)
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
