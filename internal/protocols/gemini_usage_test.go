package protocols

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestGeminiUsageWithoutCandidatesTokenCount(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS","index":0}],"usageMetadata":{"promptTokenCount":10,"totalTokenCount":110,"thoughtsTokenCount":100}}`
	c, err := decodeGemini([]byte(body), "route", false)
	if err != nil {
		t.Fatal(err)
	}
	u := c.Usage
	if u == nil || u.InputTokens != 10 || u.OutputTokens != 100 || u.TotalTokens != 110 || u.ReasoningTokens == nil || *u.ReasoningTokens != 100 {
		t.Fatalf("usage = %+v", u)
	}

	c, err = decodeGemini([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":3}}`), "route", false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Usage != nil {
		t.Fatalf("usage without promptTokenCount = %+v, want nil", c.Usage)
	}
}

func TestGeminiStreamKeepsUsageWhenFinalChunkOmitsOutputCount(t *testing.T) {
	stream := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]},\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":1,\"totalTokenCount\":11}}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"MAX_TOKENS\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":10,\"thoughtsTokenCount\":100,\"totalTokenCount\":110}}\n\n"
	var out bytes.Buffer
	c, err := Stream(openai.FamilyGemini, openai.FamilyChat, strings.NewReader(stream), 8192, "route", true, func(b []byte) error { out.Write(b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.Usage == nil || c.Usage.InputTokens != 10 || c.Usage.TotalTokens != 110 {
		t.Fatalf("usage = %+v", c.Usage)
	}
	if !strings.Contains(out.String(), `"usage"`) {
		t.Fatalf("chat stream has no usage chunk: %s", out.String())
	}
}
