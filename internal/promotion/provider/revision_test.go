package provider

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func TestExportedSlotKeepsEveryLimitOfItsRevision(t *testing.T) {
	t.Parallel()
	slots, err := json.Marshal([]routing.RevisionSlot{{Slot: routing.Slot{
		Name: "primary", RequestsPerMinute: new(int64(10)), TokensPerMinute: new(int64(100)),
		InputTokensPerMinute: new(int64(80)), OutputTokensPerMinute: new(int64(20)), MaxConcurrency: new(int64(2)),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := fromRevision(revision{name: "provider", configuration: []byte(`{"kind":"openai"}`), models: []byte(`[]`), slots: slots}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := json.Marshal(entry.Slots[0].Limits)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"requests_per_minute":10,"tokens_per_minute":100,"input_tokens_per_minute":80,"output_tokens_per_minute":20,"max_concurrency":2}`
	var got, expected map[string]int64
	if json.Unmarshal(exported, &got) != nil || json.Unmarshal([]byte(want), &expected) != nil || !maps.Equal(got, expected) {
		t.Fatalf("exported limits = %s, want %s", exported, want)
	}
}
