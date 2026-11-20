package provider

import (
	"encoding/json"
	"testing"
)

func TestExportedSlotKeepsEveryLimitOfItsRevision(t *testing.T) {
	t.Parallel()
	const slots = `[{"name":"primary","requests_per_minute":10,"tokens_per_minute":100,"input_tokens_per_minute":80,"output_tokens_per_minute":20,"max_concurrency":2,"daily_cost_limit":"2.00","monthly_cost_limit":"20.00","priority_shares":{"critical":40,"high":30,"normal":20,"low":10},"saturation_percent":80},{"name":"split-only","output_tokens_per_minute":10},{"name":"unbounded"}]`
	entry, err := FromRevision("provider", nil, []byte(`{"kind":"openai"}`), []byte(`[]`), []byte(slots))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"max_concurrency":2,"requests_per_minute":10,"tokens_per_minute":100,"input_tokens_per_minute":80,"output_tokens_per_minute":20,"daily_cost_limit":"2.00","monthly_cost_limit":"20.00","priority_shares":{"critical":40,"high":30,"normal":20,"low":10},"saturation_percent":80}`,
		`{"max_concurrency":null,"requests_per_minute":null,"tokens_per_minute":null,"output_tokens_per_minute":10}`,
		`{"max_concurrency":null,"requests_per_minute":null,"tokens_per_minute":null}`,
	}
	if len(entry.Slots) != len(want) {
		t.Fatalf("exported %d slots, want %d", len(entry.Slots), len(want))
	}
	for i, slot := range entry.Slots {
		exported, err := json.Marshal(slot.Limits)
		if err != nil || string(exported) != want[i] {
			t.Fatalf("exported limits for %s = %s, want %s (err %v)", slot.Name, exported, want[i], err)
		}
	}
}
