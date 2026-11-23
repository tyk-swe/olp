package kernel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestGeminiDecisionRoute(t *testing.T) {
	t.Parallel()
	const path = "/gemini/v1beta/decisions"
	const request = `{"model":"team-chat","input":"The customer cannot sign in.","questions":{"access":{"type":"noul","instructions":"Is the customer locked out?"},"team":{"type":"choice","instructions":"Choose a team.","criteria":{"accounts":"Account issues","orders":"Order issues"}},"priority":{"type":"score","instructions":"Rate the priority.","criteria":["Routine","Urgent"]}}}`
	// Response fields follow https://aistudio.google.com/api-reference/decisions/create-decision (2026-11-17).
	const answer = `{"answers":{"access":{"type":"noul","noul":0.9,"confidence":0.9},"team":{"type":"choice","choice":"accounts","probabilities":{"accounts":0.8,"orders":0.2},"confidence":0.8},"priority":{"type":"score","score":0.7,"legend":{"0":"Routine","1":"Urgent"},"probabilities":{"0":0.3,"1":0.7},"confidence":0.7}},"usage":{"total_input_tokens":19,"total_output_tokens":0,"total_tokens":19,"input_tokens_by_modality":[{"modality":"text","tokens":19}]}}`
	input := func(value string) string { return strings.Replace(request, `"The customer cannot sign in."`, value, 1) }
	inline := input(`{"type":"image","data":"AA==","mime_type":"image/png"}`)
	remote := input(`{"type":"document","uri":"https://example.test/form.pdf","mime_type":"application/pdf"}`)
	splitText := input(`[{"type":"text","text":"sec"},{"type":"text","text":"ret"}]`)
	for _, tc := range []struct {
		name, body, result string
		reason             string
		want, calls        int
		withoutUsage       bool
		allowFetch         bool
		blockSecret        bool
	}{
		{name: "native answers and usage", body: request, result: answer, want: http.StatusOK, calls: 1},
		{name: "different answer key", body: request, result: strings.Replace(answer, `"access":`, `"other":`, 1), want: http.StatusBadGateway, reason: "fidelity_protocol_violation", calls: 1},
		{name: "invalid choice distribution", body: request, result: strings.Replace(answer, `"orders":0.2`, `"other":0.2`, 1), want: http.StatusBadGateway, reason: "fidelity_protocol_violation", calls: 1},
		{name: "missing usage", body: request, result: strings.SplitN(answer, `,"usage":`, 2)[0] + "}", want: http.StatusBadGateway, reason: "fidelity_protocol_violation", calls: 1},
		{name: "null usage", body: request, result: strings.SplitN(answer, `,"usage":`, 2)[0] + `,"usage":null}`, want: http.StatusBadGateway, reason: "fidelity_protocol_violation", calls: 1},
		{name: "inconsistent usage total", body: request, result: strings.Replace(answer, `"total_tokens":19`, `"total_tokens":20`, 1), want: http.StatusBadGateway, reason: "fidelity_protocol_violation", calls: 1},
		{name: "native safety feedback", body: request, result: `{"answers":{},"prompt_feedback":{"block_reason":"safety"},"usage":{"total_input_tokens":19,"total_output_tokens":0,"total_tokens":19}}`, want: http.StatusOK, calls: 1},
		{name: "native safety feedback without usage", body: request, result: `{"answers":{},"prompt_feedback":{"block_reason":"safety"}}`, want: http.StatusOK, calls: 1, withoutUsage: true},
		{name: "native safety feedback with inconsistent usage", body: request, result: `{"answers":{},"prompt_feedback":{"block_reason":"safety"},"usage":{"total_input_tokens":19,"total_output_tokens":0,"total_tokens":20}}`, want: http.StatusBadGateway, reason: "fidelity_protocol_violation", calls: 1},
		{name: "invalid question key", body: strings.Replace(request, `"access":`, `"bad key":`, 1), want: http.StatusBadRequest},
		{name: "inline input", body: inline, result: answer, want: http.StatusOK, calls: 1},
		{name: "inline input over byte limit", body: strings.Replace(inline, "AA==", "AAA=", 1), want: http.StatusBadRequest},
		// Processing options follow https://aistudio.google.com/api-reference/decisions/create-decision (2026-11-18).
		{name: "select an audio segment", body: input(`{"type":"audio","data":"AA==","mime_type":"audio/wav","processing":{"type":"static","start_offset":"2.5s","end_offset":"4s"}}`), result: answer, want: http.StatusOK, calls: 1},
		{name: "select a PDF page range", body: input(`{"type":"document","data":"AA==","mime_type":"application/pdf","processing":{"type":"static","page_options":{"start_page":1,"end_page":2,"content_layers":["page_text"]}}}`), result: answer, want: http.StatusOK, calls: 1},
		{name: "remote input needs opt in", body: remote, want: http.StatusBadRequest, reason: "content_egress"},
		{name: "remote input with opt in", body: remote, result: answer, want: http.StatusOK, calls: 1, allowFetch: true},
		{name: "input guardrail sees joined text", body: splitText, want: http.StatusBadRequest, reason: "guardrail_blocked", blockSecret: true},
		{name: "input guardrail refuses opaque media", body: inline, want: http.StatusUnprocessableEntity, reason: "guardrail_surface_unavailable", blockSecret: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{InlineMedia: protocols.InlineMediaLimits{Items: 1, ItemBytes: 1, TotalBytes: 1}})
			h.republish(func(snapshot *routing.Snapshot, a, _ routing.Provider) {
				a.Kind, a.VendorID = connectors.KindGemini, "google"
				a.Endpoint = h.upstream.URL + "/a/v1beta"
				a.ProfileID, a.ProfileRevision = "gemini-decisions", connectors.ProfileRevision
				a.Capabilities = []routing.Capability{{Model: modelA, Operation: "decision", Surface: "gemini", Mode: "unary"}}
				snapshot.Providers[a.ID] = a
				route := snapshot.Routes[routeSlug]
				route.Operations, route.Targets, route.MaxAttempts = []string{"decision"}, route.Targets[:1], 1
				route.ProviderFetchedDocuments = tc.allowFetch
				snapshot.Routes[routeSlug] = route
			})
			if tc.blockSecret {
				installTestGuardrail(t, h, guardrails.Input, guardrails.Block, "builtin.regex", `{"rules":[{"id":"secret","pattern":"secret"}]}`)
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/a/v1beta/decisions" || r.Header.Get("X-Goog-Api-Key") != secretA || r.Header.Get("Authorization") != "" {
					t.Errorf("incorrect upstream method, path, or credentials")
				}
				var got, want map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode upstream request: %v", err)
				}
				if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
					t.Errorf("decode expected request: %v", err)
				}
				if string(got["model"]) != `"`+modelA+`"` || string(got["questions"]) != string(want["questions"]) || string(got["input"]) != string(want["input"]) {
					t.Errorf("upstream request changed beyond model binding: %v", got)
				}
				status(http.StatusOK, tc.result)(w, r)
			})
			resp := h.do(t.Context(), http.MethodPost, path, "", []byte(tc.body), map[string]string{"X-Goog-Api-Key": fullKey})
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want || h.mock.count("a") != tc.calls {
				t.Fatalf("status %d, calls %d, body %s", resp.StatusCode, h.mock.count("a"), got)
			}
			if tc.reason != "" && !strings.Contains(string(got), `"reason":"`+tc.reason+`"`) {
				t.Fatalf("expected error reason %s, body %s", tc.reason, got)
			}
			if tc.want == http.StatusBadGateway {
				env := h.sink.await(t, 1)[0]
				if env.Usage != nil || len(env.Attempts) != 1 || env.Attempts[0].UsageObserved || !env.Attempts[0].BillingUncertain {
					t.Fatalf("invalid response was accounted as measured usage: %+v", env)
				}
			}
			if tc.want != http.StatusOK {
				return
			}
			if string(got) != tc.result {
				t.Fatalf("native response changed: %s", got)
			}
			env := h.sink.await(t, 1)[0]
			if env.Operation != "decision" || env.Surface != "gemini" {
				t.Fatalf("incorrect accounting envelope: %+v", env)
			}
			if tc.withoutUsage {
				if env.Usage != nil {
					t.Fatalf("safety response without usage recorded counters: %+v", env.Usage)
				}
				return
			}
			if env.Usage == nil || env.Usage.InputTokens != 19 || env.Usage.OutputTokens != 0 || env.Usage.TotalTokens != env.Usage.InputTokens {
				t.Fatalf("incorrect usage: %+v", env.Usage)
			}
		})
	}
}

func TestGeminiDecisionSafetyFallback(t *testing.T) {
	t.Parallel()
	const input = `{"model":"team-chat","input":"Example","questions":{"check":{"type":"noul","instructions":"Does the input contain a word?"}}}`
	const answer = `{"answers":{"check":{"type":"noul","noul":0.9,"confidence":0.9}},"usage":{"total_input_tokens":19,"total_output_tokens":0,"total_tokens":19}}`
	// The safety code and status follow https://aistudio.google.com/docs/decisions-api#request (2026-11-18).
	for _, tc := range []struct {
		code, reason string
		status       int
		fallbacks    int
	}{
		{code: "safety", status: http.StatusOK, fallbacks: 1},
		{code: "invalid_argument", reason: "upstream_rejected", status: http.StatusBadRequest},
	} {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{})
			h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
				route := snapshot.Routes[routeSlug]
				for i, provider := range []routing.Provider{a, b} {
					provider.Kind, provider.VendorID = connectors.KindGemini, "google"
					provider.Endpoint = h.upstream.URL + "/" + provider.Name + "/v1beta"
					provider.ProfileID, provider.ProfileRevision = "gemini-decisions", connectors.ProfileRevision
					provider.Capabilities = []routing.Capability{{Model: route.Targets[i].ProviderModel, Operation: "decision", Surface: "gemini", Mode: "unary"}}
					snapshot.Providers[provider.ID] = provider
				}
				route.Operations, route.MaxAttempts = []string{"decision"}, 2
				backup := routing.Route{
					ID: b.ID, RoutingID: b.ID, Slug: "backup-decisions", Operations: route.Operations,
					Targets: route.Targets[1:], MaxAttempts: 1, OverallTimeout: route.OverallTimeout,
					Fidelity: route.Fidelity,
				}
				route.Targets = route.Targets[:1]
				route.Fallbacks = []routing.Fallback{{Route: backup.Slug, On: []string{routing.FallbackContentFilter}}}
				snapshot.Routes[route.Slug], snapshot.Routes[backup.Slug] = route, backup
			})
			h.mock.set("a", status(http.StatusBadRequest, `{"error":{"code":"`+tc.code+`","message":"Request refused"}}`))
			h.mock.set("b", status(http.StatusOK, answer))
			resp := h.do(t.Context(), http.MethodPost, "/gemini/v1beta/decisions", fullKey, []byte(input), nil)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.status || h.mock.count("a") != 1 || h.mock.count("b") != tc.fallbacks {
				t.Fatalf("status %d, provider calls %d/%d, body %s", resp.StatusCode, h.mock.count("a"), h.mock.count("b"), body)
			}
			if tc.reason != "" && !strings.Contains(string(body), `"reason":"`+tc.reason+`"`) {
				t.Fatalf("expected %s, got %s", tc.reason, body)
			}
			if tc.status == http.StatusOK && string(body) != answer {
				t.Fatalf("expected the fallback answer, got %s", body)
			}
		})
	}
}
