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
	"time"

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
func TestWebhookSigningAndBoundedVerdict(t *testing.T) {
	runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		header := r.Header.Get("X-OLP-Signature")
		if !strings.HasPrefix(header, "t=") || len(header) < 80 {
			t.Error("unsigned webhook")
		}
		var request Evaluation
		if json.Unmarshal(raw, &request) != nil || request.Segments[0].Text != "private content" {
			t.Error("wrong segments")
		}
		io.WriteString(w, `{"action":"block","categories":["test.classification"]}`)
	})
	config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "credential"})
	d := Revision{ID: "revision", GuardrailID: "guard", Name: "DLP", Type: "webhook", Configuration: config, TimeoutMS: 1000, OnError: FailClosed}
	e, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: Input, Action: Block}}, map[string]Revision{"revision": d}, runner)
	if err != nil {
		t.Fatal(err)
	}
	result, err := evaluateText(t, e, "private content", Input, nil)
	if err == nil || !result.Blocked || result.Decisions[0].Categories[0] != "test.classification" {
		t.Fatal(result, err)
	}
	encoded, _ := json.Marshal(result.Decisions)
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "opaque") {
		t.Fatal("leaked content")
	}
	if Signature([]byte("key"), time.Unix(42, 0), []byte("a")) == Signature([]byte("key"), time.Unix(42, 0), []byte("b")) {
		t.Fatal("signature ignores body")
	}
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
func TestMalformedExternalVerdictsAndIndependentBreakers(t *testing.T) {
	calls := 0
	runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"action":"redact","redactions":[{"segment":"s","start":0,"end":99999,"replacement":"x"}]}`)
	})
	config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "key"})
	d := Revision{ID: "a", GuardrailID: "guard", Type: "webhook", Configuration: config, TimeoutMS: 1000, OnError: FailClosed}
	// Malformed offsets are rejected by the shared engine before rewriting.
	e, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: "a", Phase: Input, Action: Redact}}, map[string]Revision{"a": d}, runner)
	if err != nil {
		t.Fatal(err)
	}
	r, err := evaluateText(t, e, "x", Input, nil)
	if err == nil || !r.Blocked || r.Decisions[0].Outcome != "failed_closed" {
		t.Fatal(r, err)
	}
	for i := 0; i < 5; i++ {
		runner.leave("a", errEvaluation)
	}
	if runner.enter("a") {
		t.Fatal("open circuit admitted")
	}
	if !runner.enter("b") {
		t.Fatal("unrelated guardrail circuit was opened")
	}
}

func TestVendorMalformedAndTimeoutPolicies(t *testing.T) {
	for _, kind := range []string{"bedrock", "azure.content_safety", "azure.prompt_shields", "model_armor", "lakera", "presidio", "webhook"} {
		for _, timeout := range []bool{false, true} {
			for _, policy := range []string{FailClosed, FailOpen} {
				t.Run(kind+"/"+policy+map[bool]string{true: "/timeout", false: "/malformed"}[timeout], func(t *testing.T) {
					runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
						if timeout {
							select {
							case <-r.Context().Done():
								return
							case <-time.After(100 * time.Millisecond):
							}
						}
						io.WriteString(w, `{}`)
					})
					config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "credential", Region: "us-east-1", Guardrail: "id", Version: "1", Template: "projects/p/locations/us/templates/t"})
					d := Revision{ID: "revision", GuardrailID: "guard", Type: kind, Configuration: config, TimeoutMS: 10, OnError: policy}
					engine, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: d.ID, Phase: Input, Action: Block}}, map[string]Revision{d.ID: d}, runner)
					if err != nil {
						t.Fatal(err)
					}
					result, err := evaluateText(t, engine, "private content", Input, nil)
					if policy == FailClosed {
						if err == nil || !result.Blocked {
							t.Fatal("failure opened", result, err)
						}
					} else if err != nil || result.Blocked {
						t.Fatal("fail-open refused", result, err)
					}
					want := "failed_open"
					if policy == FailClosed {
						want = "failed_closed"
					}
					if len(result.Decisions) != 1 || result.Decisions[0].Outcome != want {
						t.Fatal(result)
					}
				})
			}
		}
	}
}
func TestPresidioAnonymizerAndPromptShieldDocuments(t *testing.T) {
	runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid request")
		}
		switch r.URL.Path {
		case "/analyze":
			io.WriteString(w, `[{"entity_type":"EMAIL_ADDRESS","start":1,"end":4,"score":1}]`)
		case "/anonymize":
			if _, ok := request["analyzer_results"]; !ok {
				t.Error("missing analysis")
			}
			io.WriteString(w, `{"text":"é[REDACTED]"}`)
		case "/contentsafety/text:shieldPrompt":
			if _, ok := request["userPrompt"]; ok {
				t.Error("untrusted tool content sent as user instructions")
			}
			if _, ok := request["documents"]; !ok {
				t.Error("missing untrusted document")
			}
			io.WriteString(w, `{"documentsAnalysis":[{"attackDetected":true}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, AnonymizerEndpoint: endpoint, ContentEgress: true, CredentialRef: "credential"})
	d := Revision{ID: "presidio", GuardrailID: "guard", Type: "presidio", Configuration: config, TimeoutMS: 1000, OnError: FailClosed}
	v, err := runner.Evaluate(t.Context(), d, Evaluation{Phase: Input, Segments: []Segment{{ID: "s", Text: "éabc"}}})
	if err != nil || len(v.Redactions) != 1 || v.Redactions[0].Replacement != "é[REDACTED]" {
		t.Fatal(v, err)
	}
	config, _ = json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "credential"})
	d.ID, d.Type, d.Configuration = "shield", "azure.prompt_shields", config
	v, err = runner.Evaluate(t.Context(), d, Evaluation{Phase: Input, Segments: []Segment{{ID: "s", Kind: "tool_result", Text: "attack"}}})
	if err != nil || v.Action != Block {
		t.Fatal(v, err)
	}
}
