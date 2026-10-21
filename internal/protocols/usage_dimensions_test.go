package protocols

import (
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

var _ = wire.Chat

func TestGeminiUsageModalityAndGrounding(t *testing.T) {
	body := `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP",
			"groundingMetadata":{"webSearchQueries":["a","b"]}},
			{"index":1,"content":{"parts":[{"text":"ho"}]},"finishReason":"STOP",
			"groundingMetadata":{"webSearchQueries":["c"]}}],
		"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":10,"totalTokenCount":50,
			"promptTokensDetails":[{"modality":"TEXT","tokenCount":30},{"modality":"AUDIO","tokenCount":7},{"modality":"IMAGE","tokenCount":2},{"modality":"VIDEO","tokenCount":1}],
			"cacheTokensDetails":[{"modality":"AUDIO","tokenCount":3},{"modality":"VIDEO","tokenCount":2}],
			"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":4},{"modality":"IMAGE","tokenCount":1}]}}`
	c, err := decodeGemini([]byte(body), "gemini", false)
	if err != nil {
		t.Fatal(err)
	}
	u := c.Usage
	checks := []struct {
		got  *int64
		want int64
	}{
		{u.AudioInputTokens, 7}, {u.ImageInputTokens, 3},
		{u.CachedAudioInputTokens, 3}, {u.CachedImageInputTokens, 2},
		{u.AudioOutputTokens, 4}, {u.ImageOutputTokens, 1},
	}
	for _, check := range checks {
		if check.got == nil || *check.got != check.want {
			t.Fatalf("usage %+v", u)
		}
	}
	want := map[string]int64{"google_search_query": 3, "google_search_prompt": 1}
	if !reflect.DeepEqual(u.ToolCalls, want) {
		t.Fatalf("tool calls %v, want %v", u.ToolCalls, want)
	}
}
