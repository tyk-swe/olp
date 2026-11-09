package vendor

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCloudContracts(t *testing.T) {
	for _, tc := range []struct {
		kind                  Type
		extra, path, response string
	}{
		{"bedrock", `"region":"us-east-1","guardrail":"id","version":"1"`, "/guardrail/id/version/1/apply", `{"action":"GUARDRAIL_INTERVENED"}`},
		{"azure.content_safety", "", "/contentsafety/text:analyze", `{"categoriesAnalysis":[{"category":"Hate","severity":4},{"category":"SelfHarm","severity":0},{"category":"Sexual","severity":0},{"category":"Violence","severity":0}]}`},
		{"azure.prompt_shields", "", "/contentsafety/text:shieldPrompt", `{"userPromptAnalysis":{"attackDetected":true},"documentsAnalysis":[]}`},
		{"model_armor", `"template":"projects/p/locations/us/templates/t"`, "/v1/projects/p/locations/us/templates/t:sanitizeUserPrompt", `{"sanitizationResult":{"filterMatchState":"MATCH_FOUND","invocationResult":"SUCCESS"}}`},
		{"lakera", "", "/v2/guard", `{"flagged":true}`},
		{"presidio", "", "/analyze", `[{"entity_type":"EMAIL_ADDRESS","start":1,"end":4,"score":1}]`},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Errorf("path %s", r.URL.Path)
				}
				if r.Header.Get("Authorization") == "" && r.Header.Get("Ocp-Apim-Subscription-Key") == "" {
					t.Error("missing credentials")
				}
				raw, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(raw), "éabc") {
					t.Error("missing input")
				}
				_, _ = io.WriteString(w, tc.response)
			})
			decision, err := evaluate(t, server, tc.kind, configuration(server.URL, tc.extra), Request{Role: User, Text: "éabc"})
			if err != nil || !decision.Flagged() {
				t.Fatalf("decision=%+v error=%v", decision, err)
			}
			if spans := decision.Spans(); tc.kind == "presidio" && (len(spans) != 1 || spans[0].Start != 2 || spans[0].End != 5) {
				t.Fatalf("Presidio character offsets are not UTF-8 byte offsets: %+v", spans)
			}
		})
	}
}
