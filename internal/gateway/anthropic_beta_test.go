package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

// anthropicTransformedHarness serves provider "a" as an automatic Anthropic
// provider, without a profile, behind the transformed route of newHarness.
func anthropicTransformedHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, Config{})
	snapshot := h.rt.release.Snapshot
	provider := providerByName(h, "a")
	provider.Kind, provider.VendorID = "anthropic", "anthropic"
	provider.Capabilities = []runtime.Capability{}
	for _, surface := range []string{"openai", "anthropic"} {
		provider.Capabilities = append(provider.Capabilities,
			runtime.Capability{Model: modelA, Operation: "generation", Surface: surface, Mode: "unary"},
			runtime.Capability{Model: modelA, Operation: "generation", Surface: surface, Mode: "streaming"})
	}
	provider.Capabilities = append(provider.Capabilities, runtime.Capability{Model: modelA, Operation: "token_count", Surface: "anthropic", Mode: "unary"})
	snapshot.Providers[provider.ID] = provider
	route := snapshot.Routes[routeSlug]
	route.Operations = append(route.Operations, "token_count")
	// Only the Anthropic provider can serve the route, so a request has nowhere to fail over to.
	route.Targets = route.Targets[:1]
	snapshot.Routes[routeSlug] = route
	return h
}

// The fields of an Anthropic beta travel with its header. A transformed route
// hands an Anthropic upstream the caller's own Messages request, native fields
// included, so the header goes with it.
func TestTransformedRoutesForwardAnthropicBetaToAnthropicUpstreams(t *testing.T) {
	const betas = "claude-code-20250219,context-management-2025-06-27,effort-2025-11-24"
	body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}}`
	for _, tc := range []struct {
		name, path, body string
		headers          map[string]string
		wantBeta         string
	}{
		{"messages", messagesPath, body, map[string]string{"Anthropic-Beta": betas}, betas},
		{"streaming", messagesPath, strings.Replace(body, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1), map[string]string{"Anthropic-Beta": betas}, betas},
		{"count_tokens", countPath, `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"Anthropic-Beta": "token-counting-2024-11-01"}, "token-counting-2024-11-01"},
		{"no header", messagesPath, body, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := anthropicTransformedHarness(t)
			log := recordAnthropic(h)
			headers := map[string]string{"Anthropic-Version": "2023-06-01"}
			for name, value := range tc.headers {
				headers[name] = value
			}
			resp := h.do(t.Context(), http.MethodPost, tc.path+"?beta=true", fullKey, []byte(tc.body), headers)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			got := log.last(t)
			if got.Beta != tc.wantBeta {
				t.Fatalf("the upstream received Anthropic-Beta %q, want %q", got.Beta, tc.wantBeta)
			}
			// A caller that sent no betas leaves the header out, rather than sending it empty.
			if wantLines := map[bool]int{true: 1, false: 0}[tc.wantBeta != ""]; len(got.BetaLines) != wantLines {
				t.Fatalf("the upstream received the Anthropic-Beta lines %q, want %d", got.BetaLines, wantLines)
			}
			if tc.path == messagesPath && !strings.Contains(string(got.Body), `"context_management"`) {
				t.Fatalf("the native field did not arrive: %s", got.Body)
			}
		})
	}
}

// Header lines of one name are one list, and the Anthropic SDK for Go sends a
// beta per line.
func TestTransformedRoutesFoldRepeatedAnthropicBetaLines(t *testing.T) {
	h := anthropicTransformedHarness(t)
	log := recordAnthropic(h)
	body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+messagesPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Add("Anthropic-Beta", "first-2025-01-01")
	req.Header.Add("Anthropic-Beta", "second-2025-02-02")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := log.last(t).Beta; got != "first-2025-01-01,second-2025-02-02" {
		t.Fatalf("the upstream received Anthropic-Beta %q", got)
	}
}

// A request translated from another dialect carries no Anthropic semantics, so
// its Anthropic-Beta header is neither forwarded nor held to the limit of one
// that is.
func TestTransformedRoutesKeepAnthropicBetaFromOtherDialects(t *testing.T) {
	for _, tc := range []struct{ name, beta string }{
		{"a list", "context-management-2025-06-27"},
		{"a list too long to forward", strings.Repeat("a", maxAnthropicBeta+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := anthropicTransformedHarness(t)
			log := recordAnthropic(h)
			chat := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(chat), map[string]string{"Anthropic-Beta": tc.beta})
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			got := log.last(t)
			if len(got.BetaLines) != 0 {
				t.Fatalf("a translated request carried Anthropic-Beta %q upstream", got.BetaLines)
			}
			var sent struct{ Messages []struct{ Role string } }
			if err := json.Unmarshal(got.Body, &sent); err != nil || len(sent.Messages) != 1 {
				t.Fatalf("the upstream did not receive an Anthropic request: %s", got.Body)
			}
		})
	}
}

// vendorTransformedHarness serves provider "a", as a provider of the given kind
// that speaks the Anthropic surface, as the only target of the transformed route.
func vendorTransformedHarness(t *testing.T, kind string) *harness {
	t.Helper()
	h := newHarness(t, Config{})
	provider := providerByName(h, "a")
	provider.Kind = kind
	provider.Capabilities = append(provider.Capabilities,
		runtime.Capability{Model: modelA, Operation: "generation", Surface: "anthropic", Mode: "unary"},
		runtime.Capability{Model: modelA, Operation: "generation", Surface: "anthropic", Mode: "streaming"})
	h.rt.release.Snapshot.Providers[provider.ID] = provider
	route := h.rt.release.Snapshot.Routes[routeSlug]
	route.Targets = route.Targets[:1]
	h.rt.release.Snapshot.Routes[routeSlug] = route
	return h
}

// The Anthropic-Beta header is the Anthropic API's own. A request in the
// Anthropic dialect that a route translates for another vendor's API does not
// carry it there.
func TestAnthropicBetaIsNotSentToOtherVendors(t *testing.T) {
	const geminiAnswer = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`
	for _, tc := range []struct {
		kind  string
		reply http.HandlerFunc
	}{
		{"openai", completion(modelA, answerText)},
		{"gemini", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, geminiAnswer)
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			h := vendorTransformedHarness(t, tc.kind)
			log := recordUpstream(h, tc.reply)
			body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			resp := h.do(t.Context(), http.MethodPost, messagesPath, fullKey, []byte(body), map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": "context-management-2025-06-27"})
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			if got := log.last(t); len(got.BetaLines) != 0 {
				t.Fatalf("the %s upstream received Anthropic-Beta %q", tc.kind, got.BetaLines)
			}
		})
	}
}

// A header that cannot be forwarded refuses the request: dropping it would
// send the beta fields of the body without their header.
func TestTransformedRoutesRefuseAnAnthropicBetaTheyCannotForward(t *testing.T) {
	h := anthropicTransformedHarness(t)
	recordAnthropic(h)
	body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"context_management":{}}`
	resp := h.do(t.Context(), http.MethodPost, messagesPath, fullKey, []byte(body), map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": strings.Repeat("a", maxAnthropicBeta+1)})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "anthropic-beta") || h.mock.count("a") != 0 {
		t.Fatalf("status=%d body=%s dispatches=%d", resp.StatusCode, raw, h.mock.count("a"))
	}
	// The longest list that fits is forwarded.
	ok := strings.Repeat("a", maxAnthropicBeta)
	resp = h.do(t.Context(), http.MethodPost, messagesPath, fullKey, []byte(body), map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": ok})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 {
		t.Fatalf("status=%d dispatches=%d", resp.StatusCode, h.mock.count("a"))
	}
}

// The header is validated only where it could be forwarded: a route whose
// targets are all other vendors never sends it, so it cannot be half of a pair.
func TestAnthropicBetaIsNotValidatedWhereItIsNeverForwarded(t *testing.T) {
	h := newHarness(t, Config{})
	for _, name := range []string{"a", "b"} {
		provider := providerByName(h, name)
		provider.Kind = "openai"
		provider.Capabilities = append(provider.Capabilities, runtime.Capability{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: "anthropic", Mode: "unary"})
		h.rt.release.Snapshot.Providers[provider.ID] = provider
	}
	body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp := h.do(t.Context(), http.MethodPost, messagesPath, fullKey, []byte(body), map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": strings.Repeat("a", maxAnthropicBeta+1)})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 {
		t.Fatalf("status=%d body=%s dispatches=%d", resp.StatusCode, raw, h.mock.count("a"))
	}
}

// profiledConnector is a provider of the given kind configured with a profile.
func profiledConnector(kind, profile string) connectors.Config {
	return connectors.Config{Kind: kind, ProfileID: profile, ProfileRevision: connectors.ProfileRevision}
}

// A provider takes the header when it is an Anthropic provider, or when its
// profile declares it a semantic header. Bedrock InvokeModel declares none.
func TestAnthropicBetaIsTakenByProvidersThatDeclareIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  connectors.Config
		want bool
	}{
		{"an automatic Anthropic provider", connectors.Config{Kind: "anthropic"}, true},
		{"the Anthropic Messages profile", profiledConnector("anthropic", "anthropic-messages"), true},
		{"Claude on Vertex AI", profiledConnector("vertex_ai", "vertex-anthropic"), true},
		{"Claude on Bedrock InvokeModel", profiledConnector("bedrock", "bedrock-anthropic-invoke"), false},
		{"an automatic OpenAI provider", connectors.Config{Kind: "openai"}, false},
		{"an automatic Gemini provider", connectors.Config{Kind: "gemini"}, false},
		{"Gemini on Vertex AI", profiledConnector("vertex_ai", "vertex-gemini"), false},
		{"Bedrock Converse", profiledConnector("bedrock", "bedrock-converse"), false},
		{"a profile that does not exist", profiledConnector("anthropic", "no-such-profile"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := takesAnthropicBeta(tc.cfg); got != tc.want {
				t.Fatalf("takesAnthropicBeta=%v, want %v", got, tc.want)
			}
			// The check that refuses a header and the forwarding of it agree.
			h := newHarness(t, Config{})
			provider := providerByName(h, "a")
			provider.Kind, provider.ProfileID, provider.ProfileRevision = tc.cfg.Kind, tc.cfg.ProfileID, tc.cfg.ProfileRevision
			h.rt.release.Snapshot.Providers[provider.ID] = provider
			route := h.rt.release.Snapshot.Routes[routeSlug]
			route.Targets = route.Targets[:1]
			x := &execution{request: request{release: h.rt.release}, route: &route}
			if got := x.servesAnthropicBeta(); got != tc.want {
				t.Fatalf("servesAnthropicBeta=%v, want %v", got, tc.want)
			}
		})
	}
}

// A route with one provider that takes the header refuses one it cannot
// forward, whichever its other targets are.
func TestTransformedRoutesRefuseAnAnthropicBetaForAnyTargetThatTakesIt(t *testing.T) {
	const tooLong = maxAnthropicBeta + 1
	body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"context_management":{}}`
	for _, tc := range []struct {
		name          string
		kind, profile string
		refused       bool
	}{
		{"Claude on Vertex AI", "vertex_ai", "vertex-anthropic", true},
		{"Claude on Bedrock InvokeModel", "bedrock", "bedrock-anthropic-invoke", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := vendorTransformedHarness(t, tc.kind)
			provider := providerByName(h, "a")
			provider.ProfileID, provider.ProfileRevision = tc.profile, connectors.ProfileRevision
			h.rt.release.Snapshot.Providers[provider.ID] = provider
			resp := h.do(t.Context(), http.MethodPost, messagesPath, fullKey, []byte(body), map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": strings.Repeat("a", tooLong)})
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if refused := resp.StatusCode == http.StatusBadRequest && strings.Contains(string(raw), `"anthropic-beta"`); refused != tc.refused {
				t.Fatalf("refused=%v, want %v: status=%d body=%s", refused, tc.refused, resp.StatusCode, raw)
			}
		})
	}
}

// What forwardAnthropicBeta decides, by the request's dialect, the dialect it
// is sent in, and the provider it is sent to.
func TestForwardAnthropicBetaNeedsTheCallersOwnAnthropicRequestAndAProviderThatTakesIt(t *testing.T) {
	anthropicProvider := connectors.Config{Kind: "anthropic"}
	for _, tc := range []struct {
		name         string
		source, wire openai.Family
		cfg          connectors.Config
		beta         []string
		want         string
	}{
		{"the callers Messages request to an Anthropic provider", openai.FamilyAnthropic, openai.FamilyAnthropic, anthropicProvider, []string{"a-2025-01-01"}, "a-2025-01-01"},
		{"the callers count_tokens request", openai.FamilyAnthropicCount, openai.FamilyAnthropicCount, anthropicProvider, []string{"a-2025-01-01"}, "a-2025-01-01"},
		{"a beta per line", openai.FamilyAnthropic, openai.FamilyAnthropic, anthropicProvider, []string{"a-2025-01-01", "b-2025-02-02"}, "a-2025-01-01,b-2025-02-02"},
		{"Claude on Vertex AI", openai.FamilyAnthropic, openai.FamilyAnthropic, profiledConnector("vertex_ai", "vertex-anthropic"), []string{"a-2025-01-01"}, "a-2025-01-01"},
		{"Claude on Bedrock InvokeModel", openai.FamilyAnthropic, openai.FamilyAnthropic, profiledConnector("bedrock", "bedrock-anthropic-invoke"), []string{"a-2025-01-01"}, ""},
		{"a Messages request sent to OpenAI", openai.FamilyAnthropic, openai.FamilyChat, connectors.Config{Kind: "openai"}, []string{"a-2025-01-01"}, ""},
		{"a Messages request sent to Gemini", openai.FamilyAnthropic, openai.FamilyGemini, connectors.Config{Kind: "gemini"}, []string{"a-2025-01-01"}, ""},
		{"a Messages request sent in another dialect to an Anthropic provider", openai.FamilyAnthropic, openai.FamilyChat, anthropicProvider, []string{"a-2025-01-01"}, ""},
		{"a Chat Completions request translated to Anthropic", openai.FamilyChat, openai.FamilyAnthropic, anthropicProvider, []string{"a-2025-01-01"}, ""},
		{"a Responses request translated to Anthropic", openai.FamilyResponses, openai.FamilyAnthropic, anthropicProvider, []string{"a-2025-01-01"}, ""},
		{"a Chat Completions request that no dialect change touches", openai.FamilyChat, openai.FamilyChat, anthropicProvider, []string{"a-2025-01-01"}, ""},
		{"a header too long to forward", openai.FamilyAnthropic, openai.FamilyAnthropic, anthropicProvider, []string{strings.Repeat("a", maxAnthropicBeta+1)}, ""},
		{"a header that is not a valid value", openai.FamilyAnthropic, openai.FamilyAnthropic, anthropicProvider, []string{"a\r\nInjected: 1"}, ""},
		{"no header", openai.FamilyAnthropic, openai.FamilyAnthropic, anthropicProvider, nil, ""},
		{"an empty header", openai.FamilyAnthropic, openai.FamilyAnthropic, anthropicProvider, []string{""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &execution{parsed: &openai.Request{Family: tc.source}, semanticHeaders: semanticHeaders(http.Header{"Anthropic-Beta": tc.beta})}
			header := http.Header{}
			forwardAnthropicBeta(header, x, tc.wire, tc.cfg)
			if tc.want == "" {
				if _, present := header["Anthropic-Beta"]; present {
					t.Fatalf("Anthropic-Beta %q was sent", header["Anthropic-Beta"])
				}
			} else if lines := header.Values("Anthropic-Beta"); len(lines) != 1 || lines[0] != tc.want {
				t.Fatalf("Anthropic-Beta %q was sent, want %q", lines, tc.want)
			}
		})
	}
}

// A strict route binds the header through its profile, which refuses one it
// cannot forward in its own terms.
func TestStrictRoutesLeaveAnAnthropicBetaToTheirProfile(t *testing.T) {
	h := anthropicStrictHarness(t)
	log := recordAnthropic(h)
	body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"context_management":{}}`
	resp := h.do(t.Context(), http.MethodPost, messagesPath, fullKey, []byte(body), map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": strings.Repeat("a", maxAnthropicBeta+1)})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "target_capability") || strings.Contains(string(raw), `"anthropic-beta"`) || h.mock.count("a") != 0 {
		t.Fatalf("status=%d body=%s dispatches=%d", resp.StatusCode, raw, h.mock.count("a"))
	}
	// The longest list that fits is forwarded as it is.
	ok := strings.Repeat("a", maxAnthropicBeta)
	resp = h.do(t.Context(), http.MethodPost, messagesPath, fullKey, []byte(body), map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": ok})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || log.last(t).Beta != ok {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}
