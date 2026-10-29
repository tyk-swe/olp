package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

type promptSelection struct {
	ID         string            `json:"id"`
	Label      string            `json:"label"`
	RevisionID string            `json:"revision_id"`
	Variables  map[string]string `json:"variables"`
}

func (s *Server) expandPrompt(x *call, body []byte) ([]byte, *usage.PromptReference, *Error) {
	if x.family != wire.Chat || !bytes.Contains(body, []byte("olp_prompt")) && !bytes.Contains(body, []byte(`\u`)) {
		return body, nil, nil
	}
	doc, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		return body, nil, nil
	}
	ref, ok := doc.Root().Lookup("olp_prompt")
	if !ok {
		return body, nil, nil
	}
	param := "olp_prompt"
	if _, ok := doc.Root().Lookup("messages"); ok {
		return nil, nil, invalidRequest("invalid_prompt", "A published prompt cannot be combined with messages.", &param)
	}
	_, hasLabel := ref.Lookup("label")
	_, hasRevision := ref.Lookup("revision_id")
	var selection promptSelection
	dec := json.NewDecoder(bytes.NewBufferString(ref.Raw()))
	dec.DisallowUnknownFields()
	if dec.Decode(&selection) != nil || dec.Decode(new(any)) != io.EOF || selection.ID == "" || hasLabel == hasRevision || (selection.Label == "") == (selection.RevisionID == "") {
		return nil, nil, invalidRequest("invalid_prompt", "Specify a prompt ID, exactly one label or revision ID, and string variables.", &param)
	}
	variables, ok := ref.Lookup("variables")
	var rawVariables map[string]json.RawMessage
	if !ok || json.Unmarshal([]byte(variables.Raw()), &rawVariables) != nil || rawVariables == nil {
		return nil, nil, invalidRequest("invalid_prompt", "Prompt variables must be an object of strings.", &param)
	}
	for _, value := range rawVariables {
		if len(value) == 0 || value[0] != '"' {
			return nil, nil, invalidRequest("invalid_prompt", "Prompt variables must be an object of strings.", &param)
		}
	}
	model, ok := doc.Root().Lookup("model")
	if !ok {
		return nil, nil, invalidRequest("missing_required_parameter", "model is required.", nil)
	}
	var slug string
	if json.Unmarshal([]byte(model.Raw()), &slug) != nil {
		return nil, nil, invalidRequest("invalid_model", "model must be a string.", nil)
	}
	if x.snapshot() == nil {
		return nil, nil, serverError(http.StatusServiceUnavailable, "runtime_unavailable", "No runtime release is available.")
	}
	route, ok := x.snapshot().Routes[slug]
	if !ok {
		return nil, nil, modelNotFound(slug)
	}
	if e := s.keyAuthorizer(x.authority)(&route); e != nil {
		return nil, nil, e
	}
	if route.Fidelity.Strict() {
		return nil, nil, invalidRequest("invalid_prompt", "Published prompts require a transformed route.", &param)
	}
	for _, revision := range x.snapshot().AgentPublications {
		if revision.Kind != "prompt" || revision.PublicationID != selection.ID || route.ProjectID == nil || revision.ProjectID != *route.ProjectID || revision.Definition.Prompt == nil {
			continue
		}
		if selection.RevisionID != "" && revision.ID != selection.RevisionID || selection.Label != "" && !slices.Contains(revision.Labels, selection.Label) {
			continue
		}
		rendered, err := revision.Definition.Prompt.Render(selection.Variables)
		if err != nil {
			return nil, nil, invalidRequest("invalid_prompt", "Prompt variables do not match the published definition.", &param)
		}
		messages, err := json.Marshal([]struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: rendered}})
		if err != nil {
			return nil, nil, serverError(http.StatusInternalServerError, "internal_error", "The prompt could not be prepared.")
		}
		expanded, err := oif.Apply(doc, []oif.Change{
			{Pointer: "/olp_prompt", Remove: true, Origin: oif.ResourceBinding, Reason: "prompt reference consumed"},
			{Pointer: "/messages", Value: string(messages), Origin: oif.ResourceBinding, Reason: "published prompt rendered"},
		})
		if err != nil {
			return nil, nil, serverError(http.StatusInternalServerError, "internal_error", "The prompt could not be prepared.")
		}
		renderedBody := expanded.Bytes()
		if int64(len(renderedBody)) > s.cfg.MaxBodyBytes {
			return nil, nil, bodyTooLarge()
		}
		if x.request.body != nil {
			x.request.body.decoded = max(x.request.body.decoded, int64(len(renderedBody)))
		}
		return renderedBody, &usage.PromptReference{ID: revision.PublicationID, RevisionID: revision.ID}, nil
	}
	return nil, nil, invalidRequest("invalid_prompt", "The published prompt revision was not found for this project.", &param)
}
