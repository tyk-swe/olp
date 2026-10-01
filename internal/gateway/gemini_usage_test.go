package gateway

import (
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestGeminiLiveUsageCountsThinkingAndToolUse(t *testing.T) {
	const frame = `{"usageMetadata":{"promptTokenCount":500,"responseTokenCount":80,"thoughtsTokenCount":900,"toolUsePromptTokenCount":20,"totalTokenCount":1500}}`
	u := geminiLiveUsage([]byte(frame), 1024)
	if u == nil || u.InputTokens != 520 || u.OutputTokens != 980 || u.ReasoningTokens == nil || *u.ReasoningTokens != 900 {
		t.Fatalf("Live thinking and tool-use usage: %+v", u)
	}
	if total := totalTokens(u); total == nil || *total != 1500 {
		t.Fatalf("Live total usage: %v", total)
	}
	if got := geminiLiveUsage([]byte(`{"usageMetadata":{"promptTokenCount":5,"responseTokenCount":1,"thoughtsTokenCount":-1}}`), 1024); got != nil {
		t.Fatalf("accepted negative thinking usage: %+v", got)
	}
}

func TestGeminiInteractionUsageCountsThinkingAndToolUse(t *testing.T) {
	doc, err := oif.ParseJSON([]byte(`{"usage":{"total_input_tokens":500,"total_output_tokens":80,"total_thought_tokens":900,"total_tool_use_tokens":20,"total_tokens":1500}}`), oif.Limits{MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{dispatched: true, facts: []AttemptFact{{}}}
	(&Server{}).recordGeminiInteractionUsage(x, doc.Root())
	u := x.facts[0].Usage
	if u == nil || u.InputTokens != 520 || u.OutputTokens != 980 || u.ReasoningTokens == nil || *u.ReasoningTokens != 900 {
		t.Fatalf("Interaction thinking and tool-use usage: %+v", u)
	}
	if settled := x.settledTokens(); settled == nil || *settled != 1500 {
		t.Fatalf("Interaction settled usage: %v", settled)
	}
}
