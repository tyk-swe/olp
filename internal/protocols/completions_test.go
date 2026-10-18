package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestLegacyCompletionsMapping(t *testing.T) {
	r, err := Parse(wire.Completions, []byte(`{"model":"route","prompt":"hello","max_tokens":8,"n":2}`), "")
	if err != nil {
		t.Fatal(err)
	}
	body, family, err := EncodeTarget(r, wire.Chat, "openai", "openai", "upstream", nil)
	if err != nil || family != wire.Chat || !strings.Contains(string(body), `"content":"hello"`) || strings.Contains(string(body), `"prompt"`) {
		t.Fatalf("mapping: %s %s %v", body, family, err)
	}
	input := `{"id":"cmpl-1","object":"chat.completion","model":"upstream","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"one"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"two"},"finish_reason":"length"}],"usage":{"prompt_tokens":2,"completion_tokens":4,"total_tokens":6}}`
	c, err := Decode(wire.Chat, wire.Completions, []byte(input), "route", "")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Choices []struct {
			Text  string
			Index int
		}
	}
	if json.Unmarshal(c.Body, &result) != nil || len(result.Choices) != 2 || result.Choices[1].Text != "two" || c.Usage.TotalTokens != 6 {
		t.Fatalf("result: %s %+v", c.Body, c.Usage)
	}
	for _, extra := range []string{`"prompt":["a","b"]`, `"echo":false`, `"best_of":1`, `"logprobs":0`, `"suffix":"x"`, `"unknown":true`} {
		fields := `{"model":"route","prompt":"hello",` + extra + `}`
		// Avoid duplicate prompt keys; malformed/ambiguous JSON is also refused.
		r, err := Parse(wire.Completions, []byte(fields), "")
		if err == nil {
			_, _, err = EncodeTarget(r, wire.Chat, "openai", "openai", "upstream", nil)
		}
		if err == nil {
			t.Errorf("accepted unmappable %s", extra)
		}
	}
	r, _ = Parse(wire.Completions, []byte(`{"model":"route","prompt":"hi"}`), "")
	if _, _, err = EncodeTarget(r, wire.Chat, "openai", "openai", "upstream", Object{"tools": raw([]any{})}); err == nil {
		t.Fatal("unmapped target default admitted")
	}
}

func TestLegacyCompletionsNativeAndStream(t *testing.T) {
	r, err := Parse(wire.Completions, []byte(`{ "model":"route", "prompt":[1,2],"echo":true,"extension":{"v":1.00}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := PrepareIdentity(r, "upstream")
	if err != nil || !strings.Contains(string(p.Document().Bytes()), `"v":1.00`) {
		t.Fatalf("native: %s %v", p.Document().Bytes(), err)
	}
	body := `{ "id":"c", "object":"text_completion", "model":"upstream", "choices":[{"index":0,"text":"hello","finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3},"extension":1.00 }`
	c, err := Decode(wire.Completions, wire.Completions, []byte(body), "route", "")
	if err != nil || c.OutputText != "hello" || !strings.Contains(string(c.Body), `"extension":1.00`) {
		t.Fatalf("native result: %+v %v", c, err)
	}
	native := `data: {"object":"text_completion","model":"upstream","choices":[{"index":0,"text":"hello","finish_reason":"stop"}]}` + "\n\n" + `data: {"object":"text_completion","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}` + "\n\ndata: [DONE]\n\n"
	for _, tc := range []struct {
		source wire.Format
		input  string
	}{{wire.Completions, native}, {wire.Chat, strings.ReplaceAll(strings.ReplaceAll(native, `"object":"text_completion"`, `"object":"chat.completion.chunk"`), `"text":"hello"`, `"delta":{"content":"hello"}`)}} {
		var out strings.Builder
		c, err = Stream(tc.source, wire.Completions, strings.NewReader(tc.input), 4096, "route", true, func(b []byte) error { out.Write(b); return nil })
		if err != nil || c.Usage == nil || c.Usage.TotalTokens != 3 || !strings.Contains(out.String(), `"text":"hello"`) || !strings.Contains(out.String(), "[DONE]") {
			t.Fatalf("stream %s: %s %+v %v", tc.source, out.String(), c, err)
		}
		truncated := strings.ReplaceAll(tc.input, "data: [DONE]\n\n", "")
		if _, err = Stream(tc.source, wire.Completions, strings.NewReader(truncated), 4096, "route", true, func([]byte) error { return nil }); err == nil {
			t.Fatal("truncated stream accepted")
		}
	}
}

func TestLegacyCompletionRejectsMalformedLogprobs(t *testing.T) {
	for _, value := range []string{"-1", "6", "true"} {
		if _, err := Parse(wire.Completions, []byte(`{"model":"route","prompt":"hello","logprobs":`+value+`}`), ""); err == nil {
			t.Fatalf("accepted logprobs %s", value)
		}
	}
}

func TestLegacyCompletionCannotIntroduceRetainedState(t *testing.T) {
	for _, name := range []string{"store", "background", "conversation", "previous_response_id", "input_file_id", "file_id"} {
		if _, err := Parse(wire.Completions, []byte(`{"model":"route","prompt":"hello","`+name+`":true}`), ""); err == nil {
			t.Fatalf("accepted retained field %s", name)
		}
	}
}

func TestLegacyCompletionMappingHonorsVendorContract(t *testing.T) {
	for _, tc := range []struct {
		vendor, field, value string
	}{
		{"deepseek", "presence_penalty", "0.5"},
		{"together", "logit_bias", `{"1":1}`},
		{"cohere", "n", "2"},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			for _, useDefault := range []bool{false, true} {
				body := `{"model":"route","prompt":"hello"`
				var defaults Object
				if useDefault {
					defaults = Object{tc.field: json.RawMessage(tc.value)}
				} else {
					body += `,"` + tc.field + `":` + tc.value
				}
				r, err := Parse(wire.Completions, []byte(body+`}`), "")
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := EncodeTarget(r, wire.Chat, "openai_compatible", tc.vendor, "upstream", defaults); err == nil {
					t.Fatalf("unsupported %s admitted (default=%v)", tc.field, useDefault)
				}
			}
		})
	}
	r, err := Parse(wire.Completions, []byte(`{"model":"route","prompt":"hello","seed":7}`), "")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := EncodeTarget(r, wire.Chat, "openai_compatible", "mistral", "upstream", nil)
	if err != nil {
		t.Fatal(err)
	}
	var fields Object
	if err := json.Unmarshal(body, &fields); err != nil || string(fields["random_seed"]) != "7" || fields["seed"] != nil {
		t.Fatalf("seed rewrite: %s (%v)", body, err)
	}
}

func TestLegacyCompletionMappedStreamPreservesObfuscation(t *testing.T) {
	input := `data: {"object":"chat.completion.chunk","model":"upstream","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}],"obfuscation":"padding"}` + "\n\n" +
		`data: {"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3},"obfuscation":"usage-padding"}` + "\n\ndata: [DONE]\n\n"
	var output strings.Builder
	c, err := Stream(wire.Chat, wire.Completions, strings.NewReader(input), 4096, "route", true, func(b []byte) error {
		output.Write(b)
		return nil
	})
	if err != nil || c.Usage == nil || c.Usage.TotalTokens != 3 || !strings.Contains(output.String(), `"text":"hello"`) || !strings.Contains(output.String(), `"obfuscation":"padding"`) || !strings.Contains(output.String(), `"obfuscation":"usage-padding"`) || !strings.Contains(output.String(), "[DONE]") {
		t.Fatalf("mapped stream: %s %+v %v", output.String(), c, err)
	}
}
