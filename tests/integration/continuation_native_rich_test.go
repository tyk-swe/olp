//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStrictNativeResultsKeepCitationRefusalAndCandidateStructure(t *testing.T) {
	for _, tc := range []struct {
		name, profile, path, request, reply string
		mustHave                            []string
	}{
		{
			name: "Anthropic citation boundaries", profile: "anthropic-messages", path: "/anthropic/v1/messages",
			request:  `{"model":"ROUTE","max_tokens":64,"messages":[{"role":"user","content":"Cite the supplied text"}]}`,
			reply:    `{"id":"msg-citation","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"quoted","citations":[{"type":"char_location","cited_text":"quoted","document_index":0,"document_title":"source.txt","start_char_index":2,"end_char_index":8}]}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":3}}`,
			mustHave: []string{`"citations"`, `"document_title":"source.txt"`, `"start_char_index":2`, `"end_char_index":8`},
		},
		{
			name: "OpenAI native refusal", profile: "openai-responses", path: "/v1/responses",
			request:  `{"model":"ROUTE","input":"refusal fixture","store":false}`,
			reply:    `{"id":"resp-refusal","object":"response","created_at":1,"status":"completed","model":"fixture-model","output":[{"id":"msg-refusal","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I cannot provide that."}]}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`,
			mustHave: []string{`"type":"refusal"`, `"refusal":"I cannot provide that."`, `"status":"completed"`},
		},
		{
			name: "Gemini ordered candidates", profile: "gemini-generation", path: "/gemini/v1beta/models/ROUTE:generateContent",
			request:  `{"contents":[{"role":"user","parts":[{"text":"two alternatives"}]}],"generationConfig":{"candidateCount":2}}`,
			reply:    `{"responseId":"gem-two","modelVersion":"fixture-model","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"first"}]},"finishReason":"STOP","safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"NEGLIGIBLE"}]},{"index":1,"content":{"role":"model","parts":[{"text":"second"}]},"finishReason":"MAX_TOKENS","safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"}]}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":7,"totalTokenCount":11}}`,
			mustHave: []string{`"index":0`, `"index":1`, `"text":"first"`, `"text":"second"`, `"finishReason":"MAX_TOKENS"`, `"probability":"LOW"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAccessHarness(t)
			var dispatched atomic.Int64
			f := &strictProviderFixture{profile: tc.profile}
			f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if tc.profile == "gemini-generation" {
						writeJSON(w, map[string]any{"models": []any{map[string]string{"name": "models/" + vendorModel}}})
					} else {
						writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
					}
					return
				}
				body, _ := io.ReadAll(r.Body)
				if !bytes.Contains(body, []byte("Cite the supplied text")) && !bytes.Contains(body, []byte("refusal fixture")) && !bytes.Contains(body, []byte("two alternatives")) {
					var probe map[string]json.RawMessage
					_ = json.Unmarshal(body, &probe)
					stream := string(probe["stream"]) == "true" || strings.HasSuffix(r.URL.Path, ":streamGenerateContent")
					switch tc.profile {
					case "anthropic-messages":
						kindGeneration(w, "anthropic", stream)
					case "gemini-generation":
						kindGeneration(w, "gemini", stream)
					case "openai-responses":
						writeResponsesFixture(w, vendorModel, vendorAnswer, stream)
					}
					return
				}
				dispatched.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.reply)
			}))
			t.Cleanup(f.Close)
			slug, key := publishStrictProvider(t, h, h.owner(), f, nil, nil, "strict")
			path := strings.Replace(tc.path, "ROUTE", slug, 1)
			req := strings.Replace(tc.request, "ROUTE", slug, 1)
			status, result, _ := h.gatewayRaw("POST", path, key, strings.NewReader(req), map[string]string{"Content-Type": "application/json"})
			if status != 200 || dispatched.Load() != 1 {
				t.Fatalf("native result: %d dispatches=%d %s", status, dispatched.Load(), result)
			}
			for _, required := range tc.mustHave {
				if !bytes.Contains(result, []byte(required)) {
					t.Fatalf("native structure %s missing from %s", required, result)
				}
			}
		})
	}
}
