package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/prompts"
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

func geminiPromptFamily(family wire.Format) bool {
	return family == wire.Gemini || family == wire.GeminiStream
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
	if geminiPromptFamily(family) {
		return ref, "olp_prompt", "contents", ok
	}
	return ref, "olp_prompt", "messages", ok
}

func promptRoute(doc oif.Document, family wire.Format, pathModel string) (string, *Error) {
	if geminiPromptFamily(family) {
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
		return []string{input, "systemInstruction", "system_instruction"}
	default:
		return []string{input}
	}
}

func promptMessages(family wire.Format, rendered []prompts.Message) ([]byte, error) {
	if geminiPromptFamily(family) {
		type part struct {
			Text string `json:"text"`
		}
		type content struct {
			Role  string `json:"role"`
			Parts []part `json:"parts"`
		}
		values := slices.Collect(func(yield func(content) bool) {
			for _, message := range rendered {
				if !yield(content{Role: geminiPromptRole(message.Role), Parts: []part{{Text: message.Content}}}) {
					return
				}
			}
		})
		return json.Marshal(values)
	}
	return json.Marshal(rendered)
}

func geminiPromptRole(role string) string {
	if role == "assistant" {
		return "model"
	}
	return role
}

type promptDefault struct {
	name  string
	value string
}

func promptDefaultFields(family wire.Format) (string, string) {
	if family == wire.Responses {
		return "max_output_tokens", "top_p"
	}
	if geminiPromptFamily(family) {
		return "maxOutputTokens", "topP"
	}
	return "max_tokens", "top_p"
}

func promptDefaults(family wire.Format, parameters *prompts.Parameters) []promptDefault {
	if parameters == nil {
		return nil
	}
	maxField, topField := promptDefaultFields(family)
	return slices.Collect(func(yield func(promptDefault) bool) {
		if parameters.Temperature != nil && !yield(promptDefault{name: "temperature", value: strconv.FormatFloat(*parameters.Temperature, 'g', -1, 64)}) {
			return
		}
		if parameters.TopP != nil && !yield(promptDefault{name: topField, value: strconv.FormatFloat(*parameters.TopP, 'g', -1, 64)}) {
			return
		}
		if parameters.MaxOutputTokens != nil {
			yield(promptDefault{name: maxField, value: strconv.FormatInt(*parameters.MaxOutputTokens, 10)})
		}
	})
}

func applyPromptDefaults(doc oif.Document, family wire.Format, parameters *prompts.Parameters) (oif.Document, error) {
	defaults := promptDefaults(family, parameters)
	if len(defaults) == 0 {
		return doc, nil
	}
	if !geminiPromptFamily(family) {
		return applyPromptDefaultFields(doc, doc.Root(), "", family, defaults)
	}
	config, exists := doc.Root().Lookup("generationConfig")
	if !exists {
		fields := slices.Collect(func(yield func(string) bool) {
			for _, value := range defaults {
				if !yield(strconv.Quote(value.name) + ":" + value.value) {
					return
				}
			}
		})
		return oif.Apply(doc, []oif.Change{{Pointer: "/generationConfig", Value: "{" + strings.Join(fields, ",") + "}", Origin: oif.ResourceBinding, Reason: "published prompt defaults"}})
	}
	if config.Kind() != oif.Object {
		return doc, nil
	}
	return applyPromptDefaultFields(doc, config, "/generationConfig", family, defaults)
}

func applyPromptDefaultFields(doc oif.Document, container oif.Value, prefix string, family wire.Format, defaults []promptDefault) (oif.Document, error) {
	changes := slices.Collect(func(yield func(oif.Change) bool) {
		for _, value := range defaults {
			if _, exists := container.Lookup(value.name); exists {
				continue
			}
			if family == wire.Chat && value.name == "max_tokens" {
				if _, exists := container.Lookup("max_completion_tokens"); exists {
					continue
				}
			}
			if !yield(oif.Change{Pointer: prefix + "/" + value.name, Value: value.value, Origin: oif.ResourceBinding, Reason: "published prompt defaults"}) {
				return
			}
		}
	})
	return oif.Apply(doc, changes)
}

func promptSystemMessage(family wire.Format, messages []prompts.Message) ([]prompts.Message, []oif.Change, error) {
	if len(messages) == 0 || messages[0].Role != "system" || family != wire.Anthropic && !geminiPromptFamily(family) {
		return messages, nil, nil
	}
	value, err := json.Marshal(messages[0].Content)
	if err != nil {
		return nil, nil, err
	}
	if geminiPromptFamily(family) {
		return messages[1:], []oif.Change{{Pointer: "/systemInstruction", Value: `{"parts":[{"text":` + string(value) + `}]}`, Origin: oif.ResourceBinding, Reason: "published prompt rendered"}}, nil
	}
	return messages[1:], []oif.Change{{Pointer: "/system", Value: string(value), Origin: oif.ResourceBinding, Reason: "published prompt rendered"}}, nil
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
		rendered, err := revision.Definition.Prompt.RenderMessages(selection.Variables)
		if err != nil {
			return oif.Document{}, nil, invalidRequest("invalid_prompt", "Prompt variables do not match the published definition.", &param)
		}
		promptDoc, err := applyPromptDefaults(doc, family, revision.Definition.Prompt.DefaultParameters)
		if err != nil {
			return oif.Document{}, nil, serverError(http.StatusInternalServerError, "internal_error", "The prompt could not be prepared.")
		}
		userMessages, system, err := promptSystemMessage(family, rendered)
		if err != nil {
			return oif.Document{}, nil, serverError(http.StatusInternalServerError, "internal_error", "The prompt could not be prepared.")
		}
		messages, err := promptMessages(family, userMessages)
		if err != nil {
			return oif.Document{}, nil, serverError(http.StatusInternalServerError, "internal_error", "The prompt could not be prepared.")
		}
		expanded, err := oif.Apply(promptDoc, slices.Concat([]oif.Change{
			{Pointer: "/" + param, Remove: true, Origin: oif.ResourceBinding, Reason: "prompt reference consumed"},
			{Pointer: "/" + input, Value: string(messages), Origin: oif.ResourceBinding, Reason: "published prompt rendered"},
		}, system))
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
