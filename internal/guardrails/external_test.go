package guardrails

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func fakeRunner(t *testing.T, handler http.HandlerFunc) (*ExternalRunner, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &ExternalRunner{Egress: egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}, Clients: egress.NewConnectionClientCache(8), Credentials: func(context.Context, string) ([]byte, error) {
		return []byte(`{"api_key":"opaque-api-key","signing_key":"01234567890123456789012345678901","access_key_id":"ACCESS","secret_access_key":"SECRET"}`), nil
	}}, server.URL
}

func TestVendorContracts(t *testing.T) {
	for _, tc := range []struct{ kind, path, response string }{
		{"bedrock", "/guardrail/id/version/1/apply", `{"action":"GUARDRAIL_INTERVENED"}`},
		{"azure.content_safety", "/contentsafety/text:analyze", `{"categoriesAnalysis":[{"category":"Hate","severity":4},{"category":"SelfHarm","severity":0},{"category":"Sexual","severity":0},{"category":"Violence","severity":0}]}`},
		{"azure.prompt_shields", "/contentsafety/text:shieldPrompt", `{"userPromptAnalysis":{"attackDetected":true},"documentsAnalysis":[]}`},
		{"model_armor", "/v1/projects/p/locations/us/templates/t:sanitizeUserPrompt", `{"sanitizationResult":{"filterMatchState":"MATCH_FOUND","invocationResult":"SUCCESS"}}`},
		{"lakera", "/v2/guard", `{"flagged":true}`},
		{"presidio", "/analyze", `[{"entity_type":"EMAIL_ADDRESS","start":1,"end":4,"score":1}]`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
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
				io.WriteString(w, tc.response)
			})
			config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "credential", Region: "us-east-1", Guardrail: "id", Version: "1", Template: "projects/p/locations/us/templates/t"})
			d := Revision{ID: "revision", GuardrailID: "guard", Type: tc.kind, Configuration: config, TimeoutMS: 1000, OnError: FailClosed}
			v, err := runner.Evaluate(t.Context(), d, Evaluation{Phase: Input, Segments: []Segment{{ID: "s", Text: "éabc"}}})
			if err != nil || v.Action != "block" {
				t.Fatal(v, err)
			}
			if tc.kind == "presidio" && (v.Redactions[0].Start != 2 || v.Redactions[0].End != 5) {
				t.Fatal("Presidio character offsets are not UTF-8 byte offsets")
			}
		})
	}
}

// Malformed offsets are rejected by the shared engine before rewriting.
