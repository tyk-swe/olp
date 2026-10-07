package protocols

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestRequestFeaturesReadEveryGenerationDialect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family openai.Family
		body   string
		want   Features
	}{
		{"openai chat", openai.FamilyChat, `{"model":"m","reasoning_effort":"HIGH","response_format":{"type":"json_object"},"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}]}`,
			Features{Tools: true, StructuredOutput: true, Modalities: []string{"text", "image"}, ReasoningEffort: "high"}},
		{"openai responses", openai.FamilyResponses, `{"model":"m","reasoning":{"effort":"low"},"input":"hello"}`,
			Features{Modalities: []string{"text"}, ReasoningEffort: "low"}},
		{"openai audio", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}},{"type":"text","text":"transcribe"}]}]}`,
			Features{Modalities: []string{"text", "audio"}}},
		{"openai file", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-1"}}]}]}`,
			Features{Modalities: []string{"file"}}},
		{"responses file", openai.FamilyResponses, `{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_data":"data:application/pdf;base64,AA=="}]}]}`,
			Features{Modalities: []string{"file"}}},
		{"anthropic", openai.FamilyAnthropic, `{"model":"m","max_tokens":5,"output_config":{"effort":"medium"},"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}`,
			Features{Modalities: []string{"image"}, ReasoningEffort: "medium"}},
		{"gemini", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"},{"inlineData":{"mimeType":"audio/wav","data":"AA=="}}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`,
			Features{Modalities: []string{"text", "audio"}, ReasoningEffort: "high"}},
		{"gemini search", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"googleSearch":{}}]}`,
			Features{Tools: true, Modalities: []string{"text"}}},
		{"gemini code execution", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"codeExecution":{}}]}`,
			Features{Tools: true, Modalities: []string{"text"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := Parse(tc.family, []byte(tc.body), "m")
			if err != nil {
				t.Fatal(err)
			}
			got := RequestFeatures(request)
			if got.Tools != tc.want.Tools || got.StructuredOutput != tc.want.StructuredOutput || got.ReasoningEffort != tc.want.ReasoningEffort || !slices.Equal(got.Modalities, tc.want.Modalities) {
				t.Fatalf("features = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRequestTextIsTheLastUserTurnCutOnACharacterBoundary(t *testing.T) {
	request, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"r","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":[{"type":"text","text":"héllo"},{"type":"text","text":"world"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := RequestText(request, 1024); got != "héllo\nworld" {
		t.Fatalf("text = %q", got)
	}
	if got := RequestText(request, 2); got != "h" {
		t.Fatalf("cut text = %q, want the split character dropped", got)
	}
}
