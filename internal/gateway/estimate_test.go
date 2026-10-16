package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
)

// estimateTarget is one target of a route built for the estimation tests.
type estimateTarget struct {
	name, model string
	slots       int
}

// routeEstimateTargets replaces the harness route with one target per entry,
// each on a provider of its own with the given number of credential slots, all
// able to serve the Anthropic surface through the OpenAI wire, so a request in
// the Anthropic dialect is translated for every target.
func routeEstimateTargets(h *harness, targets ...estimateTarget) {
	h.t.Helper()
	snapshot := h.rt.release.Snapshot
	template := providerByName(h, "a")
	version := 1
	route := snapshot.Routes[routeSlug]
	route.Targets = nil
	route.MaxAttempts = 12
	snapshot.Providers = map[string]routing.Provider{}
	for i, target := range targets {
		provider := template
		provider.ID, provider.RevisionID, provider.Name = uuid.NewString(), uuid.NewString(), target.name
		provider.Kind = "openai"
		provider.Endpoint = h.upstream.URL + "/" + target.name + "/v1"
		provider.Capabilities = []routing.Capability{
			{Model: target.model, Operation: "generation", Surface: "anthropic", Mode: "unary"},
			{Model: target.model, Operation: "generation", Surface: "openai", Mode: "unary"},
		}
		provider.Slots = nil
		for range target.slots {
			provider.Slots = append(provider.Slots, routing.Slot{ID: uuid.NewString(), Name: "slot", Enabled: true, Weight: 1, CredentialID: &h.credA, CredentialVersion: &version})
		}
		snapshot.Providers[provider.ID] = provider
		route.Targets = append(route.Targets, routing.Target{ID: uuid.NewString(), ProviderID: provider.ID, ProviderModel: target.model, Priority: i, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()})
	}
	snapshot.Routes[routeSlug] = route
}

// countFamilies records every family the gateway counts a prompt for.
func countFamilies(h *harness) func() map[estimate.Family]int {
	var mu sync.Mutex
	counted := map[estimate.Family]int{}
	h.gateway.counted = func(f estimate.Family) {
		mu.Lock()
		defer mu.Unlock()
		counted[f]++
	}
	return func() map[estimate.Family]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[estimate.Family]int{}
		for f, n := range counted {
			out[f] = n
		}
		return out
	}
}

// A rate limit takes one credential slot out and moves on to the provider's
// next, where a server error leaves the provider for the next target, so the
// first two targets are tried through both their slots.

// The case is only worth something if a provider's second credential slot
// was reserved and dispatched too, not only one slot of each.

// Each attempt records the estimate it was admitted under, counted for the
// family of the model it was sent to and not the dialect the caller spoke.
// The OpenAI counts are tiktoken's: the system prompt "Be brief." is three
// tokens and "hello" one, "system" and "user" one each, two messages of
// three and the reply's priming of three. Claude has no public tokenizer:
// nine characters and five are three tokens and two.

// The event the accounting consumer receives carries them too: an
// annotation the event contract refuses is dropped without a word.

// One provider serving both.

// Twenty characters are five tokens to the heuristic, and more to o200k_base
// once the message is framed: the two families disagree.

// Three and one tokens of text, two roles, two messages and the reply.

// Replacing the request without clearing the summary still follows it.

// What a provider is sent can carry more than the caller wrote: here an
// image, which no tokenizer counts.

// The source is eight tokens and ten for the reply; the rewritten request
// adds an image.

// The other way round, the caller's request is the larger and the tokenizer
// alone counted it, so its provenance stands.

// What the two requests the Bedrock provider may be sent hold, each counted
// as the model counts it.

// The catalogue is in the count, and in it once.

// Whatever wire the tools travel in, a request is about as large.

// A model with a tokenizer reads a tool schema in a rendering of its own, so
// its count is calibrated, as it is for every other tool catalogue; one
// without says it is the heuristic.

// Three hundred tokens of o200k_base, and four hundred characters.

// A body-sized request records the size-based input, whatever the model.

// A lifecycle call, a realtime session or a job poll carries no prompt: no
// estimate is invented for it, and its family is recorded all the same.

// A walked request records the input alone, not the reply the reservation
// holds as well.

// Five characters are two tokens to the heuristic.

// The key holds the largest of the attempts it may make.

// The first run walks the prompt, and AllocsPerRun discards it.

// The caller's fifteen characters are four tokens to the four-character
// rule, which counts the model. The provider is sent four hundred and nine
// characters, a hundred and three tokens.

// A hundred and fifty-one tokens of o200k_base, which are a hundred and
// fifty-eight with the message framed, and nine hundred characters.

// TestRequestsReadBySizeRecordTheirSizeAsTheEstimate drives each handler that
// reserves a request by its size, and holds what its attempt records: four
// bytes to a token over the body, as a heuristic, for the model's family.
func TestRequestsReadBySizeRecordTheirSizeAsTheEstimate(t *testing.T) {
	t.Run("bedrock invoke", func(t *testing.T) {
		h := newHarness(t, Config{})
		for id, p := range h.rt.release.Snapshot.Providers {
			p.Kind = "bedrock"
			p.Capabilities = append(p.Capabilities, routing.Capability{Model: p.Capabilities[0].Model, Operation: "bedrock_invoke", Surface: "bedrock", Mode: "unary"})
			h.rt.release.Snapshot.Providers[id] = p
		}
		route := h.rt.release.Snapshot.Routes[routeSlug]
		route.Operations = append(route.Operations, "bedrock_invoke")
		h.rt.release.Snapshot.Routes[routeSlug] = route
		h.mock.set("a", status(200, `{}`))
		h.mock.set("b", status(200, `{}`))
		// The route is not qualified for the model, so the handler refuses the
		// result after the upstream has answered; the attempt was made.
		req := httptest.NewRequest(http.MethodPost, "/bedrock/model/"+routeSlug+"/invoke", strings.NewReader(`{"prompt":"hello there"}`))
		req.SetPathValue("model", routeSlug)
		req.Header.Set("Authorization", "Bearer "+fullKey)
		req.Header.Set("Content-Type", "application/json")
		h.gateway.bedrockInvoke(httptest.NewRecorder(), req)
		assertSizedAttempt(t, h, 24/4)
	})
	t.Run("gemini interaction", func(t *testing.T) {
		h := newHarness(t, Config{})
		for id, p := range h.rt.release.Snapshot.Providers {
			p.Kind, p.ProfileID, p.ProfileRevision, p.Endpoint = "gemini", "gemini-interactions", connectors.ProfileRevision, h.upstream.URL+"/v1beta"
			p.Capabilities = append(p.Capabilities, routing.Capability{Model: p.Capabilities[0].Model, Operation: "generation", Surface: "gemini", Mode: "unary"})
			h.rt.release.Snapshot.Providers[id] = p
		}
		// The mock tells providers apart by the first segment of the path, and an
		// Interactions endpoint is the origin and /v1beta.
		h.mock.set("v1beta", status(200, `{"id":"int_1","status":"completed","outputs":[]}`))
		body := `{"model":"` + routeSlug + `","input":"hello there","store":false}`
		resp := h.do(t.Context(), http.MethodPost, "/gemini/v1beta/interactions", "", []byte(body), map[string]string{"x-goog-api-key": fullKey})
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		assertSizedAttempt(t, h, int64(len(body))/4)
	})
	t.Run("json media", func(t *testing.T) {
		h := newMediaHarness(t)
		h.mock.set("a", status(200, `{"data":[{"url":"https://example.com/image.png"}]}`))
		h.mock.set("b", status(200, `{"data":[{"url":"https://example.com/image.png"}]}`))
		body := `{"model":"team-chat","prompt":"a lighthouse at dusk"}`
		resp := h.do(t.Context(), http.MethodPost, "/v1/images/generations", fullKey, []byte(body), nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		assertSizedAttempt(t, h, int64(len(body)+3)/4)
	})
}

// assertSizedAttempt checks the attempt the last request made recorded the size
// of its body as a heuristic estimate, for the family of the fixture model.
func assertSizedAttempt(t *testing.T, h *harness, want int64) {
	t.Helper()
	env := h.sink.last(t)
	if len(env.Attempts) != 1 {
		t.Fatalf("%d attempts", len(env.Attempts))
	}
	if got := env.Attempts[0]; got.EstimatedInputTokens != want || got.EstimateProvenance != "heuristic" || got.ModelFamily != "other" {
		t.Fatalf("the attempt recorded %d tokens from %q for %q, want %d from the heuristic for the other family", got.EstimatedInputTokens, got.EstimateProvenance, got.ModelFamily, want)
	}
}

// "Be brief." is three tokens and "hello there" two; "system" and
// "user" are one each, two messages are six and the reply three.
