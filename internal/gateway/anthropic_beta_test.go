package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/connectors"
)

// anthropicTransformedHarness serves provider "a" as an automatic Anthropic
// provider, without a profile, behind the transformed route of newHarness.
func anthropicTransformedHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, Config{})
	snapshot := h.rt.release.Snapshot
	provider := providerByName(h, "a")
	provider.Kind, provider.VendorID = "anthropic", "anthropic"
	provider.Capabilities = []routing.Capability{}
	for _, surface := range []string{"openai", "anthropic"} {
		provider.Capabilities = append(provider.Capabilities,
			routing.Capability{Model: modelA, Operation: "generation", Surface: surface, Mode: "unary"},
			routing.Capability{Model: modelA, Operation: "generation", Surface: surface, Mode: "streaming"})
	}
	provider.Capabilities = append(provider.Capabilities, routing.Capability{Model: modelA, Operation: "token_count", Surface: "anthropic", Mode: "unary"})
	snapshot.Providers[provider.ID] = provider
	route := snapshot.Routes[routeSlug]
	route.Operations = append(route.Operations, "token_count")
	// Only the Anthropic provider can serve the route, so a request has nowhere to fail over to.
	route.Targets = route.Targets[:1]
	snapshot.Routes[routeSlug] = route
	return h
}

// A caller that sent no betas leaves the header out, rather than sending it empty.

// vendorTransformedHarness serves provider "a", as a provider of the given kind
// that speaks the Anthropic surface, as the only target of the transformed route.
func vendorTransformedHarness(t *testing.T, kind string) *harness {
	t.Helper()
	h := newHarness(t, Config{})
	provider := providerByName(h, "a")
	provider.Kind = kind
	provider.Capabilities = append(provider.Capabilities,
		routing.Capability{Model: modelA, Operation: "generation", Surface: "anthropic", Mode: "unary"},
		routing.Capability{Model: modelA, Operation: "generation", Surface: "anthropic", Mode: "streaming"})
	h.rt.release.Snapshot.Providers[provider.ID] = provider
	route := h.rt.release.Snapshot.Routes[routeSlug]
	route.Targets = route.Targets[:1]
	h.rt.release.Snapshot.Routes[routeSlug] = route
	return h
}

// The longest list that fits is forwarded.

// profiledConnector is a provider of the given kind configured with a profile.
func profiledConnector(kind, profile string) connectors.Config {
	return connectors.Config{Kind: kind, ProfileID: profile, ProfileRevision: connectors.ProfileRevision}
}

// The check that refuses a header and the forwarding of it agree.

// publishing is a connector that publishes a list of Anthropic betas of its own.
func publishing(cfg connectors.Config, betas string) connectors.Config {
	cfg.SemanticHeaders = map[string]string{"Anthropic-Beta": betas}
	return cfg
}

// A provider that publishes betas sends them with the caller's.

// The upstream receives what hosting leaves: the provider's published
// headers are applied to the request as it is hosted.

// The connector the caller handed in still publishes what it did.

// anthropicPublishingHarness serves provider "a" as the Anthropic Messages
// profile behind the transformed route, with the Anthropic-Beta header published
// among its semantic headers, as an operator sends a beta with every request.
func anthropicPublishingHarness(t *testing.T, betas string) *harness {
	t.Helper()
	h := anthropicTransformedHarness(t)
	provider := providerByName(h, "a")
	provider.ProfileID, provider.ProfileRevision = "anthropic-messages", connectors.ProfileRevision
	provider.SemanticHeaders = map[string]string{"Anthropic-Beta": betas}
	h.rt.release.Snapshot.Providers[provider.ID] = provider
	return h
}

// A provider that publishes Anthropic-Beta sends it with the caller's own list,
// which the fields of the caller's request need: replacing the caller's betas
// would send the upstream the beta fields of the body without their header.
func TestTransformedRoutesSendTheCallersAnthropicBetaWithThePublishedOnes(t *testing.T) {
	const published = "operator-feature-1"
	body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}}`
	for _, tc := range []struct {
		name, path, body, caller, want string
	}{
		{"no header", messagesPath, body, "", published},
		{"a beta of the callers", messagesPath, body, "context-management-2025-06-27", published + ",context-management-2025-06-27"},
		{"betas the provider publishes too", messagesPath, body, "caller-feature-2," + published, published + ",caller-feature-2"},
		{"a streaming request", messagesPath, strings.Replace(body, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1), "caller-feature-2", published + ",caller-feature-2"},
		{"count_tokens", countPath, `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]}`, "token-counting-2024-11-01", published + ",token-counting-2024-11-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := anthropicPublishingHarness(t, published)
			log := recordAnthropic(h)
			headers := map[string]string{"Anthropic-Version": "2023-06-01"}
			if tc.caller != "" {
				headers["Anthropic-Beta"] = tc.caller
			}
			resp := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(tc.body), headers)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			got := log.last(t)
			if len(got.BetaLines) != 1 || got.Beta != tc.want {
				t.Fatalf("the upstream received the Anthropic-Beta lines %q, want one line of %q", got.BetaLines, tc.want)
			}
			if tc.path == messagesPath && !strings.Contains(string(got.Body), `"context_management"`) {
				t.Fatalf("the native field did not arrive: %s", got.Body)
			}
		})
	}
}

// The caller's list fits alone and not beside the provider's.

// One that fits beside it is sent whole, up to the bound.

// The longest list that fits is forwarded as it is.
