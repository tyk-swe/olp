package media

import (
	"encoding/json"
	"testing"
)

func int64ptr(v int64) *int64 { return &v }

// With an output breakdown only its image share bills as image output.

// Without one, every output token is the produced image.

func TestGeminiMediaModalityShares(t *testing.T) {
	// A Gemini usage's modality details map onto the shares: audio in and out,
	// images and video frames as image tokens.
	var result geminiResult
	if err := json.Unmarshal([]byte(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14,
		"promptTokensDetails":[{"modality":"AUDIO","tokenCount":3},{"modality":"VIDEO","tokenCount":2}],
		"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":4}]}}`), &result); err != nil {
		t.Fatal(err)
	}
	usage := result.tokens()
	if usage == nil || usage.AudioInputTokens == nil || *usage.AudioInputTokens != 3 ||
		usage.ImageInputTokens == nil || *usage.ImageInputTokens != 2 ||
		usage.ImageOutputTokens == nil || *usage.ImageOutputTokens != 4 {
		t.Fatalf("usage %+v", usage)
	}
	// Without modality detail every output token stays text.
	var plain geminiResult
	if err := json.Unmarshal([]byte(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14}}`), &plain); err != nil {
		t.Fatal(err)
	}
	usage = plain.tokens()
	if usage == nil || usage.ImageOutputTokens != nil || usage.AudioOutputTokens != nil ||
		usage.OutputTokens != 4 {
		t.Fatalf("usage %+v", usage)
	}
}
