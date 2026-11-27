package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contracts/unary"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/platform/concurrency"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestBrowserDelegationPreflightAllowsConsent(t *testing.T) {
	const origin = "https://client.example"
	h := newHarness(t, Config{CORSAllowedOrigins: []string{origin}})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, h.server.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "x-olp-delegation-consent")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	allowed := strings.Split(resp.Header.Get("Access-Control-Allow-Headers"), ",")
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != origin ||
		!slices.ContainsFunc(allowed, func(value string) bool {
			return strings.EqualFold(strings.TrimSpace(value), "X-OLP-Delegation-Consent")
		}) {
		t.Fatalf("delegated request cannot pass preflight: %d, %v", resp.StatusCode, resp.Header)
	}
}

func TestModelsFilteredByKey(t *testing.T) {
	h := newHarness(t, Config{})
	list := func(key, path string) (int, map[string]any) {
		resp := h.do(t.Context(), http.MethodGet, path, key, nil, nil)
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	if code, body := list(fullKey, "/v1/models"); code != 200 || len(body["data"].([]any)) != 1 || body["data"].([]any)[0].(map[string]any)["id"] != routeSlug {
		t.Fatalf("%d %v", code, body)
	}
	if code, body := list(otherKey, "/v1/models"); code != 200 || len(body["data"].([]any)) != 0 {
		t.Fatalf("restricted key sees %v", body)
	}
	if code, body := list(fullKey, "/v1/models/"+routeSlug); code != 200 || body["owned_by"] != "openllmproxy" {
		t.Fatalf("%d %v", code, body)
	}
	if code, body := list(otherKey, "/v1/models/"+routeSlug); code != 404 || errorCode(t, body) != "route_not_found" {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := list(fullKey, "/v1/not-enabled"); code != 404 {
		t.Fatalf("unknown endpoint %d", code)
	}
	if code, body := list(fullKey, "/anthropic/v1/models"); code != 200 {
		t.Fatalf("%d %v", code, body)
	} else {
		item := body["data"].([]any)[0].(map[string]any)
		if item["id"] != routeSlug || item["capabilities"] == nil {
			t.Fatalf("anthropic model %v", item)
		}
	}
	if code, body := list(otherKey, "/anthropic/v1/models"); code != 200 || len(body["data"].([]any)) != 0 {
		t.Fatalf("restricted key sees %v", body)
	}
	if code, body := list(fullKey, "/gemini/v1/models"); code != 200 {
		t.Fatalf("%d %v", code, body)
	} else {
		item := body["models"].([]any)[0].(map[string]any)
		if item["name"] != "models/"+routeSlug || item["capabilities"] == nil {
			t.Fatalf("gemini model %v", item)
		}
	}
	if code, body := list(otherKey, "/gemini/v1/models"); code != 200 || len(body["models"].([]any)) != 0 {
		t.Fatalf("restricted key sees %v", body)
	}
}

func TestTransformedRerankPreservesDocumentIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
		status int
	}{
		{"wrong string document", `{"index":0,"relevance_score":0.5,"document":"second"}`, http.StatusBadGateway},
		{"wrong object document", `{"index":0,"relevance_score":0.5,"document":{"text":"second"}}`, http.StatusBadGateway},
		{"null score", `{"index":0,"relevance_score":null,"document":"first"}`, http.StatusBadGateway},
		{"zero score", `{"index":0,"relevance_score":0,"document":"first"}`, http.StatusOK},
		{"matching object", `{"index":0,"relevance_score":0.5,"document":{"text":"first"}}`, http.StatusOK},
		{"omitted document", `{"index":0,"relevance_score":0.5}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
				for id, provider := range snapshot.Providers {
					provider.VendorID = "cohere"
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "rerank", Surface: "openai", Mode: "unary"}}
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.Operations, route.MaxAttempts = []string{"rerank"}, 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"results":[`+tc.result+`]}`)
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/rerank", fullKey, []byte(`{"model":"team-chat","query":"q","documents":["first","second"],"top_n":1,"return_documents":true}`), nil)
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d, body %s", resp.StatusCode, tc.status, body)
			}
			if tc.status == http.StatusBadGateway && !strings.Contains(string(body), `"code":"provider_protocol_error"`) {
				t.Fatalf("unexpected rejection: %s", body)
			}
			if tc.status == http.StatusOK && !strings.Contains(string(body), `"document":"first"`) {
				t.Fatalf("document identity lost: %s", body)
			}
		})
	}
}

func TestTransformedEmbeddingsRejectIncompleteVectors(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           int
	}{
		{"complete", `{"embeddings":[{"values":[0.5,0.25]},{"values":[0.25,0.5]}]}`, http.StatusOK},
		{"missing input", `{"embeddings":[{"values":[0.5,0.25]}]}`, http.StatusBadGateway},
		{"extra input", `{"embeddings":[{"values":[0.5,0.25]},{"values":[0.25,0.5]},{"values":[1,1]}]}`, http.StatusBadGateway},
		{"null coordinate", `{"embeddings":[{"values":[0.5,null]},{"values":[0.25,0.5]}]}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
				strictEmbeddings(snapshot)
				for id, provider := range snapshot.Providers {
					provider.Kind = "gemini"
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.MaxAttempts = 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, tc.response))
			resp := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"team-chat","input":["a","b"],"encoding_format":"float"}`), nil)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d, body %s", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestEmbeddingsRejectInvalidResultIndices(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       int
	}{
		{"complete", `{"index":0,"embedding":[0.5,0]},{"index":1,"embedding":[0.25,0.5]}`, http.StatusOK},
		{"reordered", `{"index":1,"embedding":[0.25,0.5]},{"index":0,"embedding":[0.5,0]}`, http.StatusOK},
		{"missing input", `{"index":0,"embedding":[0.5,0]}`, http.StatusBadGateway},
		{"out of range", `{"index":0,"embedding":[0.5,0]},{"index":2,"embedding":[0.25,0.5]}`, http.StatusBadGateway},
		{"shifted", `{"index":1,"embedding":[0.5,0]},{"index":2,"embedding":[0.25,0.5]}`, http.StatusBadGateway},
		{"duplicate", `{"index":0,"embedding":[0.5,0]},{"index":0,"embedding":[0.25,0.5]}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
				strictEmbeddings(snapshot)
				route := snapshot.Routes[routeSlug]
				route.MaxAttempts = 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, `{"data":[`+tc.data+`]}`))
			resp := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"team-chat","input":["a","b"],"encoding_format":"float"}`), nil)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d, body %s", resp.StatusCode, tc.want, body)
			}
			if tc.want == http.StatusBadGateway && !strings.Contains(string(body), `"code":"provider_protocol_error"`) {
				t.Fatalf("unexpected rejection %s", body)
			}
			if tc.want == http.StatusOK {
				var result struct {
					Data []struct {
						Index     int       `json:"index"`
						Embedding []float64 `json:"embedding"`
					} `json:"data"`
				}
				if err := json.Unmarshal(body, &result); err != nil || len(result.Data) != 2 {
					t.Fatalf("invalid result %s", body)
				}
				for _, item := range result.Data {
					want := [][]float64{{0.5, 0}, {0.25, 0.5}}[item.Index]
					if !slices.Equal(item.Embedding, want) {
						t.Fatalf("vector identity changed %s", body)
					}
				}
			}
		})
	}
}

func TestEmbeddingsCountTokenIDInputs(t *testing.T) {
	for _, tc := range []struct {
		name, input, data string
		want              int
	}{
		{"batches", `[[1,2],[3,4]]`, `{"index":0,"embedding":[0.5,0]},{"index":1,"embedding":[0.25,0.5]}`, http.StatusOK},
		{"missing batch", `[[1,2],[3,4]]`, `{"index":0,"embedding":[0.5,0]}`, http.StatusBadGateway},
		{"one tokenized input", `[1024,2048]`, `{"index":0,"embedding":[0.5,0]}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
				strictEmbeddings(snapshot)
				route := snapshot.Routes[routeSlug]
				route.MaxAttempts = 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, `{"data":[`+tc.data+`]}`))
			resp := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"team-chat","input":`+tc.input+`,"encoding_format":"float"}`), nil)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d, body %s", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestResearchStreamFollowsOutagePolicy(t *testing.T) {
	runner, err := scripts.NewRunner(unreachableValkey{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	limiter := limitadmission.NewDurable(runner, nil)
	slots := int64(1)
	authority := keyauthority.Authority{ID: uuid.NewString(), Policy: keyauthority.KeyPolicy{MaxConcurrency: &slots}}
	for _, test := range []struct {
		name       string
		policy     limitadmission.OutagePolicy
		provider   *routing.Limits
		wantRefuse bool
	}{
		{"fail open admits key concurrency", limitadmission.FailOpen, nil, false},
		{"fail closed refuses key concurrency", limitadmission.FailClosed, nil, true},
		{"fail open still refuses provider concurrency", limitadmission.FailOpen, &routing.Limits{MaxConcurrency: &slots}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			admission := quota.NewAdmission(limiter, fleet.New(runner), func() limitadmission.OutagePolicy { return test.policy }, slog.New(slog.DiscardHandler))
			lease, failure := admission.ReserveResearchStream(t.Context(), authority, routeSlug, "service", "provider", test.provider, time.Minute)
			if test.wantRefuse {
				if failure == nil || failure.Code != "distributed_limits_unavailable" {
					t.Fatalf("outage admitted the stream: lease=%v failure=%v", lease, failure)
				}
				return
			}
			if failure != nil {
				t.Fatalf("fail-open installation refused the stream: %v", failure)
			}
			if admission.FailOpenTotal() != 1 {
				t.Fatalf("fail-open total = %d, want 1", admission.FailOpenTotal())
			}
		})
	}
}

func TestBedrockStreamClassifiesRequestEndings(t *testing.T) {
	for _, tc := range []struct {
		name, class, outcome, errorClass string
		overallTimeout                   int64
	}{
		{"client cancelled", ClassCancelled, "cancelled", "client_cancelled", 5000},
		{"overall timeout", ClassTimeout, "failure", "gateway_timeout", 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
				for id, provider := range snapshot.Providers {
					provider.Kind, provider.ProfileID, provider.ProfileRevision, provider.VendorID = "bedrock", "bedrock-converse", connectors.ProfileRevision, ""
					provider.CloudRegion, provider.AuthMode = "us-east-1", "static"
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: "bedrock", Mode: "streaming"}}
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.OverallTimeout = tc.overallTimeout
				snapshot.Routes[routeSlug] = route
			})
			serveBedrock(t, h)
			started := bedrockEvent(t, "messageStart", map[string]any{"role": "assistant"})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				if err := eventstream.NewEncoder().Encode(w, *started); err != nil {
					t.Error(err)
					return
				}
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			resp := h.do(ctx, http.MethodPost, "/bedrock/model/team-chat/converse-stream", fullKey, []byte(`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":16}}`), nil)
			defer resp.Body.Close()
			if _, err := eventstream.NewDecoder().Decode(resp.Body, nil); err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("stream did not start: status=%d err=%v", resp.StatusCode, err)
			}
			if tc.class == ClassCancelled {
				cancel()
			} else {
				_, _ = io.ReadAll(resp.Body)
			}
			env := h.sink.last(t)
			if env.Outcome != tc.outcome || env.ErrorClass != tc.errorClass || len(env.Attempts) != 1 || env.Attempts[0].Class != tc.class {
				t.Fatalf("outcome=%q error=%q attempts=%+v", env.Outcome, env.ErrorClass, env.Attempts)
			}
		})
	}
	t.Run("terminal frame could not be delivered", func(t *testing.T) {
		h := newHarness(t, Config{})
		h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
			for id, provider := range snapshot.Providers {
				provider.Kind, provider.VendorID, provider.ProfileID, provider.ProfileRevision = "bedrock", "", "", ""
				provider.CloudRegion, provider.AuthMode = "us-east-1", "static"
				provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: "bedrock", Mode: "streaming"}}
				snapshot.Providers[id] = provider
			}
		})
		serveBedrock(t, h)
		var source bytes.Buffer
		for _, frame := range []*eventstream.Message{
			bedrockMetadata(`{"usage":{"inputTokens":2,"outputTokens":3,"totalTokens":5}}`),
			bedrockEvent(t, "messageStop", map[string]string{"stopReason": "end_turn"}),
		} {
			if err := eventstream.NewEncoder().Encode(&source, *frame); err != nil {
				t.Fatal(err)
			}
		}
		h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(source.Bytes())
		})
		req := httptest.NewRequest(http.MethodPost, "/bedrock/model/team-chat/converse-stream", strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":16}}`))
		req.SetPathValue("model", routeSlug)
		req.Header.Set("Authorization", "Bearer "+fullKey)
		req.Header.Set("Content-Type", "application/json")
		writer := &bedrockFrameWriter{ResponseRecorder: httptest.NewRecorder()}
		BedrockConverseStream(h.gateway)(writer, req)
		h.sink.mu.Lock()
		defer h.sink.mu.Unlock()
		if len(h.sink.envs) != 1 {
			t.Fatalf("terminal envelopes=%d, want 1", len(h.sink.envs))
		}
		env := h.sink.envs[0]
		if env.Outcome != "cancelled" || env.ErrorClass != "client_delivery_failed" || !env.Committed || len(env.Attempts) != 1 {
			t.Fatalf("terminal envelope=%+v", env)
		}
		attempt := env.Attempts[0]
		if attempt.Class != ClassSuccess || !attempt.UsageComplete || attempt.Usage == nil || attempt.Usage.InputTokens != 2 || attempt.Usage.OutputTokens != 3 {
			t.Fatalf("validated provider result was lost: %+v", attempt)
		}
	})
}

type bedrockFrameWriter struct {
	*httptest.ResponseRecorder
}

func (w *bedrockFrameWriter) Write(body []byte) (int, error) {
	if bytes.Contains(body, []byte("messageStop")) {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(body)
}

func TestModerationRejectsIncompleteDecisions(t *testing.T) {
	const valid = `{"flagged":false,"categories":{"fixture":false},"category_scores":{"fixture":0.01}}`
	const batch = `["first","second"]`
	for _, tc := range []struct {
		name, input, results string
		want                 int
	}{
		{"complete batch", batch, valid + "," + valid, http.StatusOK},
		{"single text", `"first"`, valid, http.StatusOK},
		{"joint multimodal", `[{"type":"text","text":"first"},{"type":"text","text":"second"}]`, valid, http.StatusOK},
		{"missing result", batch, valid, http.StatusBadGateway},
		{"extra result", batch, valid + "," + valid + "," + valid, http.StatusBadGateway},
		{"null flag", batch, `{"flagged":null,"categories":{"fixture":false},"category_scores":{"fixture":0.01}},` + valid, http.StatusBadGateway},
		{"null category", batch, `{"flagged":false,"categories":{"fixture":null},"category_scores":{"fixture":0.01}},` + valid, http.StatusBadGateway},
		{"null score", batch, `{"flagged":false,"categories":{"fixture":false},"category_scores":{"fixture":null}},` + valid, http.StatusBadGateway},
		{"mismatched categories", batch, `{"flagged":false,"categories":{"fixture":false},"category_scores":{"other":0.01}},` + valid, http.StatusBadGateway},
		{"empty categories", batch, `{"flagged":false,"categories":{},"category_scores":{}},` + valid, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
				for id, provider := range snapshot.Providers {
					provider.Kind = "openai"
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "moderation", Surface: "openai", Mode: "unary"}}
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.Operations = []string{"moderation"}
				route.MaxAttempts = 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, `{"id":"modr-fixture","model":"model-a","results":[`+tc.results+`]}`))
			resp := h.do(t.Context(), http.MethodPost, "/v1/moderations", fullKey, []byte(`{"model":"team-chat","input":`+tc.input+`}`), nil)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d, body %s", resp.StatusCode, tc.want, body)
			}
			if tc.want == http.StatusBadGateway && !strings.Contains(string(body), `"code":"provider_protocol_error"`) {
				t.Fatalf("unexpected rejection %s", body)
			}
			if tc.want == http.StatusOK {
				var result struct {
					Results json.RawMessage `json:"results"`
					Model   string          `json:"model"`
				}
				if json.Unmarshal(body, &result) != nil || string(result.Results) != "["+tc.results+"]" || result.Model != routeSlug {
					t.Fatalf("valid decisions changed %s", body)
				}
			}
		})
	}
}

func TestBraveSearchRejectsNonObjectResponses(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           int
	}{
		{"null", `null`, http.StatusBadGateway},
		{"array", `[]`, http.StatusBadGateway},
		{"string", `"not a result"`, http.StatusBadGateway},
		{"boolean", `false`, http.StatusBadGateway},
		{"number", `1`, http.StatusBadGateway},
		{"omitted web", `{"type":"search","query":{"original":"test"}}`, http.StatusOK},
		{"empty results", `{"type":"search","web":{"results":[]}}`, http.StatusOK},
		{"null web", `{"type":"search","web":null}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
				for id, provider := range snapshot.Providers {
					provider.VendorID = "brave"
					provider.ProfileID, provider.ProfileRevision = "brave-search", connectors.ProfileRevision
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "search", Surface: "openai", Mode: "unary"}}
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.Operations, route.MaxAttempts = []string{"search"}, 1
				snapshot.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, tc.response))
			resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(`{"query":"test","max_results":1}`), map[string]string{"X-OLP-Route": routeSlug})
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("provider=%s status=%d body=%s", tc.response, resp.StatusCode, body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusBadGateway && !strings.Contains(string(body), `"code":"fidelity_protocol_violation"`) {
				t.Fatalf("unexpected rejection: %s", body)
			}
		})
	}
}

func TestStrictPreparationKeepsTargetsAndSourcesSeparate(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "generation"
		configure := (func(*routing.Snapshot))(nil)
		if native {
			name, configure = "native operation", strictEmbeddings
		}
		t.Run(name, func(t *testing.T) {
			h := strictHarness(t, configure)
			route := h.rt.release.Snapshot().Route(routeSlug)
			target := route.Targets[0]
			provider := h.rt.release.Snapshot().Provider(target.ProviderID)
			x := &Call{family: wire.Chat, routeLeg: routeLeg{Route: &route}}
			x.request.release = h.rt.release
			x.authority = h.rt.keys[fullKey]
			for _, text := range []string{"first", "second"} {
				body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, routeSlug, text))
				if native {
					body = []byte(fmt.Sprintf(`{"model":%q,"input":%q}`, routeSlug, text))
					if x.unary == nil {
						selected, err := h.gateway.selectUnary(x, wire.Embeddings, "", body, "", "")
						if err != nil || !selected {
							t.Fatalf("select operation = %t, error = %v", selected, err)
						}
					} else {
						source, err := unary.Parse("openai-embeddings", body, 64<<10)
						if err != nil {
							t.Fatal(err)
						}
						x.unary.source = source
					}
				} else {
					parsed, err := protocols.Parse(wire.Chat, body, routeSlug)
					if err != nil {
						t.Fatal(err)
					}
					x.generation.parsed = parsed
				}
				if _, err := x.prepareCandidate(&provider, target.ID, target.ProviderModel); err != nil {
					t.Fatal(err)
				}
				if _, err := x.prepareCandidate(&provider, "missing-target", target.ProviderModel); err == nil {
					t.Fatal("a target without a compiled contract reused another target's preparation")
				}
				var preparedBody []byte
				if native {
					plan, err := x.unaryPlan(&provider, target.ID, target.ProviderModel)
					if err != nil {
						t.Fatal(err)
					}
					preparedBody = plan.Body()
				} else {
					prepared, err := x.generationRequest(&provider, target.ID, target.ProviderModel)
					if err != nil {
						t.Fatal(err)
					}
					preparedBody = prepared.document.Bytes()
				}
				if !bytes.Contains(preparedBody, []byte(text)) {
					t.Fatalf("prepared request does not contain current input %q: %s", text, preparedBody)
				}
			}
		})
	}
}

func TestRealtimeCallAdmissionHonorsRequestedPriority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, defaultPriority, header, expected string }{
		{"caller critical", "normal", `{"priority":"critical"}`, "critical"},
		{"key critical", "critical", "", "critical"},
		{"normal", "normal", "", "normal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := concurrency.NewPool(1).Queue(2, 2*time.Second)
			management := concurrency.NewPool(1)
			outer := &concurrency.Public{Inference: pool, Management: management, InferenceEnabled: true, Reject: func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(503) }}
			h := newHarnessWrapped(t, Config{}, outer.Wrap)
			critical := "critical"
			authority := h.rt.keys[fullKey]
			authority.Policy.MaxPriority = &critical
			authority.Policy.Priority = &test.defaultPriority
			h.rt.keys[fullKey] = authority
			registry := prometheus.NewRegistry()
			registry.MustRegister(concurrency.Metrics(pool, management))
			depth := func() map[string]float64 {
				families, e := registry.Gather()
				if e != nil {
					t.Fatal(e)
				}
				out := map[string]float64{}
				for _, f := range families {
					if f.GetName() != "olp_admission_queue_depth" {
						continue
					}
					for _, m := range f.Metric {
						for _, label := range m.Label {
							if label.GetName() == "class" {
								out[label.GetValue()] = m.GetGauge().GetValue()
							}
						}
					}
				}
				return out
			}
			holder := pool.Enter()
			defer holder.Release()
			req, e := http.NewRequest("POST", h.server.URL+"/v1/realtime/calls?model="+routeSlug, strings.NewReader("v=0\r\n"))
			if e != nil {
				t.Fatal(e)
			}
			req.Header.Set("Authorization", "Bearer "+fullKey)
			req.Header.Set("Content-Type", "application/sdp")
			if test.header != "" {
				req.Header.Set("X-OLP-Routing", test.header)
			}
			_, resolved, controlErr := routingControls(req, authority.Policy)
			if controlErr != nil || resolved != test.expected {
				t.Fatalf("fixture priority=%s err=%v", resolved, controlErr)
			}
			done := make(chan error, 1)
			go func() {
				r, e := h.server.Client().Do(req)
				if e == nil {
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
				}
				done <- e
			}()
			var observed map[string]float64
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				observed = depth()
				if observed["normal"]+observed["critical"] > 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			holder.Release()
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			t.Logf("priority=%s queued depths=%v", test.expected, observed)
			if pool.Admitted() != 0 {
				t.Fatalf("admission permit leaked: %d", pool.Admitted())
			}
			h.mock.mu.Lock()
			calls := len(h.mock.calls)
			h.mock.mu.Unlock()
			if calls != 0 {
				t.Fatal("unexpected provider dispatch")
			}
			if observed[test.expected] != 1 {
				t.Fatalf("request entered wrong admission class: %v", observed)
			}
		})
	}
}
