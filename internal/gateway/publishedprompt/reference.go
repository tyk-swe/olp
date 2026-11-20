package publishedprompt

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
)

type selection struct {
	ID         string            `json:"id"`
	Label      string            `json:"label"`
	RevisionID string            `json:"revision_id"`
	Variables  map[string]string `json:"variables"`
}

type reference struct {
	value oif.Value
	param string
	input string
}

func referenceIn(doc oif.Document, family wire.Format) (reference, bool) {
	if family == wire.Responses {
		ref, ok := doc.Root().Lookup("prompt")
		id, _ := ref.Lookup("id")
		value, valid := id.Chars()
		if !ok || !valid || !strings.HasPrefix(value, "olpp_") {
			return reference{}, false
		}
		return reference{value: ref, param: "prompt", input: "input"}, true
	}
	ref, ok := doc.Root().Lookup("olp_prompt")
	if gemini(family) {
		return reference{value: ref, param: "olp_prompt", input: "contents"}, ok
	}
	return reference{value: ref, param: "olp_prompt", input: "messages"}, ok
}

func (ref reference) invalid(message string) *fault.Error {
	return fault.InvalidRequest("invalid_prompt", message, &ref.param)
}

func (ref reference) selection(doc oif.Document, family wire.Format) (selection, *fault.Error) {
	for _, field := range inputFields(family, ref.input) {
		if _, ok := doc.Root().Lookup(field); ok {
			return selection{}, ref.invalid("A published prompt cannot be combined with messages.")
		}
	}
	_, hasLabel := ref.value.Lookup("label")
	_, hasRevision := ref.value.Lookup("revision_id")
	var chosen selection
	dec := json.NewDecoder(bytes.NewBufferString(ref.value.Raw()))
	dec.DisallowUnknownFields()
	if dec.Decode(&chosen) != nil || dec.Decode(new(any)) != io.EOF || chosen.ID == "" || hasLabel == hasRevision || (chosen.Label == "") == (chosen.RevisionID == "") {
		return selection{}, ref.invalid("Specify a prompt ID, exactly one label or revision ID, and string variables.")
	}
	variables, ok := ref.value.Lookup("variables")
	var raw map[string]json.RawMessage
	if !ok || json.Unmarshal([]byte(variables.Raw()), &raw) != nil || raw == nil {
		return selection{}, ref.invalid("Prompt variables must be an object of strings.")
	}
	for _, value := range raw {
		if len(value) == 0 || value[0] != '"' {
			return selection{}, ref.invalid("Prompt variables must be an object of strings.")
		}
	}
	return chosen, nil
}

func routeSlug(doc oif.Document, family wire.Format, pathModel string) (string, *fault.Error) {
	if pathModel != "" || gemini(family) {
		return pathModel, nil
	}
	model, ok := doc.Root().Lookup("model")
	if !ok {
		return "", fault.InvalidRequest("missing_required_parameter", "model is required.", nil)
	}
	slug, valid := model.Chars()
	if !valid {
		return "", fault.InvalidRequest("invalid_model", "model must be a string.", nil)
	}
	return slug, nil
}

func publicationID(family wire.Format, id string) string {
	if family == wire.Responses {
		return strings.TrimPrefix(id, "olpp_")
	}
	return id
}

func inputFields(family wire.Format, input string) []string {
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
