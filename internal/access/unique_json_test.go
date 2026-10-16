package access_test

import (
	"net/http/httptest"
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

// Deliberately ambiguous wire names are built dynamically: vet should still
// reject accidental duplicate JSON tags in real request declarations.
