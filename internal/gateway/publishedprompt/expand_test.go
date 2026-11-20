package publishedprompt_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/prompts"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/publishedprompt"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	project     = "11111111-1111-4111-8111-111111111111"
	revision    = "22222222-2222-4222-8222-222222222222"
	publication = "33333333-3333-4333-8333-333333333333"
	slug        = "team-chat"
	reference   = `{"id":"` + publication + `","label":"production","variables":{"name":"Ada"}}`
	installed   = int64(64 << 10)
)

func conversation() []prompts.Message {
	return []prompts.Message{
		{Role: "system", Content: "Be kind to {{name}}."},
		{Role: "user", Content: "Greet {{name}}."},
		{Role: "assistant", Content: "Hello {{name}}."},
	}
}

type published struct {
	messages   []prompts.Message
	parameters *prompts.Parameters
	routeLimit *int64
	strict     bool
	admit      func(*routing.Route) *fault.Error
}

func (p published) scope() publishedprompt.Scope {
	owner := project
	draft := agentcontract.Draft{ProjectID: project, Name: "greeter", Prompt: &prompts.Definition{
		Messages:          p.messages,
		VariableSchema:    &prompts.VariableSchema{Type: "object", Properties: map[string]prompts.VariableProperty{"name": {Type: "string"}}, Required: []string{"name"}},
		DefaultParameters: p.parameters,
	}}
	route := routing.Route{Slug: slug, ProjectID: &owner}
	route.MaxBodyBytes = p.routeLimit
	if !p.strict {
		route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityTransformed}
	}
	snapshot := &routing.Snapshot{
		Routes: map[string]routing.Route{slug: route},
		AgentPublications: []agentcontract.Revision{{
			ID: revision, PublicationID: publication, ProjectID: project, Kind: "prompt",
			Labels: []string{"production"}, Definition: draft, Digest: agentcontract.DraftDigest(draft),
		}},
	}
	admit := p.admit
	if admit == nil {
		admit = func(*routing.Route) *fault.Error { return nil }
	}
	return publishedprompt.Scope{Snapshot: snapshot.ReadOnly(), MaxBodyBytes: installed, Admit: admit}
}

func source(t *testing.T, family wire.Format, body string) oif.Document {
	t.Helper()
	doc, err := protocols.ParseSource(family, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func expanded(t *testing.T, p published, family wire.Format, body, pathModel string) map[string]any {
	t.Helper()
	doc, used, e := publishedprompt.Expand(p.scope(), family, source(t, family, body), pathModel)
	if e != nil {
		t.Fatalf("refused: %s %s", e.Code, e.Message)
	}
	if used == nil || used.ID != publication || used.RevisionID != revision {
		t.Fatalf("recorded prompt %+v, want publication %s revision %s", used, publication, revision)
	}
	var fields map[string]any
	if err := json.Unmarshal(doc.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func refused(t *testing.T, p published, family wire.Format, body, pathModel string) *fault.Error {
	t.Helper()
	_, used, e := publishedprompt.Expand(p.scope(), family, source(t, family, body), pathModel)
	if e == nil || used != nil {
		t.Fatalf("expanded with prompt %+v, want a refusal", used)
	}
	return e
}

func said(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func geminiSaid(role, text string) map[string]any {
	return map[string]any{"role": role, "parts": []any{map[string]any{"text": text}}}
}

func TestEachWireFamilyReceivesTheRenderedPromptWhereItsProviderReadsIt(t *testing.T) {
	prompt := published{messages: conversation()}
	for _, test := range []struct {
		name      string
		family    wire.Format
		body      string
		pathModel string
		want      map[string]any
	}{
		{"chat keeps the system message in messages", wire.Chat, `{"model":"team-chat","olp_prompt":` + reference + `}`, "", map[string]any{
			"model":    slug,
			"messages": []any{said("system", "Be kind to Ada."), said("user", "Greet Ada."), said("assistant", "Hello Ada.")},
		}},
		{"responses reads input", wire.Responses, `{"model":"team-chat","prompt":{"id":"olpp_` + publication + `","label":"production","variables":{"name":"Ada"}}}`, "", map[string]any{
			"model": slug,
			"input": []any{said("system", "Be kind to Ada."), said("user", "Greet Ada."), said("assistant", "Hello Ada.")},
		}},
		{"anthropic lifts the system message", wire.Anthropic, `{"model":"team-chat","max_tokens":5,"olp_prompt":` + reference + `}`, "", map[string]any{
			"model": slug, "max_tokens": float64(5), "system": "Be kind to Ada.",
			"messages": []any{said("user", "Greet Ada."), said("assistant", "Hello Ada.")},
		}},
		{"gemini lifts the system instruction and names the model role", wire.Gemini, `{"olp_prompt":` + reference + `}`, slug, map[string]any{
			"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "Be kind to Ada."}}},
			"contents":          []any{geminiSaid("user", "Greet Ada."), geminiSaid("model", "Hello Ada.")},
		}},
		{"gemini streaming", wire.GeminiStream, `{"olp_prompt":` + reference + `}`, slug, map[string]any{
			"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "Be kind to Ada."}}},
			"contents":          []any{geminiSaid("user", "Greet Ada."), geminiSaid("model", "Hello Ada.")},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := expanded(t, prompt, test.family, test.body, test.pathModel); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got  %v\nwant %v", got, test.want)
			}
		})
	}
}

func TestPromptDefaultsFillOnlyWhatTheCallerLeftUnsetUnderEachFamilysFieldNames(t *testing.T) {
	temperature, topP, tokens := 0.25, 0.5, int64(64)
	prompt := published{
		messages:   []prompts.Message{{Role: "user", Content: "Greet {{name}}."}},
		parameters: &prompts.Parameters{Temperature: &temperature, TopP: &topP, MaxOutputTokens: &tokens},
	}
	for _, test := range []struct {
		name      string
		family    wire.Format
		caller    string
		pathModel string
		want      map[string]any
	}{
		{"chat", wire.Chat, `"model":"team-chat",`, "", map[string]any{"temperature": 0.25, "top_p": 0.5, "max_tokens": float64(64)}},
		{"chat caller values win", wire.Chat, `"model":"team-chat","temperature":1,"top_p":0.9,"max_tokens":8,`, "", map[string]any{"temperature": float64(1), "top_p": 0.9, "max_tokens": float64(8)}},
		{"chat completion tokens stand in for the token default", wire.Chat, `"model":"team-chat","max_completion_tokens":8,`, "", map[string]any{"temperature": 0.25, "top_p": 0.5, "max_completion_tokens": float64(8)}},
		{"responses", wire.Responses, `"model":"team-chat",`, "", map[string]any{"temperature": 0.25, "top_p": 0.5, "max_output_tokens": float64(64)}},
		{"anthropic", wire.Anthropic, `"model":"team-chat",`, "", map[string]any{"temperature": 0.25, "top_p": 0.5, "max_tokens": float64(64)}},
		{"gemini without a generation config", wire.Gemini, ``, slug, map[string]any{"generationConfig": map[string]any{"temperature": 0.25, "topP": 0.5, "maxOutputTokens": float64(64)}}},
		{"gemini caller values win", wire.Gemini, `"generationConfig":{"topP":0.9,"candidateCount":2},`, slug, map[string]any{"generationConfig": map[string]any{"temperature": 0.25, "topP": 0.9, "maxOutputTokens": float64(64), "candidateCount": float64(2)}}},
		{"gemini snake case config is the caller's config", wire.Gemini, `"generation_config":{"top_p":0.9,"max_output_tokens":8},`, slug, map[string]any{"generationConfig": map[string]any{"temperature": 0.25, "topP": 0.9, "maxOutputTokens": float64(8)}}},
		{"gemini both spellings keep the camel case value", wire.Gemini, `"generation_config":{"top_p":0.1,"max_output_tokens":8},"generationConfig":{"topP":0.9},`, slug, map[string]any{"generationConfig": map[string]any{"temperature": 0.25, "topP": 0.9, "maxOutputTokens": float64(8)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			field, id := "olp_prompt", publication
			if test.family == wire.Responses {
				field, id = "prompt", "olpp_"+publication
			}
			body := `{` + test.caller + `"` + field + `":{"id":"` + id + `","label":"production","variables":{"name":"Ada"}}}`
			got := expanded(t, prompt, test.family, body, test.pathModel)
			for _, rendered := range []string{"model", "messages", "input", "contents"} {
				delete(got, rendered)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got  %v\nwant %v", got, test.want)
			}
		})
	}
}

func TestAPromptIsRefusedBesideCallerInputOverItsLimitsOrWithoutARoute(t *testing.T) {
	tight := int64(16)
	long := published{messages: []prompts.Message{{Role: "user", Content: "Greet {{name}} at length, please."}}}
	limited := long
	limited.routeLimit = &tight
	forbidden := published{messages: conversation(), admit: func(*routing.Route) *fault.Error {
		return &fault.Error{Status: http.StatusForbidden, Code: "route_forbidden"}
	}}
	for _, test := range []struct {
		name      string
		prompt    published
		family    wire.Format
		body      string
		pathModel string
		status    int
		code      string
	}{
		{"anthropic system beside the prompt", published{messages: conversation()}, wire.Anthropic, `{"model":"team-chat","system":"x","olp_prompt":` + reference + `}`, "", 400, "invalid_prompt"},
		{"gemini system instruction beside the prompt", published{messages: conversation()}, wire.Gemini, `{"systemInstruction":{},"olp_prompt":` + reference + `}`, slug, 400, "invalid_prompt"},
		{"gemini snake case system instruction beside the prompt", published{messages: conversation()}, wire.Gemini, `{"system_instruction":{},"olp_prompt":` + reference + `}`, slug, 400, "invalid_prompt"},
		{"gemini contents beside the prompt", published{messages: conversation()}, wire.Gemini, `{"contents":[],"olp_prompt":` + reference + `}`, slug, 400, "invalid_prompt"},
		{"rendered prompt over the route limit", limited, wire.Chat, `{"model":"team-chat","olp_prompt":` + reference + `}`, "", 413, "request_too_large"},
		{"strict route", published{messages: conversation(), strict: true}, wire.Chat, `{"model":"team-chat","olp_prompt":` + reference + `}`, "", 400, "invalid_prompt"},
		{"route the caller may not use", forbidden, wire.Chat, `{"model":"team-chat","olp_prompt":` + reference + `}`, "", 403, "route_forbidden"},
		{"no model", published{messages: conversation()}, wire.Chat, `{"olp_prompt":` + reference + `}`, "", 400, "missing_required_parameter"},
		{"model that is not text", published{messages: conversation()}, wire.Chat, `{"model":7,"olp_prompt":` + reference + `}`, "", 400, "invalid_model"},
		{"gemini model outside the path", published{messages: conversation()}, wire.Gemini, `{"model":"team-chat","olp_prompt":` + reference + `}`, "", 404, "route_not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if e := refused(t, test.prompt, test.family, test.body, test.pathModel); e.Status != test.status || e.Code != test.code {
				t.Fatalf("got %d %s (%s), want %d %s", e.Status, e.Code, e.Message, test.status, test.code)
			}
		})
	}
	if got := expanded(t, long, wire.Chat, `{"model":"team-chat","olp_prompt":`+reference+`}`, ""); got["messages"] == nil {
		t.Fatalf("the same prompt without a route limit was not rendered: %v", got)
	}
}

func TestAnExpandedRequestOverTheInstallationLimitIsRefused(t *testing.T) {
	prompt := published{messages: []prompts.Message{{Role: "user", Content: "Greet {{name}}."}}}
	scope := prompt.scope()
	body := `{"model":"team-chat","olp_prompt":` + reference + `}`
	fits, _, e := publishedprompt.Expand(scope, wire.Chat, source(t, wire.Chat, body), "")
	if e != nil {
		t.Fatalf("refused: %s", e.Message)
	}
	scope.MaxBodyBytes = int64(fits.Len()) - 1
	if _, _, e := publishedprompt.Expand(scope, wire.Chat, source(t, wire.Chat, body), ""); e == nil || e.Code != "request_too_large" {
		t.Fatalf("got %v, want the expanded request refused as too large", e)
	}
}

func TestARequestWithoutAPromptReferenceIsReturnedUntouchedAndNoReleaseIsUnavailable(t *testing.T) {
	prompt := published{messages: conversation()}
	for _, test := range []struct {
		name   string
		family wire.Format
		body   string
	}{
		{"chat without a reference", wire.Chat, `{"model":"team-chat","messages":[]}`},
		{"responses prompt of the provider", wire.Responses, `{"model":"team-chat","prompt":{"id":"pmpt_123"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := source(t, test.family, test.body)
			if publishedprompt.Referenced(doc, test.family) {
				t.Fatal("reported a published prompt reference")
			}
			got, used, e := publishedprompt.Expand(prompt.scope(), test.family, doc, "")
			if e != nil || used != nil || got.Raw() != doc.Raw() {
				t.Fatalf("got %q prompt %+v refusal %v, want the request untouched", got.Raw(), used, e)
			}
		})
	}
	doc := source(t, wire.Chat, `{"model":"team-chat","olp_prompt":`+reference+`}`)
	if !publishedprompt.Referenced(doc, wire.Chat) {
		t.Fatal("did not report the published prompt reference")
	}
	if _, _, e := publishedprompt.Expand(publishedprompt.Scope{MaxBodyBytes: installed}, wire.Chat, doc, ""); e == nil || e.Status != http.StatusServiceUnavailable {
		t.Fatalf("got %v, want the request refused while no release is available", e)
	}
}
