package connection

import (
	"encoding/json"
	"testing"
)

func TestStoredModelMetadataThatIsNotAnObjectIsReportedNotOverwritten(t *testing.T) {
	t.Parallel()
	discovered := map[string]json.RawMessage{"context_length": json.RawMessage(`128000`)}
	for _, stored := range []string{`null`, `[1]`, `"fixture"`, `7`} {
		t.Run(stored, func(t *testing.T) {
			t.Parallel()
			if facts, err := ModelFacts(json.RawMessage(stored)); err == nil {
				t.Fatalf("catalog facts read %s as %v, want an error", stored, facts)
			}
			models := map[string]json.RawMessage{"fixture-model": json.RawMessage(stored)}
			if merged, ok, err := MergeDiscoveredFacts(models, "fixture-model", discovered); err == nil {
				t.Fatalf("discovery stored %s over %s (stored %t), want an error", merged, stored, ok)
			}
		})
	}
}
