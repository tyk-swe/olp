package kernel

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/domain/prompts"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	promptProject     = "11111111-1111-4111-8111-111111111111"
	promptRevision    = "22222222-2222-4222-8222-222222222222"
	promptPublication = "33333333-3333-4333-8333-333333333333"
)

type promptHarness struct {
	*harness
	mu   sync.Mutex
	sent []map[string]any
}

func newPromptHarness(t *testing.T) *promptHarness {
	t.Helper()
	h := &promptHarness{harness: newHarness(t, Config{})}
	project, temperature := promptProject, 0.25
	draft := agentcontract.Draft{ProjectID: project, Name: "greeter", Prompt: &prompts.Definition{
		Messages:          []prompts.Message{{Role: "system", Content: "Be kind to {{name}}."}, {Role: "user", Content: "Greet {{name}}."}},
		VariableSchema:    &prompts.VariableSchema{Type: "object", Properties: map[string]prompts.VariableProperty{"name": {Type: "string"}}, Required: []string{"name"}},
		DefaultParameters: &prompts.Parameters{Temperature: &temperature},
	}}
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.ProjectID = &project
		s.Routes[routeSlug] = route
		s.AgentPublications = []agentcontract.Revision{{
			ID: promptRevision, PublicationID: promptPublication, ProjectID: project, Kind: "prompt",
			Labels: []string{"production"}, Definition: draft, Digest: agentcontract.DraftDigest(draft),
		}}
	})
	h.rt.mu.Lock()
	key := h.rt.keys[fullKey]
	key.ProjectID = &project
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("provider request is not JSON: %v", err)
		}
		h.mu.Lock()
		h.sent = append(h.sent, body)
		h.mu.Unlock()
		completion(modelA, answerText)(w, r)
	})
	return h
}

func (h *promptHarness) post(path, body string) (int, string) {
	h.t.Helper()
	return h.postAs(fullKey, path, body)
}

func (h *promptHarness) postAs(key, path, body string) (int, string) {
	h.t.Helper()
	resp := h.do(h.t.Context(), http.MethodPost, path, key, []byte(body), nil)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	var reply struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &reply)
	return resp.StatusCode, reply.Error.Code + ": " + reply.Error.Message
}

func (h *promptHarness) provider() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sent
}

func TestAPublishedPromptIsRenderedForTheProviderWithItsDefaultsAndRecordedOnTheAttempt(t *testing.T) {
	rendered := []any{
		map[string]any{"role": "system", "content": "Be kind to Ada."},
		map[string]any{"role": "user", "content": "Greet Ada."},
	}
	for _, test := range []struct{ name, path, body, input string }{
		{"chat by label", "/v1/chat/completions", `{"model":"team-chat","olp_prompt":{"id":"` + promptPublication + `","label":"production","variables":{"name":"Ada"}}}`, "messages"},
		{"responses by revision", "/v1/responses", `{"model":"team-chat","prompt":{"id":"olpp_` + promptPublication + `","revision_id":"` + promptRevision + `","variables":{"name":"Ada"}}}`, "input"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPromptHarness(t)
			h.post(test.path, test.body)
			sent := h.provider()
			if len(sent) != 1 {
				t.Fatalf("the provider received %d requests", len(sent))
			}
			if !reflect.DeepEqual(sent[0][test.input], rendered) || sent[0]["temperature"] != 0.25 {
				t.Fatalf("the provider did not receive the rendered prompt and its default: %v", sent[0])
			}
			for _, reference := range []string{"olp_prompt", "prompt"} {
				if _, ok := sent[0][reference]; ok {
					t.Fatalf("the prompt reference reached the provider: %v", sent[0])
				}
			}
			attempts := h.sink.last(t).Attempts
			if len(attempts) != 1 || attempts[0].Prompt == nil || attempts[0].Prompt.ID != promptPublication || attempts[0].Prompt.RevisionID != promptRevision {
				t.Fatalf("the attempt does not name the prompt revision it ran: %+v", attempts)
			}
		})
	}
}

func TestACallerSettingKeepsItsValueOverThePromptDefault(t *testing.T) {
	h := newPromptHarness(t)
	h.post("/v1/chat/completions", `{"model":"team-chat","temperature":0.9,"olp_prompt":{"id":"`+promptPublication+`","label":"production","variables":{"name":"Ada"}}}`)
	if sent := h.provider(); len(sent) != 1 || sent[0]["temperature"] != 0.9 {
		t.Fatalf("the prompt default replaced the caller's temperature: %v", sent)
	}
}

func TestPublishedPromptRequestsThatCannotBeRenderedAreRefusedBeforeTheProvider(t *testing.T) {
	reference := func(fields string) string {
		return `{"model":"team-chat","olp_prompt":{"id":"` + promptPublication + `",` + fields + `}}`
	}
	for _, test := range []struct{ name, path, body, want string }{
		{"combined with messages", "/v1/chat/completions", `{"model":"team-chat","messages":[],"olp_prompt":{"id":"` + promptPublication + `","label":"production","variables":{"name":"Ada"}}}`, "invalid_prompt: A published prompt cannot be combined with messages."},
		{"label and revision together", "/v1/chat/completions", reference(`"label":"production","revision_id":"` + promptRevision + `","variables":{"name":"Ada"}`), "invalid_prompt: Specify a prompt ID, exactly one label or revision ID, and string variables."},
		{"neither label nor revision", "/v1/chat/completions", reference(`"variables":{"name":"Ada"}`), "invalid_prompt: Specify a prompt ID, exactly one label or revision ID, and string variables."},
		{"unknown field", "/v1/chat/completions", reference(`"label":"production","variables":{"name":"Ada"},"extra":1`), "invalid_prompt: Specify a prompt ID, exactly one label or revision ID, and string variables."},
		{"empty label beside a revision", "/v1/chat/completions", reference(`"label":"","revision_id":"` + promptRevision + `","variables":{"name":"Ada"}`), "invalid_prompt: Specify a prompt ID, exactly one label or revision ID, and string variables."},
		{"empty label alone", "/v1/chat/completions", reference(`"label":"","variables":{"name":"Ada"}`), "invalid_prompt: Specify a prompt ID, exactly one label or revision ID, and string variables."},
		{"null variable", "/v1/chat/completions", reference(`"label":"production","variables":{"name":null}`), "invalid_prompt: Prompt variables must be an object of strings."},
		{"variables missing", "/v1/chat/completions", reference(`"label":"production"`), "invalid_prompt: Prompt variables must be an object of strings."},
		{"variable that is not a string", "/v1/chat/completions", reference(`"label":"production","variables":{"name":1}`), "invalid_prompt: Specify a prompt ID, exactly one label or revision ID, and string variables."},
		{"variable the prompt does not declare", "/v1/chat/completions", reference(`"label":"production","variables":{"name":"Ada","other":"x"}`), "invalid_prompt: Prompt variables do not match the published definition."},
		{"declared variable absent", "/v1/chat/completions", reference(`"label":"production","variables":{}`), "invalid_prompt: Prompt variables do not match the published definition."},
		{"label no revision carries", "/v1/chat/completions", reference(`"label":"staging","variables":{"name":"Ada"}`), "invalid_prompt: The published prompt revision was not found for this project."},
		{"unknown revision", "/v1/chat/completions", reference(`"revision_id":"44444444-4444-4444-8444-444444444444","variables":{"name":"Ada"}`), "invalid_prompt: The published prompt revision was not found for this project."},
		{"unknown route", "/v1/chat/completions", `{"model":"no-such-route","olp_prompt":{"id":"` + promptPublication + `","label":"production","variables":{"name":"Ada"}}}`, "route_not_found: The model `no-such-route` does not exist or you do not have access to it."},
		{"responses instructions", "/v1/responses", `{"model":"team-chat","instructions":"x","prompt":{"id":"olpp_` + promptPublication + `","label":"production","variables":{"name":"Ada"}}}`, "invalid_prompt: A published prompt cannot be combined with messages."},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPromptHarness(t)
			status, refusal := h.post(test.path, test.body)
			if status != http.StatusBadRequest && status != http.StatusNotFound || refusal != test.want {
				t.Fatalf("got %d %q, want %q", status, refusal, test.want)
			}
			if sent := h.provider(); len(sent) != 0 {
				t.Fatalf("a refused prompt reached the provider: %v", sent)
			}
		})
	}
}

func TestAPublishedPromptIsRefusedOnAStrictRouteAndForAnotherProject(t *testing.T) {
	body := `{"model":"team-chat","olp_prompt":{"id":"` + promptPublication + `","label":"production","variables":{"name":"Ada"}}}`
	for _, test := range []struct {
		name    string
		publish func(*promptHarness)
		want    string
	}{
		{"strict route", func(h *promptHarness) { strictRelease(h.t, h.harness, nil) }, "invalid_prompt: Published prompts require a transformed route."},
		{"prompt of another project", func(h *promptHarness) {
			other := "55555555-5555-4555-8555-555555555555"
			revision := h.rt.Release().Snapshot().AgentPublications()[0]
			revision.ProjectID, revision.Definition.ProjectID = other, other
			revision.Digest = agentcontract.DraftDigest(revision.Definition)
			h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
				s.AgentPublications = []agentcontract.Revision{revision}
			})
		}, "invalid_prompt: The published prompt revision was not found for this project."},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPromptHarness(t)
			test.publish(h)
			if status, refusal := h.post("/v1/chat/completions", body); status != http.StatusBadRequest || refusal != test.want {
				t.Fatalf("got %d %q, want %q", status, refusal, test.want)
			}
			if sent := h.provider(); len(sent) != 0 {
				t.Fatalf("a refused prompt reached the provider: %v", sent)
			}
		})
	}
}

func TestAKeyWithoutTheRouteLearnsNothingAboutItsPublishedPrompts(t *testing.T) {
	h := newPromptHarness(t)
	for _, id := range []string{promptPublication, "44444444-4444-4444-8444-444444444444"} {
		body := `{"model":"team-chat","olp_prompt":{"id":"` + id + `","label":"production","variables":{"name":"Ada"}}}`
		status, refusal := h.postAs(otherKey, "/v1/chat/completions", body)
		if status != http.StatusForbidden {
			t.Fatalf("prompt %s: got %d %q, want the route to be forbidden", id, status, refusal)
		}
	}
	if sent := h.provider(); len(sent) != 0 {
		t.Fatalf("a forbidden prompt reached the provider: %v", sent)
	}
}
