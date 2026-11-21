package retained

import (
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

func retainedSubmissionFields() [2]string {
	return [2]string{resources.RetainedSubmissionMarkerField, "olp_submission_digest"}
}

func retainedGeminiFileID(raw string) (string, error) {
	if kernel.ValidGeminiFileID(raw) {
		return raw, nil
	}
	local := strings.TrimPrefix(raw, "files/")
	if local != raw && kernel.ValidGeminiFileID(local) {
		return local, nil
	}
	uri, err := url.Parse(raw)
	if err != nil || uri.User != nil || uri.RawQuery != "" || uri.Fragment != "" {
		return "", resources.ErrNotFound
	}
	local = strings.TrimPrefix(uri.Path, "/gemini/v1beta/files/")
	if local == uri.Path || !kernel.ValidGeminiFileID(local) {
		return "", resources.ErrNotFound
	}
	return local, nil
}

func retainedGeminiFileURI(r *resources.Resource, p *kernel.Pin) (string, error) {
	var metadata struct {
		URI  string `json:"uri"`
		Name string `json:"name"`
	}
	if json.Unmarshal(r.Metadata, &metadata) != nil || metadata.Name != "files/"+r.UpstreamID {
		return "", resources.ErrContract
	}
	uri, err := url.Parse(metadata.URI)
	endpoint, endpointErr := url.Parse(p.Provider.Endpoint)
	if err != nil || endpointErr != nil || uri.User != nil || uri.RawQuery != "" || uri.Fragment != "" || uri.Host != endpoint.Host || uri.Scheme != endpoint.Scheme || !strings.HasSuffix(uri.Path, "/files/"+r.UpstreamID) {
		return "", resources.ErrContract
	}
	return metadata.URI, nil
}

// Walk only native evaluation model positions; application data and schemas
// remain opaque. Grader dictionary keys are labels, including reserved words.
func walkRetainedEvalModels(value oif.Value, path string, visit func(string, string) error) error {
	switch value.Kind() {
	case oif.Array:
		for i, item := range value.Elements() {
			if err := walkRetainedEvalModels(item, path+"/"+strconv.Itoa(i), visit); err != nil {
				return err
			}
		}
	case oif.Object:
		if kind, present := value.Lookup("type"); present {
			if text, ok := kind.Text(); ok && (text == "score_model" || text == "label_model") {
				model, present := value.Lookup("model")
				if text, ok := model.Text(); !present || !ok || text == "" {
					return errors.New("model-based graders require an explicitly authorized model")
				}
			}
		}
		graderMap := strings.HasSuffix(path, "/graders")
		for _, field := range value.Members() {
			if !graderMap && slices.Contains([]string{"metadata", "schema", "json_schema", "item_schema", "parameters"}, field.Name) || path == "/data_source/source" && field.Name == "content" {
				continue
			}
			token := strings.ReplaceAll(strings.ReplaceAll(field.Name, "~", "~0"), "/", "~1")
			pointer := path + "/" + token
			if !graderMap && field.Name == "model" && field.Value.Kind() != oif.Null {
				model, ok := field.Value.Text()
				if !ok || model == "" {
					return resources.ErrContract
				}
				if err := visit(pointer, model); err != nil {
					return err
				}
			}
			if err := walkRetainedEvalModels(field.Value, pointer, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func retainedEvalModelAllowed(p *kernel.Pin, route *routing.Route, owner, model string) bool {
	if model == "" || route == nil || !slices.Contains(route.Operations, "generation") || !p.Slot.Allows(model, route.Slug, owner) || !p.Provider.Supports(model, "eval", "openai", "unary") || !p.Provider.Supports(model, "generation", "openai", "unary") {
		return false
	}
	return slices.ContainsFunc(route.Targets, func(target routing.Target) bool {
		return target.ProviderID == p.Provider.ID && target.ProviderModel == model
	})
}

// Qualify every declared model before choosing a credential. A slot usable for
// the route target alone need not be allowed to invoke its sampling or graders.
func retainedEvalSlots(route *routing.Route, owner string, source oif.Document, parent *resources.Resource) (kernel.RetainedSlotQualifier, error) {
	var models []string
	if parent != nil {
		var metadata struct {
			Models []string `json:"authorized_models"`
		}
		if json.Unmarshal(parent.Metadata, &metadata) != nil {
			return nil, resources.ErrContract
		}
		models = append(models, metadata.Models...)
	}
	if err := walkRetainedEvalModels(source.Root(), "", func(_ string, model string) error {
		if !slices.Contains(models, model) {
			models = append(models, model)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return func(provider *routing.Provider, slot *routing.Slot, model string) bool {
		candidate := &kernel.Pin{Provider: *provider, Slot: *slot, Model: model}
		for _, declared := range models {
			if !retainedEvalModelAllowed(candidate, route, owner, declared) {
				return false
			}
		}
		return true
	}, nil
}

func validateEvalSource(root oif.Value, e retainedapi.Endpoint) error {
	if source, present := root.Lookup("data_source_config"); present {
		kind, _ := source.Lookup("type")
		text, _ := kind.Text()
		if text != "custom" {
			return errors.New("evaluation logs require an owner-bound source contract")
		}
	}
	if e.Kind != "eval_run" || e.Action != "create" {
		return nil
	}
	data, _ := root.Lookup("data_source")
	kind, _ := data.Lookup("type")
	text, _ := kind.Text()
	if !slices.Contains([]string{"jsonl", "completions", "responses"}, text) {
		return errors.New("unsupported evaluation data source")
	}
	if text == "completions" || text == "responses" {
		model, present := data.Lookup("model")
		if value, ok := model.Text(); !present || !ok || value == "" {
			return errors.New("evaluation generation requires an explicitly authorized model")
		}
		if messages, present := data.Lookup("input_messages"); present {
			kind, _ := messages.Lookup("type")
			if value, _ := kind.Text(); value == "item_reference" {
				return errors.New("item-reference trajectories require a per-item resource authority contract")
			}
		}
		sampling, _ := data.Lookup("sampling_params")
		if tools, present := sampling.Lookup("tools"); present && tools.Kind() != oif.Null {
			if tools.Kind() != oif.Array {
				return resources.ErrContract
			}
			for _, tool := range tools.Elements() {
				kind, _ := tool.Lookup("type")
				name, _ := kind.Text()
				if name != "function" && name != "custom" {
					return errors.New("evaluation hosted tools require resource authority and settlement contracts")
				}
			}
		}
		if choice, present := sampling.Lookup("tool_choice"); present && choice.Kind() == oif.Object {
			kind, _ := choice.Lookup("type")
			name, _ := kind.Text()
			if name != "function" && name != "custom" {
				return errors.New("evaluation hosted tool choices are unsupported")
			}
		}
	}
	source, _ := data.Lookup("source")
	sourceKind, _ := source.Lookup("type")
	text, _ = sourceKind.Text()
	if text != "file_id" && text != "file_content" {
		return errors.New("evaluation data must use an owned file or inline file content")
	}
	if text == "file_id" {
		id, _ := source.Lookup("id")
		if raw, ok := id.Text(); !ok || raw == "" {
			return resources.ErrContract
		}
	} else {
		content, present := source.Lookup("content")
		if !present || content.Kind() != oif.Array {
			return resources.ErrContract
		}
	}
	return nil
}

// A correlation marker is sent only by native APIs with a documented metadata
// dictionary. Its value is a gateway-generated identity, never a caller hint.
func retainedCorrelationBody(e retainedapi.Endpoint, r *resources.Resource, body []byte) ([]byte, error) {
	if r != nil && e.Kind == "vertex_tuning_job" && e.Action == "create" {
		source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: kernel.RetainedBodyLimit})
		if err != nil {
			return nil, err
		}
		labels, present := source.Root().Lookup("labels")
		change := oif.Change{Pointer: "/labels/" + resources.RetainedSubmissionMarkerField, Value: strconv.Quote(r.UUID.String()), Origin: oif.ResourceBinding, Reason: "durable Vertex tuning correlation"}
		if !present || labels.Kind() == oif.Null {
			change.Pointer = "/labels"
			change.Value = `{` + strconv.Quote(resources.RetainedSubmissionMarkerField) + `:` + strconv.Quote(r.UUID.String()) + `}`
		} else if labels.Kind() != oif.Object {
			return nil, resources.ErrContract
		}
		result, err := oif.Apply(source, []oif.Change{change})
		if err != nil {
			return nil, err
		}
		return result.Bytes(), nil
	}

	if r == nil || e.Action != "create" && e.Action != "update" || !slices.Contains([]string{"fine_tuning_job", "vector_store", "eval", "eval_run"}, e.Kind) {
		return body, nil
	}
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: kernel.RetainedBodyLimit})
	if err != nil {
		return nil, err
	}
	marker := oif.Change{Pointer: "/metadata/" + resources.RetainedSubmissionMarkerField, Value: strconv.Quote(r.UUID.String()), Origin: oif.ResourceBinding, Reason: "durable retained submission correlation"}
	metadata, present := source.Root().Lookup("metadata")
	if e.Action == "update" && !present {
		return body, nil
	}
	if !present || metadata.Kind() == oif.Null {
		marker.Pointer = "/metadata"
		marker.Value = `{` + strconv.Quote(resources.RetainedSubmissionMarkerField) + `:` + strconv.Quote(r.UUID.String()) + `}`
	} else if metadata.Kind() != oif.Object {
		return nil, resources.ErrContract
	}
	bound, err := oif.Apply(source, []oif.Change{marker})
	if err != nil {
		return nil, err
	}
	return bound.Bytes(), nil
}
