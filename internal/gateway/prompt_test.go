package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/prompts"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func publishedPromptHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := newHarness(t, Config{})
	projectID := uuid.NewString()
	publicationID := uuid.NewString()
	revisionID := uuid.NewString()
	route := h.rt.release.Snapshot.Routes[routeSlug]
	route.ProjectID = &projectID
	h.rt.release.Snapshot.Routes[routeSlug] = route
	key := h.rt.keys[fullKey]
	key.ProjectID = &projectID
	h.rt.keys[fullKey] = key
	draft := agentcontract.Draft{ProjectID: projectID, Name: "test", Prompt: &prompts.Definition{
		Template: "Hello {{name}}", Variables: []string{"name"},
	}}
	h.rt.release.Snapshot.AgentPublications = []agentcontract.Revision{{
		ID: revisionID, PublicationID: publicationID, ProjectID: projectID, Kind: "prompt",
		Labels: []string{"production"}, Definition: draft, Digest: agentcontract.DraftDigest(draft),
	}}
	return h, publicationID, revisionID
}

func promptTestProviders(providers map[string]routing.Provider, kind, profileID, profileRevision string) map[string]routing.Provider {
	return maps.Collect(func(yield func(string, routing.Provider) bool) {
		for id, provider := range providers {
			model := provider.Capabilities[0].Model
			if !yield(id, routing.Provider{
				ID: provider.ID, Name: provider.Name, Kind: kind, Enabled: provider.Enabled,
				ActiveCredential: provider.ActiveCredential, RevisionID: provider.RevisionID,
				Endpoint: provider.Endpoint, AuthMode: provider.AuthMode, Slots: provider.Slots,
				ProfileID: profileID, ProfileRevision: profileRevision,
				Capabilities: []routing.Capability{
					{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"},
					{Model: model, Operation: "generation", Surface: "openai", Mode: "streaming"},
					{Model: model, Operation: "generation", Surface: "responses", Mode: "unary"},
					{Model: model, Operation: "generation", Surface: "responses", Mode: "streaming"},
					{Model: model, Operation: "generation", Surface: "anthropic", Mode: "unary"},
					{Model: model, Operation: "generation", Surface: "anthropic", Mode: "streaming"},
					{Model: model, Operation: "generation", Surface: "gemini", Mode: "unary"},
					{Model: model, Operation: "generation", Surface: "gemini", Mode: "streaming"},
				},
			}) {
				return
			}
		}
	})
}

func promptTestRoute(route routing.Route, fidelity routing.RouteFidelity, behavior routing.Behavior) routing.Route {
	return routing.Route{
		ID: route.ID, Slug: route.Slug, Operations: route.Operations, OverallTimeout: route.OverallTimeout,
		MaxAttempts: route.MaxAttempts, Targets: route.Targets, RoutingID: route.RoutingID,
		RevisionID: route.RevisionID, Revision: route.Revision, PublishedAt: route.PublishedAt,
		Policy: route.Policy, ProjectID: route.ProjectID, ContentPolicy: route.ContentPolicy,
		Fidelity: fidelity, Behavior: behavior,
	}
}

func promptRefusalObserver() (http.HandlerFunc, <-chan struct{}) {
	dispatched := make(chan struct{}, 1)
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case dispatched <- struct{}{}:
		default:
		}
		completion(modelA, "unexpected")(w, r)
	}, dispatched
}

func assertPromptNotDispatched(t *testing.T, dispatched <-chan struct{}) {
	t.Helper()
	select {
	case <-dispatched:
		t.Fatal("refused prompt reached the upstream")
	default:
	}
}

func TestChatPublishedPromptRendersBeforeDispatch(t *testing.T) {
	h, publicationID, revisionID := publishedPromptHarness(t)
	stored := make(chan []byte, 1)
	h.gateway.Sink = &PersistingSink{Persist: func(_ context.Context, _ *usage.Event, payload []byte) error {
		stored <- payload
		return nil
	}}
	upstreamBody := make(chan []byte, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		upstreamBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	})
	request := fmt.Sprintf(`{"model":%q,"olp\u005fprompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, publicationID)
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Prompt json.RawMessage `json:"olp_prompt"`
	}
	if err := json.Unmarshal(<-upstreamBody, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Role != "user" || sent.Messages[0].Content != "Hello Ada" {
		t.Fatalf("upstream messages = %#v", sent.Messages)
	}
	if len(sent.Prompt) != 0 {
		t.Fatalf("prompt reference leaked upstream: %s", sent.Prompt)
	}
	payload := <-stored
	if strings.Contains(string(payload), "Ada") || strings.Contains(string(payload), "Hello") {
		t.Fatalf("stored metadata contains prompt content: %s", payload)
	}
	decoded, err := usage.Decode(payload)
	if err != nil {
		t.Fatalf("decode stored metadata: %v", err)
	}
	roundtrip, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Attempts []struct {
			Routing struct {
				Prompt struct {
					ID         string `json:"id"`
					RevisionID string `json:"revision_id"`
				} `json:"prompt"`
			} `json:"routing"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(roundtrip, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Attempts) != 1 || metadata.Attempts[0].Routing.Prompt.ID != publicationID || metadata.Attempts[0].Routing.Prompt.RevisionID != revisionID {
		t.Fatalf("attempt prompt attribution = %s", roundtrip)
	}
}

func TestPublishedPromptRendersStructuredMessagesAndDefaults(t *testing.T) {
	h, publicationID, _ := publishedPromptHarness(t)
	definition := []byte(`{"messages":[{"role":"system","content":"You help {{name}}"},{"role":"user","content":"Hello {{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false},"default_parameters":{"temperature":0.25,"max_output_tokens":32}}`)
	var prompt prompts.Definition
	if err := json.Unmarshal(definition, &prompt); err != nil {
		t.Fatal(err)
	}
	h.rt.release.Snapshot.AgentPublications[0].Definition.Prompt = &prompt
	upstream := make(chan []byte, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err == nil {
			upstream <- body
		}
		completion(modelA, answerText)(w, r)
	})
	request := fmt.Sprintf(`{"model":%q,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, publicationID)
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Temperature float64 `json:"temperature"`
		MaxTokens   int     `json:"max_tokens"`
	}
	if err := json.Unmarshal(<-upstream, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 2 || sent.Messages[0].Role != "system" || sent.Messages[0].Content != "You help Ada" || sent.Messages[1].Role != "user" || sent.Messages[1].Content != "Hello Ada" || sent.Temperature != 0.25 || sent.MaxTokens != 32 {
		t.Fatalf("upstream request = %#v", sent)
	}
	override := fmt.Sprintf(`{"model":%q,"temperature":0.75,"max_completion_tokens":7,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, publicationID)
	overrideResponse := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(override), nil)
	defer overrideResponse.Body.Close()
	if overrideResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(overrideResponse.Body)
		t.Fatalf("override status = %d, body = %s", overrideResponse.StatusCode, body)
	}
	var overridden struct {
		Temperature         float64 `json:"temperature"`
		MaxCompletionTokens int     `json:"max_completion_tokens"`
		MaxTokens           *int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(<-upstream, &overridden); err != nil {
		t.Fatal(err)
	}
	if overridden.Temperature != 0.75 || overridden.MaxCompletionTokens != 7 || overridden.MaxTokens != nil {
		t.Fatalf("request parameters were replaced by defaults: %#v", overridden)
	}
}

func TestChatPublishedPromptRefusesInvalidRequestsBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, extra  string
		strict, crossProject bool
	}{
		{name: "missing variable", prompt: `"label":"production","variables":{}`},
		{name: "unknown variable", prompt: `"label":"production","variables":{"name":"Ada","other":"value"}`},
		{name: "unknown label", prompt: `"label":"unknown","variables":{"name":"Ada"}`},
		{name: "unknown revision", prompt: fmt.Sprintf(`"revision_id":%q,"variables":{"name":"Ada"}`, uuid.NewString())},
		{name: "non-string variable", prompt: `"label":"production","variables":{"name":null}`},
		{name: "both selectors", prompt: `"label":"production","revision_id":"other","variables":{"name":"Ada"}`},
		{name: "null selector", prompt: `"label":null,"revision_id":"other","variables":{"name":"Ada"}`},
		{name: "messages", prompt: `"label":"production","variables":{"name":"Ada"}`, extra: `,"messages":[{"role":"user","content":"other"}]`},
		{name: "strict", prompt: `"label":"production","variables":{"name":"Ada"}`, strict: true},
		{name: "cross project", prompt: `"label":"production","variables":{"name":"Ada"}`, crossProject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, publicationID, _ := publishedPromptHarness(t)
			if tc.strict {
				route := h.rt.release.Snapshot.Routes[routeSlug]
				route.Fidelity.Mode = routing.FidelityStrict
				h.rt.release.Snapshot.Routes[routeSlug] = route
			}
			if tc.crossProject {
				h.rt.release.Snapshot.AgentPublications[0].ProjectID = uuid.NewString()
			}
			request := fmt.Sprintf(`{"model":%q,"olp_prompt":{"id":%q,%s}%s}`, routeSlug, publicationID, tc.prompt, tc.extra)
			response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			if len(h.sink.last(t).Attempts) != 0 {
				t.Fatal("invalid request created an attempt")
			}
		})
	}
}

func TestChatPublishedPromptPassesRenderedInputToGuardrails(t *testing.T) {
	h, publicationID, _ := publishedPromptHarness(t)
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	projectID := *route.ProjectID
	snapshot.Guardrails = &guardrails.Registry{
		Definitions: map[string]guardrails.Revision{"revision": {ID: "revision", GuardrailID: "guard", ProjectID: projectID, Name: "Guard", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["forbidden"]}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}},
		Policies:    map[string]guardrails.PolicyRevision{"policy-revision": {ID: "policy-revision", PolicyID: "policy", ProjectID: projectID, Entries: []guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrails.Input, Action: guardrails.Block}}}},
		Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: route.ID, PolicyRevisionID: "policy-revision"}},
	}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	request := fmt.Sprintf(`{"model":%q,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"forbidden"}}}`, routeSlug, publicationID)
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode < 400 || !strings.Contains(string(body), "guardrail_blocked") {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if len(h.sink.last(t).Attempts) != 0 {
		t.Fatal("guardrail-blocked request created an attempt")
	}
}

func TestChatPublishedPromptPinsLabelAndAcceptsExactRevision(t *testing.T) {
	h, publicationID, firstRevision := publishedPromptHarness(t)
	snapshot := h.rt.release.Snapshot
	first := snapshot.AgentPublications[0]
	secondDraft := agentcontract.Draft{ProjectID: first.ProjectID, Name: "test", Prompt: &prompts.Definition{Template: "Welcome {{name}}", Variables: []string{"name"}}}
	secondRevision := uuid.NewString()
	snapshot.AgentPublications = append(snapshot.AgentPublications, agentcontract.Revision{
		ID: secondRevision, PublicationID: publicationID, ProjectID: first.ProjectID, Kind: "prompt",
		Labels: []string{"staging"}, Definition: secondDraft, Digest: agentcontract.DraftDigest(secondDraft),
	})
	started := make(chan struct{})
	finish := make(chan struct{})
	var calls atomic.Int32
	bodies := make(chan string, 3)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-finish:
			case <-r.Context().Done():
				return
			}
		}
		completion(modelA, "ok")(w, r)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := fmt.Sprintf(`{"model":%q,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, publicationID)
	firstResponse := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(request))
		if err != nil {
			firstResponse <- err
			return
		}
		req.Header.Set("Authorization", "Bearer "+fullKey)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				err = fmt.Errorf("first status = %d", res.StatusCode)
			}
		}
		firstResponse <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first request did not reach upstream")
	}
	firstBody := <-bodies
	updated := snapshot.Clone()
	updated.AgentPublications[0].Labels = nil
	updated.AgentPublications[1].Labels = []string{"production"}
	h.rt.mu.Lock()
	nextRelease := *h.rt.release
	nextRelease.Snapshot = updated
	h.rt.release = &nextRelease
	h.rt.mu.Unlock()
	response := h.do(ctx, http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", response.StatusCode, body)
	}
	if sent := <-bodies; !strings.Contains(sent, "Welcome Ada") {
		t.Fatalf("second request did not use moved label: %s", sent)
	}
	if fact := h.sink.nth(t, 1).Attempts[0].Prompt; fact == nil || fact.RevisionID != secondRevision {
		t.Fatalf("second request attribution = %#v", fact)
	}
	close(finish)
	if err := <-firstResponse; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstBody, "Hello Ada") {
		t.Fatalf("first request changed revisions: %s", firstBody)
	}
	if fact := h.sink.nth(t, 2).Attempts[0].Prompt; fact == nil || fact.RevisionID != firstRevision {
		t.Fatalf("first request attribution = %#v", fact)
	}
	exact := fmt.Sprintf(`{"model":%q,"olp_prompt":{"id":%q,"revision_id":%q,"variables":{"name":"Ada"}}}`, routeSlug, publicationID, firstRevision)
	exactResponse := h.do(ctx, http.MethodPost, "/v1/chat/completions", fullKey, []byte(exact), nil)
	exactBody, _ := io.ReadAll(exactResponse.Body)
	exactResponse.Body.Close()
	if exactResponse.StatusCode != http.StatusOK || !strings.Contains(<-bodies, "Hello Ada") {
		t.Fatalf("exact revision status = %d, body = %s", exactResponse.StatusCode, exactBody)
	}
	if fact := h.sink.nth(t, 3).Attempts[0].Prompt; fact == nil || fact.RevisionID != firstRevision {
		t.Fatalf("exact revision attribution = %#v", fact)
	}
}

func TestChatPublishedPromptAttributesEachProviderAttempt(t *testing.T) {
	h, publicationID, revisionID := publishedPromptHarness(t)
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"unavailable","type":"server_error"}}`))
	request := fmt.Sprintf(`{"model":%q,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, publicationID)
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	attempts := h.sink.last(t).Attempts
	if len(attempts) < 2 {
		t.Fatalf("attempt count = %d", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.Prompt == nil || attempt.Prompt.ID != publicationID || attempt.Prompt.RevisionID != revisionID {
			t.Fatalf("attempt prompt = %#v", attempt.Prompt)
		}
	}
}

func TestChatPublishedPromptDoesNotPersistInResponseCache(t *testing.T) {
	h, publicationID, _ := publishedPromptHarness(t)
	publications := h.rt.release.Snapshot.AgentPublications
	enableTestCache(t, h)
	h.rt.release.Snapshot.AgentPublications = publications
	var sequence atomic.Int32
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		input, _ := io.ReadAll(r.Body)
		content := fmt.Sprintf("response %d", sequence.Add(1))
		if strings.Contains(string(input), "Hello Ada") {
			content = fmt.Sprintf("Hello Ada %d", sequence.Load())
		}
		completion(modelA, content)(w, r)
	})
	request := func(input string) string {
		t.Helper()
		response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(input), nil)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.StatusCode, body)
		}
		return string(body)
	}
	ordinary := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"public"}]}`, routeSlug)
	firstOrdinary, secondOrdinary := request(ordinary), request(ordinary)
	if firstOrdinary != secondOrdinary {
		t.Fatalf("ordinary request was not replayed from the enabled cache: %s, %s", firstOrdinary, secondOrdinary)
	}
	prompt := fmt.Sprintf(`{"model":%q,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, publicationID)
	firstPrompt, secondPrompt := request(prompt), request(prompt)
	if !strings.Contains(firstPrompt, "Hello Ada") || !strings.Contains(secondPrompt, "Hello Ada") || firstPrompt == secondPrompt {
		t.Fatalf("prompt response was replayed from the cache: %s, %s", firstPrompt, secondPrompt)
	}
}

func TestChatPublishedPromptRespectsRenderedInputLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		routeLimit bool
		length     int
		wantStatus int
	}{
		{name: "route limit exceeded", routeLimit: true, length: 700, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "installation limit exceeded", length: 700, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "within route limit", routeLimit: true, length: 120, wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, publicationID, _ := publishedPromptHarness(t)
			h.gateway.cfg.MaxBodyBytes = 512
			if tc.routeLimit {
				h.gateway.cfg.MaxBodyBytes = 1024
				route := h.rt.release.Snapshot.Routes[routeSlug]
				limit := int64(512)
				route.MaxBodyBytes = &limit
				h.rt.release.Snapshot.Routes[routeSlug] = route
			}
			draft := h.rt.release.Snapshot.AgentPublications[0].Definition
			draft.Prompt.Template = strings.Repeat("x", tc.length) + " {{name}}"
			h.rt.release.Snapshot.AgentPublications[0].Definition = draft
			h.rt.release.Snapshot.AgentPublications[0].Digest = agentcontract.DraftDigest(draft)
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			request := fmt.Sprintf(`{"model":%q,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, publicationID)
			response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			if tc.wantStatus != http.StatusOK && len(h.sink.last(t).Attempts) != 0 {
				t.Fatal("oversized prompt created an attempt")
			}
		})
	}
}

func TestChatPublishedPromptLeavesOrdinaryEscapedContentUnchanged(t *testing.T) {
	h := newHarness(t, Config{})
	upstreamBody := make(chan []byte, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody <- body
		completion(modelA, "ok")(w, r)
	})
	request := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"Text with \"olp_prompt\" and \u0041"}],"extra":"\u0042"}`
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	var sent struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Extra string `json:"extra"`
	}
	if err := json.Unmarshal(<-upstreamBody, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Content != `Text with "olp_prompt" and A` || sent.Extra != "B" {
		t.Fatalf("upstream request = %#v", sent)
	}
}

func TestResponsesPublishedPromptRendersBeforeDispatch(t *testing.T) {
	h, publicationID, revisionID := publishedPromptHarness(t)
	upstreamBody := make(chan []byte, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody <- body
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_1","object":"response","model":"model-a","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	})
	request := fmt.Sprintf(`{"model":%q,"prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, "olpp_"+publicationID)
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(request), nil)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	var sent struct {
		Input []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"input"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(<-upstreamBody, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Input) != 1 || sent.Input[0].Role != "user" || sent.Input[0].Content != "Hello Ada" || len(sent.Prompt) != 0 {
		t.Fatalf("upstream request = %#v", sent)
	}
	if fact := h.sink.last(t).Attempts[0].Prompt; fact == nil || fact.ID != publicationID || fact.RevisionID != revisionID {
		t.Fatalf("attempt prompt = %#v", fact)
	}
}

func TestNativePublishedPromptRendersBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{name: "anthropic", path: "/anthropic/v1/messages", body: `{"model":"` + routeSlug + `","max_tokens":20,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`},
		{name: "gemini", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", body: `{"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, publicationID, revisionID := publishedPromptHarness(t)
			h.rt.release.Snapshot.Providers = promptTestProviders(h.rt.release.Snapshot.Providers, "openai", "", "")
			upstreamBody := make(chan []byte, 1)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstreamBody <- body
				completion(modelA, "ok")(w, r)
			})
			request := fmt.Sprintf(tc.body, publicationID)
			response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(request), nil)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			var sent struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
				Prompt json.RawMessage `json:"olp_prompt"`
			}
			if err := json.Unmarshal(<-upstreamBody, &sent); err != nil {
				t.Fatal(err)
			}
			if len(sent.Messages) != 1 || sent.Messages[0].Role != "user" || sent.Messages[0].Content != "Hello Ada" || len(sent.Prompt) != 0 {
				t.Fatalf("upstream request = %#v", sent)
			}
			if fact := h.sink.last(t).Attempts[0].Prompt; fact == nil || fact.ID != publicationID || fact.RevisionID != revisionID {
				t.Fatalf("attempt prompt = %#v", fact)
			}
		})
	}
}

func TestPublishedPromptUsesNativeProviderMessages(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		path    string
		body    string
		reply   string
	}{
		{name: "anthropic", profile: "anthropic-messages", path: "/anthropic/v1/messages", body: `{"model":"` + routeSlug + `","max_tokens":20,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, reply: `{"id":"msg_1","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`},
		{name: "gemini", profile: "gemini-generation", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", body: `{"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, reply: `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, publicationID, revisionID := publishedPromptHarness(t)
			h.rt.release.Snapshot.Providers = promptTestProviders(h.rt.release.Snapshot.Providers, tc.name, tc.profile, connectors.ProfileRevision)
			upstreamBody := make(chan []byte, 1)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstreamBody <- body
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.reply)
			})
			request := fmt.Sprintf(tc.body, publicationID)
			response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(request), nil)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			var sent struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
				Contents []struct {
					Role  string `json:"role"`
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"contents"`
				Prompt json.RawMessage `json:"olp_prompt"`
			}
			if err := json.Unmarshal(<-upstreamBody, &sent); err != nil {
				t.Fatal(err)
			}
			if len(sent.Prompt) != 0 {
				t.Fatalf("prompt reference leaked upstream: %s", sent.Prompt)
			}
			if tc.name == "anthropic" && (len(sent.Messages) != 1 || sent.Messages[0].Role != "user" || sent.Messages[0].Content != "Hello Ada") {
				t.Fatalf("anthropic messages = %#v", sent.Messages)
			}
			if tc.name == "gemini" && (len(sent.Contents) != 1 || sent.Contents[0].Role != "user" || len(sent.Contents[0].Parts) != 1 || sent.Contents[0].Parts[0].Text != "Hello Ada") {
				t.Fatalf("gemini contents = %#v", sent.Contents)
			}
			if fact := h.sink.last(t).Attempts[0].Prompt; fact == nil || fact.ID != publicationID || fact.RevisionID != revisionID {
				t.Fatalf("attempt prompt = %#v", fact)
			}
		})
	}
}

func TestResponsesNativePromptPassesThrough(t *testing.T) {
	h, _, _ := publishedPromptHarness(t)
	upstreamBody := make(chan []byte, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody <- body
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_1","object":"response","model":"model-a","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	})
	request := `{"model":"` + routeSlug + `","input":"hello","prompt":{"id":"provider-prompt","version":"7","variables":{"name":"Ada"}}}`
	response := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(request), nil)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	var sent struct {
		Prompt struct {
			ID        string            `json:"id"`
			Version   string            `json:"version"`
			Variables map[string]string `json:"variables"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(<-upstreamBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Prompt.ID != "provider-prompt" || sent.Prompt.Version != "7" || sent.Prompt.Variables["name"] != "Ada" {
		t.Fatalf("native prompt = %#v", sent.Prompt)
	}
	if fact := h.sink.last(t).Attempts[0].Prompt; fact != nil {
		t.Fatalf("native prompt was attributed to the registry: %#v", fact)
	}
}

func TestPublishedPromptRefusesConflictsAcrossSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		body  string
		extra string
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"` + routeSlug + `","prompt":{"id":"olpp_%s","label":"production","variables":{"name":"Ada"}}`, extra: `,"input":"other"}`},
		{name: "responses instructions", path: "/v1/responses", body: `{"model":"` + routeSlug + `","prompt":{"id":"olpp_%s","label":"production","variables":{"name":"Ada"}}`, extra: `,"instructions":"other"}`},
		{name: "anthropic", path: "/anthropic/v1/messages", body: `{"model":"` + routeSlug + `","max_tokens":20,"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}`, extra: `,"messages":[{"role":"user","content":"other"}]}`},
		{name: "anthropic system", path: "/anthropic/v1/messages", body: `{"model":"` + routeSlug + `","max_tokens":20,"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}`, extra: `,"system":"other"}`},
		{name: "gemini", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", body: `{"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}`, extra: `,"contents":[{"role":"user","parts":[{"text":"other"}]}]}`},
		{name: "gemini system", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", body: `{"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}`, extra: `,"systemInstruction":{"parts":[{"text":"other"}]}}`},
		{name: "gemini system snake case", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", body: `{"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}`, extra: `,"system_instruction":{"parts":[{"text":"other"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, publicationID, _ := publishedPromptHarness(t)
			h.rt.release.Snapshot.Providers = promptTestProviders(h.rt.release.Snapshot.Providers, "openai", "", "")
			upstream, dispatched := promptRefusalObserver()
			h.mock.set("a", upstream)
			h.mock.set("b", upstream)
			request := fmt.Sprintf(tc.body+tc.extra, publicationID)
			response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(request), nil)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "invalid_prompt") {
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			if len(h.sink.last(t).Attempts) != 0 {
				t.Fatal("conflicting input dispatched an attempt")
			}
			assertPromptNotDispatched(t, dispatched)
		})
	}
}

func TestPublishedPromptEnforcesVariablesAndRoutePolicyAcrossSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"` + routeSlug + `","prompt":{"id":"olpp_%s","label":"production","variables":%s}}`},
		{name: "anthropic", path: "/anthropic/v1/messages", body: `{"model":"` + routeSlug + `","max_tokens":20,"olp_prompt":{"id":"%s","label":"production","variables":%s}}`},
		{name: "gemini", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", body: `{"olp_prompt":{"id":"%s","label":"production","variables":%s}}`},
	} {
		for _, scenario := range []struct {
			name      string
			variables string
			strict    bool
		}{
			{name: "missing variable", variables: `{}`},
			{name: "unknown variable", variables: `{"name":"Ada","unexpected":"value"}`},
			{name: "strict route", variables: `{"name":"Ada"}`, strict: true},
		} {
			t.Run(tc.name+"/"+scenario.name, func(t *testing.T) {
				h, publicationID, _ := publishedPromptHarness(t)
				h.rt.release.Snapshot.Providers = promptTestProviders(h.rt.release.Snapshot.Providers, "openai", "", "")
				upstream, dispatched := promptRefusalObserver()
				h.mock.set("a", upstream)
				h.mock.set("b", upstream)
				if scenario.strict {
					route := h.rt.release.Snapshot.Routes[routeSlug]
					h.rt.release.Snapshot.Routes = map[string]routing.Route{
						routeSlug: promptTestRoute(route, routing.RouteFidelity{Mode: routing.FidelityStrict}, route.Behavior),
					}
				}
				request := fmt.Sprintf(tc.body, publicationID, scenario.variables)
				response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(request), nil)
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "invalid_prompt") {
					t.Fatalf("status = %d, body = %s", response.StatusCode, body)
				}
				if len(h.sink.last(t).Attempts) != 0 {
					t.Fatal("invalid prompt dispatched an attempt")
				}
				assertPromptNotDispatched(t, dispatched)
			})
		}
	}
}

func TestPublishedPromptAppliesInputGuardrailsAcrossSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"` + routeSlug + `","prompt":{"id":"olpp_%s","label":"production","variables":{"name":"forbidden"}}}`},
		{name: "anthropic", path: "/anthropic/v1/messages", body: `{"model":"` + routeSlug + `","max_tokens":20,"olp_prompt":{"id":"%s","label":"production","variables":{"name":"forbidden"}}}`},
		{name: "gemini", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", body: `{"olp_prompt":{"id":"%s","label":"production","variables":{"name":"forbidden"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, publicationID, _ := publishedPromptHarness(t)
			upstream, dispatched := promptRefusalObserver()
			h.mock.set("a", upstream)
			h.mock.set("b", upstream)
			snapshot := h.rt.release.Snapshot
			route := snapshot.Routes[routeSlug]
			projectID := *route.ProjectID
			snapshot.Guardrails = &guardrails.Registry{
				Definitions: map[string]guardrails.Revision{"revision": {ID: "revision", GuardrailID: "guard", ProjectID: projectID, Name: "Guard", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["forbidden"]}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}},
				Policies:    map[string]guardrails.PolicyRevision{"policy-revision": {ID: "policy-revision", PolicyID: "policy", ProjectID: projectID, Entries: []guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrails.Input, Action: guardrails.Block}}}},
				Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: route.ID, PolicyRevisionID: "policy-revision"}},
			}
			if err := snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			snapshot.Providers = promptTestProviders(snapshot.Providers, "openai", "", "")
			request := fmt.Sprintf(tc.body, publicationID)
			response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(request), nil)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode < 400 || !strings.Contains(string(body), "guardrail_blocked") {
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			if len(h.sink.last(t).Attempts) != 0 {
				t.Fatal("guardrail-blocked prompt dispatched an attempt")
			}
			assertPromptNotDispatched(t, dispatched)
		})
	}
}

func TestPublishedPromptRespectsRenderedInputLimitAcrossSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"` + routeSlug + `","prompt":{"id":"olpp_%s","label":"production","variables":{"name":"Ada"}}}`},
		{name: "anthropic", path: "/anthropic/v1/messages", body: `{"model":"` + routeSlug + `","max_tokens":20,"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}}`},
		{name: "gemini", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", body: `{"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, publicationID, _ := publishedPromptHarness(t)
			upstream, dispatched := promptRefusalObserver()
			h.mock.set("a", upstream)
			h.mock.set("b", upstream)
			publication := h.rt.release.Snapshot.AgentPublications[0]
			draft := agentcontract.Draft{ProjectID: publication.ProjectID, Name: publication.Definition.Name,
				Prompt: &prompts.Definition{Template: strings.Repeat("long text ", 200) + "{{name}}", Variables: []string{"name"}}}
			h.rt.release.Snapshot.AgentPublications = []agentcontract.Revision{{
				ID: publication.ID, PublicationID: publication.PublicationID, ProjectID: publication.ProjectID,
				Kind: publication.Kind, Labels: publication.Labels, Definition: draft, Digest: agentcontract.DraftDigest(draft),
			}}
			h.rt.release.Snapshot.Providers = promptTestProviders(h.rt.release.Snapshot.Providers, "openai", "", "")
			route := h.rt.release.Snapshot.Routes[routeSlug]
			limit := int64(1024)
			h.rt.release.Snapshot.Routes = map[string]routing.Route{
				routeSlug: promptTestRoute(route, route.Fidelity, routing.Behavior{MaxBodyBytes: &limit}),
			}
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			request := fmt.Sprintf(tc.body, publicationID)
			response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(request), nil)
			defer response.Body.Close()
			if body, _ := io.ReadAll(response.Body); response.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			if attempts := h.sink.last(t).Attempts; len(attempts) != 0 {
				t.Fatalf("refusal dispatched an attempt: %#v", attempts)
			}
			assertPromptNotDispatched(t, dispatched)
		})
	}
}

func TestPublishedPromptStreamsAcrossSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name      string
		path      string
		body      string
		firstText string
		lastText  string
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"` + routeSlug + `","stream":true,"prompt":{"id":"olpp_%s","label":"production","variables":{"name":"Ada"}}}`, firstText: "streamed answer", lastText: "streamed answer"},
		{name: "anthropic", path: "/anthropic/v1/messages", body: `{"model":"` + routeSlug + `","max_tokens":20,"stream":true,"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}}`, firstText: "word 0", lastText: "word 63"},
		{name: "gemini", path: "/gemini/v1beta/models/" + routeSlug + ":streamGenerateContent?alt=sse", body: `{"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}}`, firstText: "word 0", lastText: "word 63"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, publicationID, revisionID := publishedPromptHarness(t)
			h.rt.release.Snapshot.Providers = promptTestProviders(h.rt.release.Snapshot.Providers, "openai", "", "")
			upstreamBody := make(chan []byte, 1)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstreamBody <- body
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.name == "responses" {
					for _, event := range streamedEvents("streamed answer") {
						fmt.Fprintf(w, "data: %s\n\n", event)
					}
					return
				}
				_, _ = w.Write(benchChatStream())
			})
			request := fmt.Sprintf(tc.body, publicationID)
			response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(request), nil)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusOK || !strings.Contains(string(body), tc.firstText) || !strings.Contains(string(body), tc.lastText) {
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			if upstream := string(<-upstreamBody); !strings.Contains(upstream, "Hello Ada") || strings.Contains(upstream, "olp_prompt") || strings.Contains(upstream, `"prompt":`) {
				t.Fatalf("upstream request = %s", upstream)
			}
			if fact := h.sink.last(t).Attempts[0].Prompt; fact == nil || fact.ID != publicationID || fact.RevisionID != revisionID {
				t.Fatalf("attempt prompt = %#v", fact)
			}
		})
	}
}

func TestPublishedPromptStructuredMessagesAcrossNativeSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name, profile, path, request, reply string
	}{
		{name: "responses", path: "/v1/responses", request: `{"model":"` + routeSlug + `","prompt":{"id":"olpp_%s","label":"production","variables":{"name":"Ada"}}}`, reply: `{"id":"resp_1","object":"response","model":"model-a","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`},
		{name: "anthropic", profile: "anthropic-messages", path: "/anthropic/v1/messages", request: `{"model":"` + routeSlug + `","olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}}`, reply: `{"id":"msg_1","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`},
		{name: "gemini", profile: "gemini-generation", path: "/gemini/v1beta/models/" + routeSlug + ":generateContent", request: `{"olp_prompt":{"id":"%s","label":"production","variables":{"name":"Ada"}}}`, reply: `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, publicationID, _ := publishedPromptHarness(t)
			definition := []byte(`{"messages":[{"role":"system","content":"Help {{name}}"},{"role":"user","content":"First"},{"role":"assistant","content":"Second"},{"role":"user","content":"Hello {{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false},"default_parameters":{"temperature":0.25,"max_output_tokens":32}}`)
			var prompt prompts.Definition
			if err := json.Unmarshal(definition, &prompt); err != nil {
				t.Fatal(err)
			}
			h.rt.release.Snapshot.AgentPublications[0].Definition.Prompt = &prompt
			if tc.name != "responses" {
				h.rt.release.Snapshot.Providers = promptTestProviders(h.rt.release.Snapshot.Providers, tc.name, tc.profile, connectors.ProfileRevision)
			}
			upstream := make(chan []byte, 1)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstream <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.reply)
			})
			response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(fmt.Sprintf(tc.request, publicationID)), nil)
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			var sent struct {
				Input             []prompts.Message `json:"input"`
				Messages          []prompts.Message `json:"messages"`
				System            string            `json:"system"`
				SystemInstruction struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"systemInstruction"`
				Contents []struct {
					Role  string `json:"role"`
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"contents"`
				Temperature      *float64 `json:"temperature"`
				MaxTokens        *int     `json:"max_tokens"`
				MaxOutputTokens  *int     `json:"max_output_tokens"`
				GenerationConfig struct {
					Temperature     float64 `json:"temperature"`
					MaxOutputTokens int     `json:"maxOutputTokens"`
				} `json:"generationConfig"`
			}
			if err := json.Unmarshal(<-upstream, &sent); err != nil {
				t.Fatal(err)
			}
			expected := []prompts.Message{{Role: "user", Content: "First"}, {Role: "assistant", Content: "Second"}, {Role: "user", Content: "Hello Ada"}}
			switch tc.name {
			case "responses":
				if len(sent.Input) != 4 || sent.Input[0] != (prompts.Message{Role: "system", Content: "Help Ada"}) || !slices.Equal(sent.Input[1:], expected) || sent.Temperature == nil || *sent.Temperature != .25 || sent.MaxOutputTokens == nil || *sent.MaxOutputTokens != 32 {
					t.Fatalf("responses upstream = %#v", sent)
				}
			case "anthropic":
				if sent.System != "Help Ada" || !slices.Equal(sent.Messages, expected) || sent.Temperature == nil || *sent.Temperature != .25 || sent.MaxTokens == nil || *sent.MaxTokens != 32 {
					t.Fatalf("anthropic upstream = %#v", sent)
				}
			case "gemini":
				if len(sent.SystemInstruction.Parts) != 1 || sent.SystemInstruction.Parts[0].Text != "Help Ada" || len(sent.Contents) != 3 || sent.Contents[0].Role != "user" || sent.Contents[0].Parts[0].Text != "First" || sent.Contents[1].Role != "model" || sent.Contents[1].Parts[0].Text != "Second" || sent.Contents[2].Role != "user" || sent.Contents[2].Parts[0].Text != "Hello Ada" || sent.GenerationConfig.Temperature != .25 || sent.GenerationConfig.MaxOutputTokens != 32 {
					t.Fatalf("gemini upstream = %#v", sent)
				}
			}
		})
	}
}

func TestPublishedPromptDefaultsCannotExceedRouteModelLimit(t *testing.T) {
	h, publicationID, _ := publishedPromptHarness(t)
	defaultTokens := int64(32)
	h.rt.release.Snapshot.AgentPublications[0].Definition.Prompt = &prompts.Definition{Template: "Hello {{name}}", Variables: []string{"name"}, DefaultParameters: &prompts.Parameters{MaxOutputTokens: &defaultTokens}}
	h.rt.release.Snapshot.Providers = maps.Collect(func(yield func(string, routing.Provider) bool) {
		for id, provider := range h.rt.release.Snapshot.Providers {
			if !yield(id, routing.Provider{ID: provider.ID, Name: provider.Name, Kind: provider.Kind, Enabled: provider.Enabled,
				ActiveCredential: provider.ActiveCredential, RevisionID: provider.RevisionID, Endpoint: provider.Endpoint, AuthMode: provider.AuthMode,
				Slots: provider.Slots, Capabilities: provider.Capabilities, Models: map[string]json.RawMessage{provider.Capabilities[0].Model: json.RawMessage(`{"max_output_tokens":16}`)}}) {
				return
			}
		}
	})
	upstream, dispatched := promptRefusalObserver()
	h.mock.set("a", upstream)
	h.mock.set("b", upstream)
	request := fmt.Sprintf(`{"model":%q,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, publicationID)
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
	defer response.Body.Close()
	if response.StatusCode < 400 {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("a default exceeding the route's model limit dispatched: %d %s", response.StatusCode, body)
	}
	assertPromptNotDispatched(t, dispatched)
	h.mock.set("a", completion(modelA, "ok"))
	allowed := fmt.Sprintf(`{"model":%q,"max_completion_tokens":8,"olp_prompt":{"id":%q,"label":"production","variables":{"name":"Ada"}}}`, routeSlug, publicationID)
	allowedResponse := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(allowed), nil)
	defer allowedResponse.Body.Close()
	if allowedResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(allowedResponse.Body)
		t.Fatalf("narrower explicit limit rejected: %d %s", allowedResponse.StatusCode, body)
	}
}

func TestStructuredPromptGuardrailsInspectEveryMessage(t *testing.T) {
	for _, surface := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"` + routeSlug + `","olp_prompt":{"id":"%s","label":"production","variables":{"name":"forbidden"}}}`},
		{"responses", "/v1/responses", `{"model":"` + routeSlug + `","prompt":{"id":"olpp_%s","label":"production","variables":{"name":"forbidden"}}}`},
		{"anthropic", "/anthropic/v1/messages", `{"model":"` + routeSlug + `","max_tokens":20,"olp_prompt":{"id":"%s","label":"production","variables":{"name":"forbidden"}}}`},
		{"gemini", "/gemini/v1beta/models/" + routeSlug + ":generateContent", `{"olp_prompt":{"id":"%s","label":"production","variables":{"name":"forbidden"}}}`},
	} {
		for _, role := range []string{"system", "user", "assistant"} {
			t.Run(surface.name+"/"+role, func(t *testing.T) {
				h, publicationID, _ := publishedPromptHarness(t)
				upstream, dispatched := promptRefusalObserver()
				h.mock.set("a", upstream)
				h.mock.set("b", upstream)
				snapshot := h.rt.release.Snapshot
				route := snapshot.Routes[routeSlug]
				projectID := *route.ProjectID
				definition := prompts.Definition{Messages: []prompts.Message{{Role: role, Content: "{{name}}"}, {Role: "user", Content: "hello"}}, VariableSchema: &prompts.VariableSchema{Type: "object", Properties: map[string]prompts.VariableProperty{"name": {Type: "string"}}, Required: []string{"name"}}}
				publication := snapshot.AgentPublications[0]
				draft := agentcontract.Draft{ProjectID: publication.ProjectID, Name: publication.Definition.Name, Prompt: &definition}
				snapshot.AgentPublications = []agentcontract.Revision{{ID: publication.ID, PublicationID: publication.PublicationID, ProjectID: publication.ProjectID, Kind: publication.Kind, Labels: publication.Labels, Definition: draft, Digest: agentcontract.DraftDigest(draft)}}
				snapshot.Guardrails = &guardrails.Registry{
					Definitions: map[string]guardrails.Revision{"revision": {ID: "revision", GuardrailID: "guard", ProjectID: projectID, Name: "Guard", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["forbidden"]}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}},
					Policies:    map[string]guardrails.PolicyRevision{"policy-revision": {ID: "policy-revision", PolicyID: "policy", ProjectID: projectID, Entries: []guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrails.Input, Action: guardrails.Block}}}},
					Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: route.ID, PolicyRevisionID: "policy-revision"}},
				}
				if err := snapshot.Compile(); err != nil {
					t.Fatal(err)
				}
				snapshot.Providers = promptTestProviders(snapshot.Providers, "openai", "", "")
				response := h.do(t.Context(), http.MethodPost, surface.path, fullKey, []byte(fmt.Sprintf(surface.body, publicationID)), nil)
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode < 400 || !strings.Contains(string(body), "guardrail_blocked") {
					t.Fatalf("status = %d, body = %s", response.StatusCode, body)
				}
				assertPromptNotDispatched(t, dispatched)
			})
		}
	}
}
