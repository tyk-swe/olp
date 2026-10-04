package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
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
	snapshot.Providers = map[string]runtime.Provider{}
	for i, target := range targets {
		provider := template
		provider.ID, provider.RevisionID, provider.Name = uuid.NewString(), uuid.NewString(), target.name
		provider.Kind = "openai"
		provider.Endpoint = h.upstream.URL + "/" + target.name + "/v1"
		provider.Capabilities = []runtime.Capability{
			{Model: target.model, Operation: "generation", Surface: "anthropic", Mode: "unary"},
			{Model: target.model, Operation: "generation", Surface: "openai", Mode: "unary"},
		}
		provider.Slots = nil
		for range target.slots {
			provider.Slots = append(provider.Slots, runtime.Slot{ID: uuid.NewString(), Name: "slot", Enabled: true, Weight: 1, CredentialID: &h.credA, CredentialVersion: &version})
		}
		snapshot.Providers[provider.ID] = provider
		route.Targets = append(route.Targets, runtime.Target{ID: uuid.NewString(), ProviderID: provider.ID, ProviderModel: target.model, Priority: i, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()})
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

// TestEveryFamilyIsCountedOncePerRequest sends one request through a route
// that fails over across four targets on three tokenizer families, two of
// them rate limited on both of their two credential slots, translating the
// caller's Anthropic request for each. Planning, key admission, every slot's
// reservation and every attempt's record read the prompt, and each family is
// counted once.
func TestEveryFamilyIsCountedOncePerRequest(t *testing.T) {
	h := newHarness(t, Config{})
	routeEstimateTargets(h,
		estimateTarget{"a", "gpt-4o", 2},
		estimateTarget{"b", "gpt-4o-mini", 2},
		estimateTarget{"c", "gpt-4", 1},
		estimateTarget{"d", "claude-sonnet-4-5", 1},
	)
	// A rate limit takes one credential slot out and moves on to the provider's
	// next, where a server error leaves the provider for the next target, so the
	// first two targets are tried through both their slots.
	limited := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		status(http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)(w, r)
	}
	h.mock.set("a", limited)
	h.mock.set("b", limited)
	h.mock.set("c", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
	h.mock.set("d", completion("claude-sonnet-4-5", answerText))
	counted := countFamilies(h)

	resp := h.do(t.Context(), http.MethodPost, "/anthropic/v1/messages", fullKey,
		[]byte(`{"model":"`+routeSlug+`","max_tokens":16,"system":"Be brief.","messages":[{"role":"user","content":"hello"}]}`), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 6 || h.mock.count("a") != 2 || h.mock.count("b") != 2 || h.mock.count("c") != 1 || h.mock.count("d") != 1 {
		t.Fatalf("%d attempts (a %d, b %d, c %d, d %d), want both slots of a and b, then c and d", len(env.Attempts),
			h.mock.count("a"), h.mock.count("b"), h.mock.count("c"), h.mock.count("d"))
	}
	// The case is only worth something if a provider's second credential slot
	// was reserved and dispatched too, not only one slot of each.
	slots := map[string]map[string]bool{}
	for _, fact := range env.Attempts {
		if slots[fact.ProviderID] == nil {
			slots[fact.ProviderID] = map[string]bool{}
		}
		slots[fact.ProviderID][fact.SlotID] = true
	}
	multi := 0
	for _, used := range slots {
		if len(used) == 2 {
			multi++
		}
	}
	if multi != 2 {
		t.Fatalf("%d providers were attempted through both their slots, want 2: %+v", multi, env.Attempts)
	}

	if got, want := counted(), map[estimate.Family]int{estimate.FamilyOpenAIO200k: 1, estimate.FamilyOpenAICL100k: 1, estimate.FamilyAnthropic: 1}; len(got) != len(want) ||
		got[estimate.FamilyOpenAIO200k] != 1 || got[estimate.FamilyOpenAICL100k] != 1 || got[estimate.FamilyAnthropic] != 1 {
		t.Fatalf("families counted %v, want each of %v once", got, want)
	}

	// Each attempt records the estimate it was admitted under, counted for the
	// family of the model it was sent to and not the dialect the caller spoke.
	// The OpenAI counts are tiktoken's: the system prompt "Be brief." is three
	// tokens and "hello" one, "system" and "user" one each, two messages of
	// three and the reply's priming of three. Claude has no public tokenizer:
	// nine characters and five are three tokens and two.
	for _, fact := range env.Attempts {
		want := map[string]struct {
			family, provenance string
			input              int64
		}{
			"gpt-4o":            {"openai-o200k", "tokenizer", 15},
			"gpt-4o-mini":       {"openai-o200k", "tokenizer", 15},
			"gpt-4":             {"openai-cl100k", "tokenizer", 15},
			"claude-sonnet-4-5": {"anthropic", "heuristic", 5},
		}[fact.UpstreamModel]
		if fact.ModelFamily != want.family || fact.EstimateProvenance != want.provenance || fact.EstimatedInputTokens != want.input {
			t.Errorf("attempt %d on %s recorded %d tokens from %s for %s, want %d from %s for %s", fact.Ordinal, fact.UpstreamModel,
				fact.EstimatedInputTokens, fact.EstimateProvenance, fact.ModelFamily, want.input, want.provenance, want.family)
		}
	}

	// The event the accounting consumer receives carries them too: an
	// annotation the event contract refuses is dropped without a word.
	event := accountingEvent(env)
	if event == nil || len(event.Attempts) != len(env.Attempts) {
		t.Fatalf("event %+v", event)
	}
	for i, attempt := range event.Attempts {
		fact := env.Attempts[i]
		if attempt.EstimatedInputTokens == nil || *attempt.EstimatedInputTokens != fact.EstimatedInputTokens ||
			attempt.EstimateProvenance != fact.EstimateProvenance || attempt.ModelFamily != fact.ModelFamily {
			t.Errorf("attempt %d lost its estimate on the way to accounting: %+v", i, attempt)
		}
	}
}

// TestReservationFollowsTheTargetsFamily holds each attempt to the count of
// its own model: a provider that serves models of two families reserves each
// one's own estimate, and the key reserves the largest.
func TestReservationFollowsTheTargetsFamily(t *testing.T) {
	h := newHarness(t, Config{})
	routeEstimateTargets(h,
		estimateTarget{"a", "gpt-4o", 1},
		estimateTarget{"b", "claude-sonnet-4-5", 1},
	)
	var shared runtime.Provider
	for _, provider := range h.rt.release.Snapshot.Providers {
		if provider.Name == "a" {
			shared = provider
		}
	}
	route := h.rt.release.Snapshot.Routes[routeSlug]
	// One provider serving both.
	shared.Capabilities = append(shared.Capabilities, runtime.Capability{Model: "claude-sonnet-4-5", Operation: "generation", Surface: "openai", Mode: "unary"})
	h.rt.release.Snapshot.Providers[shared.ID] = shared
	route.Targets[1].ProviderID = shared.ID
	h.rt.release.Snapshot.Routes[routeSlug] = route

	// Twenty characters are five tokens to the heuristic, and more to o200k_base
	// once the message is framed: the two families disagree.
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","max_tokens":1,"messages":[{"role":"user","content":"`+strings.Repeat("日本語で", 5)+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{
		parsed:   parsed,
		request:  request{release: h.rt.release},
		route:    &route,
		attempts: []runtime.Attempt{{ProviderID: shared.ID, UpstreamModel: "gpt-4o"}, {ProviderID: shared.ID, UpstreamModel: "claude-sonnet-4-5"}},
	}
	o200k := estimate.Walk(parsed).Estimate(estimate.ForModel("gpt-4o"), nil).Tokens()
	heuristic := estimate.Walk(parsed).Estimate(estimate.ForModel("claude-sonnet-4-5"), nil).Tokens()
	if o200k == heuristic {
		t.Fatalf("the families agree on %d tokens; the case proves nothing", o200k)
	}
	if got := x.attemptReservation(x.attempts[0], &shared); got != o200k {
		t.Errorf("the OpenAI attempt reserves %d, want its own count %d", got, o200k)
	}
	if got := x.attemptReservation(x.attempts[1], &shared); got != heuristic {
		t.Errorf("the Claude attempt reserves %d, want its own count %d", got, heuristic)
	}
	if got := requestEstimate(x); got != max(o200k, heuristic) {
		t.Errorf("the key reserves %d, want the larger of %d and %d", got, o200k, heuristic)
	}
}

// TestPreparedTargetsCountTheSameTextOnce covers the targets a profile hands a
// request of their own: the request the provider is sent, in the dialect of
// its wire, reads the same as the caller's, so one count serves both. Two
// providers of one family and a repeat for each slot do not count again.
func TestPreparedTargetsCountTheSameTextOnce(t *testing.T) {
	h := newHarness(t, Config{})
	first := providerByName(h, "a")
	first.ProfileID, first.ProfileRevision = "compatible-chat", "1"
	first.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`5000`)}}}
	second := first
	second.ID, second.RevisionID = "other-provider", "other-revision"
	second.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`9000`)}}}
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counted := map[estimate.Family]int{}
	x := &execution{
		parsed: parsed,
		request: request{
			release: &runtime.Release{Snapshot: &runtime.Snapshot{Providers: map[string]runtime.Provider{first.ID: first, second.ID: second}}},
			counted: func(f estimate.Family) { mu.Lock(); counted[f]++; mu.Unlock() },
		},
		attempts: []runtime.Attempt{
			{ProviderID: first.ID, UpstreamModel: "gpt-4o"}, {ProviderID: second.ID, UpstreamModel: "gpt-4o-mini"},
			{ProviderID: first.ID, UpstreamModel: "gpt-4o"}, {ProviderID: second.ID, UpstreamModel: "gpt-4o"},
		},
	}
	for range 3 {
		for _, a := range x.attempts {
			provider := x.snapshot().Providers[a.ProviderID]
			if x.attemptReservation(a, &provider) == 0 {
				t.Fatal("no reservation")
			}
			prepared, err := x.preparedProvider(&provider, a.UpstreamModel)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.admitted.provenance != estimate.ProvenanceTokenizer || prepared.admitted.family != estimate.FamilyOpenAIO200k {
				t.Fatalf("prepared as %+v", prepared.admitted)
			}
			// Three and one tokens of text, two roles, two messages and the reply.
			if prepared.admitted.input != 3+1+1+1+6+3 {
				t.Fatalf("input %d, want 15", prepared.admitted.input)
			}
		}
		requestEstimate(x)
		x.sourceDemand("gpt-4o")
	}
	if len(counted) != 1 || counted[estimate.FamilyOpenAIO200k] != 1 {
		t.Fatalf("counted %v, want the one family once", counted)
	}
}

// TestRedactionCountsTheRedactedRequest keeps a count from outliving the
// request it was made for: the walk follows the request the gateway holds.
func TestRedactionCountsTheRedactedRequest(t *testing.T) {
	long, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","max_tokens":1,"messages":[{"role":"user","content":"`+strings.Repeat("a long message ", 20)+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	short, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","max_tokens":1,"messages":[{"role":"user","content":"[REDACTED]"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{parsed: long}
	before := x.sourceDemand("gpt-4o").EstimatedInputTokens
	x.parsed, x.sourceSummary = short, nil
	after := x.sourceDemand("gpt-4o").EstimatedInputTokens
	if after >= before {
		t.Fatalf("a redacted request estimates %d tokens against %d before redaction", after, before)
	}
	// Replacing the request without clearing the summary still follows it.
	x.parsed = long
	if again := x.sourceDemand("gpt-4o").EstimatedInputTokens; again != before {
		t.Fatalf("the request estimates %d tokens, want %d", again, before)
	}
}

// TestPreparedEstimateTakesTheLargerRequest holds the target of a rewritten
// request to the larger of the two requests it may be sent, with the less
// trustworthy provenance of the two counts.
func TestPreparedEstimateTakesTheLargerRequest(t *testing.T) {
	source, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// What a provider is sent can carry more than the caller wrote: here an
	// image, which no tokenizer counts.
	rewritten, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"gpt-4o","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"x"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{parsed: source}
	admitted, demand := x.preparedEstimate("gpt-4o", x.summarize(rewritten))
	// The source is eight tokens and ten for the reply; the rewritten request
	// adds an image.
	if want := (admittedEstimate{reserve: 8 + estimate.ImageTokens + 10, input: 8 + estimate.ImageTokens, reply: 10, provenance: estimate.ProvenanceCalibrated, family: estimate.FamilyOpenAIO200k}); admitted != want {
		t.Fatalf("admitted %+v, want %+v", admitted, want)
	}
	if demand.EstimatedInputTokens != 8+estimate.ImageTokens || demand.MaxOutputTokens == nil || *demand.MaxOutputTokens != 10 {
		t.Fatalf("routing weighs the request the provider is sent: %+v", demand)
	}
	// The other way round, the caller's request is the larger and the tokenizer
	// alone counted it, so its provenance stands.
	x = &execution{parsed: rewritten}
	admitted, _ = x.preparedEstimate("gpt-4o", x.summarize(source))
	if want := (admittedEstimate{reserve: 8 + estimate.ImageTokens + 10, input: 8 + estimate.ImageTokens, reply: 10, provenance: estimate.ProvenanceCalibrated, family: estimate.FamilyOpenAIO200k}); admitted != want {
		t.Fatalf("admitted %+v, want %+v", admitted, want)
	}
}

// TestBedrockToolCatalogueIsCountedOnce holds the tools a request is sent to a
// Bedrock provider with to one count. Bedrock Converse keeps its tool catalogue
// in toolConfig, which the walker reads as it reads the tools of every other
// dialect, so the request the provider is sent holds the catalogue as the
// caller's request does, and the estimate is the larger of the two: neither
// counts the catalogue twice.
func TestBedrockToolCatalogueIsCountedOnce(t *testing.T) {
	const (
		model = "us.anthropic.claude-sonnet-4-5"
		chat  = `{"model":"team-chat","max_tokens":10,"messages":[{"role":"user","content":"hello"}]`
		tools = `,"tools":[{"type":"function","function":{"name":"get_weather","description":"Weather in a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`
	)
	bedrock := runtime.Provider{ID: "bedrock", RevisionID: "bedrock-revision", Kind: "bedrock", AuthMode: "static", ProfileID: "bedrock-converse", ProfileRevision: connectors.ProfileRevision}
	compatible := runtime.Provider{ID: "compatible", RevisionID: "compatible-revision", Kind: "openai_compatible", ProfileID: "compatible-chat", ProfileRevision: "1"}
	prepare := func(provider runtime.Provider, body string) (preparedProvider, *openai.Request) {
		t.Helper()
		parsed, err := openai.Parse(openai.FamilyChat, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		x := &execution{parsed: parsed, request: request{release: &runtime.Release{Snapshot: &runtime.Snapshot{Providers: map[string]runtime.Provider{provider.ID: provider}}}}}
		prepared, err := x.preparedProvider(&provider, model)
		if err != nil {
			t.Fatal(err)
		}
		return prepared, parsed
	}

	plainBedrock, _ := prepare(bedrock, chat+`}`)
	withBedrock, source := prepare(bedrock, chat+tools)
	withCompatible, _ := prepare(compatible, chat+tools)
	if withBedrock.invocation.Wire != openai.FamilyBedrock || !strings.Contains(string(withBedrock.invocation.Prepared.Document().Bytes()), `"toolConfig"`) {
		t.Fatalf("the provider was not sent a Bedrock request with a tool catalogue: %s", withBedrock.invocation.Prepared.Document().Bytes())
	}

	// What the two requests the Bedrock provider may be sent hold, each counted
	// as the model counts it.
	counter := estimate.ForModel(model)
	sourceInput := estimate.Walk(source).Estimate(counter, nil).Input
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(withBedrock.invocation.Prepared.Document().Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	sentInput := estimate.Walk(openai.NewEnvelope(openai.FamilyBedrock, "team-chat", false, fields)).Estimate(counter, nil).Input
	catalogue, _ := counter.Count(estimate.SchemaText(fields["toolConfig"]))
	if catalogue < 10 {
		t.Fatalf("the catalogue is %d tokens: the case no longer has a catalogue worth counting", catalogue)
	}

	if want := max(sourceInput, sentInput); withBedrock.admitted.input != want {
		t.Errorf("input %d, want the larger of the caller's request (%d) and the Bedrock request (%d)", withBedrock.admitted.input, sourceInput, sentInput)
	}
	// The catalogue is in the count, and in it once.
	if withBedrock.admitted.input < plainBedrock.admitted.input+catalogue {
		t.Errorf("input %d does not hold the %d tokens of the catalogue beside the %d of the request without it", withBedrock.admitted.input, catalogue, plainBedrock.admitted.input)
	}
	if limit := sourceInput + catalogue; withBedrock.admitted.input >= limit {
		t.Errorf("input %d counts the catalogue twice: the caller's request holds it already, so the count stays under %d", withBedrock.admitted.input, limit)
	}
	// Whatever wire the tools travel in, a request is about as large.
	if gap := withBedrock.admitted.input - withCompatible.admitted.input; gap < -catalogue/2 || gap > catalogue/2 {
		t.Errorf("a Bedrock provider is admitted for %d input tokens and an OpenAI-wire one for %d", withBedrock.admitted.input, withCompatible.admitted.input)
	}
	if withBedrock.admitted.reserve != withBedrock.admitted.input+10 {
		t.Errorf("reserve %d, want the input and the ten tokens of reply", withBedrock.admitted.reserve)
	}
	// A model with a tokenizer reads a tool schema in a rendering of its own, so
	// its count is calibrated, as it is for every other tool catalogue; one
	// without says it is the heuristic.
	if withBedrock.admitted.provenance != estimate.ProvenanceHeuristic {
		t.Errorf("provenance %s, want heuristic for a Claude model", withBedrock.admitted.provenance)
	}
	var x execution
	x.parsed = source
	if admitted, _ := x.preparedEstimate("gpt-4o", x.summarize(openai.NewEnvelope(openai.FamilyBedrock, "team-chat", false, fields))); admitted.provenance != estimate.ProvenanceCalibrated {
		t.Errorf("provenance %s, want calibrated for a tokenizer family", admitted.provenance)
	}
}

// TestContextWindowIsWeighedByTheTargetsTokenizer sends a prompt the four
// character rule charges at a hundred tokens to a route whose first target is an
// OpenAI model, which counts it as three hundred and seven. The window of two
// hundred rules that target out before any upstream is called, and the Claude
// target that has no tokenizer, and the heuristic's hundred, serves it.
func TestContextWindowIsWeighedByTheTargetsTokenizer(t *testing.T) {
	h := newHarness(t, Config{})
	routeEstimateTargets(h,
		estimateTarget{"a", "gpt-4o", 1},
		estimateTarget{"b", "claude-sonnet-4-5", 1},
	)
	snapshot := h.rt.release.Snapshot
	for id, provider := range snapshot.Providers {
		provider.Models = map[string]json.RawMessage{provider.Capabilities[0].Model: json.RawMessage(`{"context_length":200}`)}
		snapshot.Providers[id] = provider
	}
	h.mock.set("a", completion("gpt-4o", answerText))
	h.mock.set("b", completion("claude-sonnet-4-5", answerText))

	// Three hundred tokens of o200k_base, and four hundred characters.
	prompt := strings.Repeat("日本語で", 100)
	resp, body := h.chat(fullKey, nil, `,"max_tokens":1`, `,"messages":[{"role":"user","content":"`+prompt+`"}]`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 1 {
		t.Fatalf("the OpenAI model was called %d times and Claude %d; the window rules the first out before dispatch", h.mock.count("a"), h.mock.count("b"))
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 || env.Attempts[0].ModelFamily != "anthropic" || env.Attempts[0].EstimatedInputTokens != 100 || env.Attempts[0].EstimateProvenance != "heuristic" {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

// TestAttemptsRecordTheSchemaOfAStructuredOutputInTheirEstimate sends a request
// whose reply must follow a schema, which a model reads as prompt text. The
// attempt records the tokens of the messages and of the schema, and says they are
// calibrated: an estimate that left the schema out would still have called the
// count exact.
func TestAttemptsRecordTheSchemaOfAStructuredOutputInTheirEstimate(t *testing.T) {
	h := newHarness(t, Config{})
	routeEstimateTargets(h, estimateTarget{"a", "gpt-4o", 1})
	declareParams(h, "gpt-4o", []string{"response_format"})
	h.mock.set("a", completion("gpt-4o", answerText))
	schema := `{"type":"object","properties":{"city":{"type":"string","description":"The city the weather is asked for"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["city","unit"],"additionalProperties":false}`

	resp, body := h.chat(fullKey, nil, `,"max_tokens":16`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	plain := h.sink.last(t).Attempts[0]
	if plain.EstimateProvenance != "tokenizer" {
		t.Fatalf("a request of plain text records %q, want tokenizer", plain.EstimateProvenance)
	}

	resp, body = h.chat(fullKey, nil, `,"max_tokens":16,"response_format":{"type":"json_schema","json_schema":{"name":"weather","schema":`+schema+`}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	attempt := h.sink.last(t).Attempts[0]
	tokens, _ := estimate.ForModel("gpt-4o").Count(estimate.SchemaText(json.RawMessage(schema)))
	if want := plain.EstimatedInputTokens + tokens; attempt.EstimatedInputTokens != want || tokens < 20 {
		t.Errorf("the attempt records %d input tokens, want the %d of the messages and the %d of the schema", attempt.EstimatedInputTokens, plain.EstimatedInputTokens, tokens)
	}
	if attempt.EstimateProvenance != "calibrated" {
		t.Errorf("the attempt records %q for a prompt with a schema in it, want calibrated", attempt.EstimateProvenance)
	}
}

// TestAttemptsRecordTheEstimateTheirRequestWasAdmittedUnder covers the requests
// the gateway reads only the size of, and the ones it reads nothing of.
func TestAttemptsRecordTheEstimateTheirRequestWasAdmittedUnder(t *testing.T) {
	attempt := runtime.Attempt{UpstreamModel: "gpt-4o"}

	// A body-sized request records the size-based input, whatever the model.
	sized := int64(42)
	var fact AttemptFact
	(&execution{sizedInput: &sized}).recordEstimate(&fact, attempt)
	if fact.EstimatedInputTokens != 42 || fact.EstimateProvenance != "heuristic" || fact.ModelFamily != "openai-o200k" {
		t.Errorf("a body-sized request recorded %+v", fact)
	}

	// A lifecycle call, a realtime session or a job poll carries no prompt: no
	// estimate is invented for it, and its family is recorded all the same.
	fact = AttemptFact{}
	(&execution{}).recordEstimate(&fact, attempt)
	if fact.EstimateProvenance != "" || fact.EstimatedInputTokens != 0 || fact.ModelFamily != "openai-o200k" {
		t.Errorf("a request without a prompt recorded %+v", fact)
	}

	// A walked request records the input alone, not the reply the reservation
	// holds as well.
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","max_tokens":1000,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{})
	x := &execution{parsed: parsed, request: request{release: h.rt.release}}
	fact = AttemptFact{}
	x.recordEstimate(&fact, attempt)
	if fact.EstimatedInputTokens != 8 || fact.EstimateProvenance != "tokenizer" {
		t.Errorf("a walked request recorded %+v, want its 8 input tokens", fact)
	}
}

// TestAttemptReservationCarriesTheProvidersDefaults holds the reservation of an
// attempt to the request its provider is sent: a provider that defaults the
// reply bound the caller left out reserves that bound, not the generic one. The
// provider is named by an attempt, as it is in every real release.
func TestAttemptReservationCarriesTheProvidersDefaults(t *testing.T) {
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	provider := runtime.Provider{ID: "bounded", ParameterDefaults: map[string]json.RawMessage{"max_tokens": json.RawMessage(`100`)}}
	other := runtime.Provider{ID: "unbounded"}
	x := &execution{
		parsed:  parsed,
		request: request{release: &runtime.Release{Snapshot: &runtime.Snapshot{Providers: map[string]runtime.Provider{provider.ID: provider, other.ID: other}}}},
		attempts: []runtime.Attempt{
			{ProviderID: provider.ID, UpstreamModel: "claude-sonnet-4-5"},
			{ProviderID: other.ID, UpstreamModel: "claude-sonnet-4-5"},
		},
	}
	// Five characters are two tokens to the heuristic.
	if got := x.attemptReservation(x.attempts[0], &provider); got != 2+100 {
		t.Errorf("the bounded provider's attempt reserves %d, want 102", got)
	}
	if got := x.attemptReservation(x.attempts[1], &other); got != 2+estimate.DefaultOutputTokens {
		t.Errorf("a provider without defaults reserves %d, want the default bound", got)
	}
	if got := requestEstimate(x); got != 2+estimate.DefaultOutputTokens {
		// The key holds the largest of the attempts it may make.
		t.Errorf("the key reserves %d, want the larger of its providers' %d", got, 2+estimate.DefaultOutputTokens)
	}
}

// TestRequestEstimatePricesEachAttemptOnce keeps the key's reservation linear
// in the targets of a route. A route may name sixty-four models of one
// provider, and pricing every attempt once for each attempt of its provider
// would make that four thousand estimates for one request. Allocations are the
// measure because they do not vary from run to run: four times the attempts
// may cost about four times as much, and the square would cost sixteen.
func TestRequestEstimatePricesEachAttemptOnce(t *testing.T) {
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	provider := runtime.Provider{ID: "shared", ParameterDefaults: map[string]json.RawMessage{"max_tokens": json.RawMessage(`100`)}}
	allocations := func(attempts int) float64 {
		x := &execution{
			parsed:  parsed,
			request: request{release: &runtime.Release{Snapshot: &runtime.Snapshot{Providers: map[string]runtime.Provider{provider.ID: provider}}}},
		}
		for i := range attempts {
			x.attempts = append(x.attempts, runtime.Attempt{ProviderID: provider.ID, UpstreamModel: fmt.Sprintf("model-%d", i)})
		}
		// The first run walks the prompt, and AllocsPerRun discards it.
		return testing.AllocsPerRun(10, func() { requestEstimate(x) })
	}
	few, many := allocations(8), allocations(32)
	if few == 0 {
		t.Fatal("an estimate allocates nothing; the case proves nothing")
	}
	if many > 6*few {
		t.Errorf("32 attempts allocate %.0f, 8 allocate %.0f: the estimate grows faster than the attempts do", many, few)
	}
}

// TestContentPolicyPricesTheRequestTheProviderIsSent covers a rule that makes a
// request longer. A route with a content policy never reaches a provider with
// the caller's request, so the attempt is admitted and recorded under the
// request the policy leaves, which is here twenty-five times the length of the
// one the caller wrote.
func TestContentPolicyPricesTheRequestTheProviderIsSent(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "expand", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: strings.Repeat("z", 400)})
	h.mock.set("a", completion(modelA, answerText))
	h.mock.set("b", completion(modelA, answerText))
	resp, body := h.chat(fullKey, nil, `,"max_tokens":1`, `,"messages":[{"role":"user","content":"the s3cr3t plan"}]`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 {
		t.Fatalf("%d attempts", len(env.Attempts))
	}
	// The caller's fifteen characters are four tokens to the four-character
	// rule, which counts the model. The provider is sent four hundred and nine
	// characters, a hundred and three tokens.
	if got := env.Attempts[0]; got.EstimatedInputTokens != 103 || got.EstimateProvenance != "heuristic" {
		t.Fatalf("the attempt recorded %d tokens from %s, want the 103 of the request it was sent", got.EstimatedInputTokens, got.EstimateProvenance)
	}
}

// TestContextWindowKeepsATargetWhoseOwnCountFits is the other side of the
// window test above: the four-character rule charges this prompt more than the
// target's window holds, and the model's own tokenizer, which is what serves
// it, counts less. Planning weighs the window twice when a route has a target
// that is sent a request of its own, once as the caller's request and once as
// the request the provider is sent, and both are the model's count.
func TestContextWindowKeepsATargetWhoseOwnCountFits(t *testing.T) {
	for _, withProfileTarget := range []bool{false, true} {
		name := "a route of plain targets"
		if withProfileTarget {
			name = "a route with a target that has a profile"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{})
			routeEstimateTargets(h, estimateTarget{"a", "gpt-4o", 1}, estimateTarget{"b", "gpt-4o-mini", 1})
			snapshot := h.rt.release.Snapshot
			for id, provider := range snapshot.Providers {
				switch provider.Name {
				case "a":
					provider.Models = map[string]json.RawMessage{"gpt-4o": json.RawMessage(`{"context_length":200}`)}
				case "b":
					if withProfileTarget {
						provider.ProfileID, provider.ProfileRevision = "compatible-chat", "1"
						provider.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat"}}
					}
				}
				snapshot.Providers[id] = provider
			}
			h.mock.set("a", completion("gpt-4o", answerText))
			h.mock.set("b", completion("gpt-4o-mini", answerText))

			// A hundred and fifty-one tokens of o200k_base, which are a hundred and
			// fifty-eight with the message framed, and nine hundred characters.
			prompt := strings.Repeat("hello ", 150)
			resp, body := h.chat(fullKey, nil, `,"max_tokens":1`, `,"messages":[{"role":"user","content":"`+prompt+`"}]`)
			if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
				t.Fatalf("status %d after %d calls to the first target and %d to the second, body %v; the window holds the model's own 159 tokens and not the rule's 226",
					resp.StatusCode, h.mock.count("a"), h.mock.count("b"), body)
			}
			env := h.sink.last(t)
			if got := env.Attempts[0]; got.EstimatedInputTokens != 158 || got.EstimateProvenance != "tokenizer" || got.ModelFamily != "openai-o200k" {
				t.Fatalf("the attempt recorded %d tokens from %s for %s", got.EstimatedInputTokens, got.EstimateProvenance, got.ModelFamily)
			}
		})
	}
}

// TestRequestsReadBySizeRecordTheirSizeAsTheEstimate drives each handler that
// reserves a request by its size, and holds what its attempt records: four
// bytes to a token over the body, as a heuristic, for the model's family.
func TestRequestsReadBySizeRecordTheirSizeAsTheEstimate(t *testing.T) {
	t.Run("bedrock invoke", func(t *testing.T) {
		h := newHarness(t, Config{})
		for id, p := range h.rt.release.Snapshot.Providers {
			p.Kind = "bedrock"
			p.Capabilities = append(p.Capabilities, runtime.Capability{Model: p.Capabilities[0].Model, Operation: "bedrock_invoke", Surface: "bedrock", Mode: "unary"})
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
			p.Capabilities = append(p.Capabilities, runtime.Capability{Model: p.Capabilities[0].Model, Operation: "generation", Surface: "gemini", Mode: "unary"})
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

// TestMultipartMediaRecordsNoEstimate pins what the documentation says of the
// uploads the gateway reserves at a flat charge: there is no size to read an
// input estimate from, so no estimate is invented, and the attempt still
// records the family of its model.
func TestMultipartMediaRecordsNoEstimate(t *testing.T) {
	h := newMediaHarness(t)
	h.mock.set("a", status(200, `{"data":[{"url":"https://example.com/image.png"}]}`))
	h.mock.set("b", status(200, `{"data":[{"url":"https://example.com/image.png"}]}`))
	body, contentType := mediaMultipart(t, map[string]string{"model": routeSlug, "prompt": "photo"}, []byte("png"))
	resp := h.do(t.Context(), http.MethodPost, "/v1/images/edits", fullKey, body, map[string]string{"Content-Type": contentType})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	env := h.sink.last(t)
	if len(env.Attempts) != 1 {
		t.Fatalf("%d attempts, status %d", len(env.Attempts), resp.StatusCode)
	}
	if got := env.Attempts[0]; got.EstimateProvenance != "" || got.EstimatedInputTokens != 0 || got.ModelFamily != "other" {
		t.Fatalf("an upload recorded %d tokens from %q for %q, want no estimate and its family", got.EstimatedInputTokens, got.EstimateProvenance, got.ModelFamily)
	}
}

// TestTranslatedPreparedTargetsCountTheSameTextOnce is the case a profile, a
// strict contract or a content policy makes of a request in another dialect: the
// chat request the provider is sent holds the text the Anthropic or Responses
// request did, with the system prompt first where those dialects walk it last.
// The text is the same, so each family is counted once for the request and its
// translation.
func TestTranslatedPreparedTargetsCountTheSameTextOnce(t *testing.T) {
	chat, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hello there"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	anthropic := openai.NewEnvelope(openai.FamilyAnthropic, routeSlug, false, map[string]json.RawMessage{
		"system":   json.RawMessage(`"Be brief."`),
		"messages": json.RawMessage(`[{"role":"user","content":"hello there"}]`),
	})
	responses, err := openai.Parse(openai.FamilyResponses, []byte(`{"model":"team-chat","input":"hello there","instructions":"Be brief."}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]*openai.Request{"anthropic": anthropic, "responses": responses} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			counted := map[estimate.Family]int{}
			x := &execution{parsed: source, request: request{counted: func(f estimate.Family) { mu.Lock(); counted[f]++; mu.Unlock() }}}
			for range 2 {
				admitted, _ := x.preparedEstimate("gpt-4o", x.summarize(chat))
				// "Be brief." is three tokens and "hello there" two; "system" and
				// "user" are one each, two messages are six and the reply three.
				if admitted.input != 3+2+1+1+6+3 || admitted.provenance != estimate.ProvenanceTokenizer || admitted.family != estimate.FamilyOpenAIO200k {
					t.Fatalf("admitted %+v", admitted)
				}
				x.sourceDemand("gpt-4o")
			}
			if len(counted) != 1 || counted[estimate.FamilyOpenAIO200k] != 1 {
				t.Fatalf("counted %v, want the one family once for the request and its translation", counted)
			}
		})
	}
}
