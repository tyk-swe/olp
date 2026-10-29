package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

type promptSelection struct {
	ID         string            `json:"id"`
	Label      string            `json:"label"`
	RevisionID string            `json:"revision_id"`
	Variables  map[string]string `json:"variables"`
}

func (s *Server) parsePrompt(x *call, family wire.Format, body []byte, pathModel string) (*wire.Request, *Error) {
	doc, err := protocols.ParseSource(family, body)
	if err != nil {
		return nil, requestError(err)
	}
	expanded, prompt, promptErr := s.expandPrompt(x, family, doc, pathModel)
	if promptErr != nil {
		return nil, promptErr
	}
	x.prompt = prompt
	parsed, err := protocols.ParseDocument(family, expanded, pathModel)
	if err != nil {
		return nil, requestError(err)
	}
	return parsed, nil
}

func promptReference(doc oif.Document, family wire.Format) (oif.Value, string, string, bool) {
	if family == wire.Responses {
		ref, ok := doc.Root().Lookup("prompt")
		id, _ := ref.Lookup("id")
		value, valid := id.Chars()
		if !ok || !valid || !strings.HasPrefix(value, "olpp_") {
			return oif.Value{}, "", "", false
		}
		return ref, "prompt", "input", true
	}
	ref, ok := doc.Root().Lookup("olp_prompt")
	if family == wire.Gemini || family == wire.GeminiStream {
		return ref, "olp_prompt", "contents", ok
	}
	return ref, "olp_prompt", "messages", ok
}

func promptRoute(doc oif.Document, family wire.Format, pathModel string) (string, *Error) {
	if family == wire.Gemini || family == wire.GeminiStream {
		return pathModel, nil
	}
	model, ok := doc.Root().Lookup("model")
	if !ok {
		return "", invalidRequest("missing_required_parameter", "model is required.", nil)
	}
	slug, valid := model.Chars()
	if !valid {
		return "", invalidRequest("invalid_model", "model must be a string.", nil)
	}
	return slug, nil
}

func promptPublicationID(family wire.Format, id string) string {
	if family == wire.Responses {
		return strings.TrimPrefix(id, "olpp_")
	}
	return id
}

func promptInputFields(family wire.Format, input string) []string {
	switch family {
	case wire.Responses:
		return []string{input, "instructions"}
	case wire.Anthropic:
		return []string{input, "system"}
	case wire.Gemini, wire.GeminiStream:
		return []string{input, "systemInstruction"}
	default:
		return []string{input}
	}
}

func promptMessages(family wire.Format, rendered string) ([]byte, error) {
	if family == wire.Gemini || family == wire.GeminiStream {
		type part struct {
			Text string `json:"text"`
		}
		type content struct {
			Role  string `json:"role"`
			Parts []part `json:"parts"`
		}
		return json.Marshal([]content{{Role: "user", Parts: []part{{Text: rendered}}}})
	}
	return json.Marshal([]struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{{Role: "user", Content: rendered}})
}

func (s *Server) expandPrompt(x *call, family wire.Format, doc oif.Document, pathModel string) (oif.Document, *usage.PromptReference, *Error) {
	ref, param, input, ok := promptReference(doc, family)
	if !ok {
		return doc, nil, nil
	}
	for _, field := range promptInputFields(family, input) {
		if _, ok := doc.Root().Lookup(field); ok {
			return oif.Document{}, nil, invalidRequest("invalid_prompt", "A published prompt cannot be combined with messages.", &param)
		}
	}
	_, hasLabel := ref.Lookup("label")
	_, hasRevision := ref.Lookup("revision_id")
	var selection promptSelection
	dec := json.NewDecoder(bytes.NewBufferString(ref.Raw()))
	dec.DisallowUnknownFields()
	if dec.Decode(&selection) != nil || dec.Decode(new(any)) != io.EOF || selection.ID == "" || hasLabel == hasRevision || (selection.Label == "") == (selection.RevisionID == "") {
		return oif.Document{}, nil, invalidRequest("invalid_prompt", "Specify a prompt ID, exactly one label or revision ID, and string variables.", &param)
	}
	variables, ok := ref.Lookup("variables")
	var rawVariables map[string]json.RawMessage
	if !ok || json.Unmarshal([]byte(variables.Raw()), &rawVariables) != nil || rawVariables == nil {
		return oif.Document{}, nil, invalidRequest("invalid_prompt", "Prompt variables must be an object of strings.", &param)
	}
	for _, value := range rawVariables {
		if len(value) == 0 || value[0] != '"' {
			return oif.Document{}, nil, invalidRequest("invalid_prompt", "Prompt variables must be an object of strings.", &param)
		}
	}
	slug, routeErr := promptRoute(doc, family, pathModel)
	if routeErr != nil {
		return oif.Document{}, nil, routeErr
	}
	if x.snapshot() == nil {
		return oif.Document{}, nil, serverError(http.StatusServiceUnavailable, "runtime_unavailable", "No runtime release is available.")
	}
	route, ok := x.snapshot().Routes[slug]
	if !ok {
		return oif.Document{}, nil, modelNotFound(slug)
	}
	if e := s.keyAuthorizer(x.authority)(&route); e != nil {
		return oif.Document{}, nil, e
	}
	if route.Fidelity.Strict() {
		return oif.Document{}, nil, invalidRequest("invalid_prompt", "Published prompts require a transformed route.", &param)
	}
	publicationID := promptPublicationID(family, selection.ID)
	for _, revision := range x.snapshot().AgentPublications {
		if revision.Kind != "prompt" || revision.PublicationID != publicationID || route.ProjectID == nil || revision.ProjectID != *route.ProjectID || revision.Definition.Prompt == nil {
			continue
		}
		if selection.RevisionID != "" && revision.ID != selection.RevisionID || selection.Label != "" && !slices.Contains(revision.Labels, selection.Label) {
			continue
		}
		rendered, err := revision.Definition.Prompt.Render(selection.Variables)
		if err != nil {
			return oif.Document{}, nil, invalidRequest("invalid_prompt", "Prompt variables do not match the published definition.", &param)
		}
		messages, err := promptMessages(family, rendered)
		if err != nil {
			return oif.Document{}, nil, serverError(http.StatusInternalServerError, "internal_error", "The prompt could not be prepared.")
		}
		expanded, err := oif.Apply(doc, []oif.Change{
			{Pointer: "/" + param, Remove: true, Origin: oif.ResourceBinding, Reason: "prompt reference consumed"},
			{Pointer: "/" + input, Value: string(messages), Origin: oif.ResourceBinding, Reason: "published prompt rendered"},
		})
		if err != nil {
			return oif.Document{}, nil, serverError(http.StatusInternalServerError, "internal_error", "The prompt could not be prepared.")
		}
		if int64(expanded.Len()) > s.cfg.MaxBodyBytes {
			return oif.Document{}, nil, bodyTooLarge()
		}
		if x.request.body != nil {
			x.request.body.decoded = max(x.request.body.decoded, int64(expanded.Len()))
		}
		return expanded, &usage.PromptReference{ID: revision.PublicationID, RevisionID: revision.ID}, nil
	}
	return oif.Document{}, nil, invalidRequest("invalid_prompt", "The published prompt revision was not found for this project.", &param)
}
