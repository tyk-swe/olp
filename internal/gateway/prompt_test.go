package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/agentcontract"
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
