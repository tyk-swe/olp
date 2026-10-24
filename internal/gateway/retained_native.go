package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

func retainedGeminiFileID(raw string) (string, error) {
	if validGeminiFileID(raw) {
		return raw, nil
	}
	local := strings.TrimPrefix(raw, "files/")
	if local != raw && validGeminiFileID(local) {
		return local, nil
	}
	uri, err := url.Parse(raw)
	if err != nil || uri.User != nil || uri.RawQuery != "" || uri.Fragment != "" {
		return "", resources.ErrNotFound
	}
	local = strings.TrimPrefix(uri.Path, "/gemini/v1beta/files/")
	if local == uri.Path || !validGeminiFileID(local) {
		return "", resources.ErrNotFound
	}
	return local, nil
}

func retainedGeminiFileURI(r *resources.Resource, p *pin) (string, error) {
	var metadata struct {
		URI  string `json:"uri"`
		Name string `json:"name"`
	}
	if json.Unmarshal(r.Metadata, &metadata) != nil || metadata.Name != "files/"+r.UpstreamID {
		return "", resources.ErrContract
	}
	uri, err := url.Parse(metadata.URI)
	endpoint, endpointErr := url.Parse(p.provider.Endpoint)
	if err != nil || endpointErr != nil || uri.User != nil || uri.RawQuery != "" || uri.Fragment != "" || uri.Host != endpoint.Host || uri.Scheme != endpoint.Scheme || !strings.HasSuffix(uri.Path, "/files/"+r.UpstreamID) {
		return "", resources.ErrContract
	}
	return metadata.URI, nil
}

// retainedInputMetadata validates controls which can spend under the pinned
// credential. Only the resulting model identities and billing dimensions are
// retained; native source content and grader definitions remain provider-side.
func retainedInputMetadata(p *pin, route *routing.Route, owner string, e retainedapi.Endpoint, source oif.Document, parent *resources.Resource) (map[string]any, error) {
	meta := map[string]any{"upstream_model": p.model, "contract_revision": retainedapi.Revision}
	if e.Kind == "container_file" && e.Action == "create" {
		var pins struct {
			Tier string `json:"memory_tier"`
		}
		if parent == nil || json.Unmarshal(parent.Metadata, &pins) != nil || pins.Tier == "" {
			return nil, resources.ErrContract
		}
		meta["memory_tier"] = pins.Tier
	}
	if metadata, present := source.Root().Lookup("metadata"); present && metadata.Kind() != oif.Null {
		if metadata.Kind() != oif.Object {
			return nil, errors.New("metadata must be an object")
		}
		for _, field := range []string{"olp_submission_id", "olp_submission_digest"} {
			if _, reserved := metadata.Lookup(field); reserved {
				return nil, errors.New("gateway submission metadata is reserved for resource correlation")
			}
		}
	}
	if e.Kind == "fine_tuning_job" && e.Action == "create" {
		method, present := source.Root().Lookup("method")
		if present && method.Kind() != oif.Null {
			kind, _ := method.Lookup("type")
			text, _ := kind.Text()
			if text != "supervised" && text != "dpo" {
				return nil, errors.New("this tuning method has no supported training-token settlement contract")
			}
		}
	}
	if e.Kind == "vertex_tuning_job" && e.Action == "create" {
		if labels, present := source.Root().Lookup("labels"); present && labels.Kind() != oif.Null {
			if labels.Kind() != oif.Object {
				return nil, resources.ErrContract
			}
			for _, name := range []string{"olp_submission_id", "olp_submission_digest"} {
				if _, reserved := labels.Lookup(name); reserved {
					return nil, errors.New("gateway submission labels are reserved")
				}
			}
		}
		data, _ := source.Root().Lookup("supervisedTuningSpec")
		training, _ := data.Lookup("trainingDatasetUri")
		uri, ok := training.Text()
		if !ok || !p.provider.Connector().AllowsTuningDataset(uri) {
			return nil, errors.New("training dataset is outside the connection's configured URI prefixes")
		}
		if validation, present := data.Lookup("validationDatasetUri"); present && validation.Kind() != oif.Null {
			uri, ok := validation.Text()
			if !ok || !p.provider.Connector().AllowsTuningDataset(uri) {
				return nil, errors.New("validation dataset is outside the connection's configured URI prefixes")
			}
		}
		// Tuning methods which select additional remote datasets or remote
		// evaluation sources need their own owner-bound source contracts.
		for _, field := range source.Root().Members() {
			if slices.Contains([]string{"distillationSpec", "preferenceOptimizationSpec", "evaluationConfig"}, field.Name) && field.Value.Kind() != oif.Null {
				return nil, errors.New("this tuning source cannot be mapped to configured dataset authority")
			}
		}
	}
	if e.Kind == "context_cache" && e.Action == "patch" {
		if len(source.Root().Members()) == 0 {
			return nil, errors.New("cache expiration update must not be empty")
		}
		for _, field := range source.Root().Members() {
			if field.Name != "ttl" && field.Name != "expireTime" && field.Name != "expire_time" {
				return nil, errors.New("only cache expiration can be updated")
			}
		}
	}
	if e.Operation == "eval" {
		if e.Kind == "eval" && e.Action == "update" {
			for _, field := range source.Root().Members() {
				if field.Name != "name" && field.Name != "metadata" {
					return nil, errors.New("only evaluation name and metadata can be updated")
				}
			}
		}
		models := []string{}
		if e.Kind == "eval_run" && e.Action == "create" || e.Kind == "eval" && e.Action == "update" {
			if parent == nil || parent.Kind != "eval" {
				return nil, resources.ErrContract
			}
			var prior struct {
				Models []string `json:"authorized_models"`
			}
			if json.Unmarshal(parent.Metadata, &prior) != nil {
				return nil, resources.ErrContract
			}
			if _, replace := source.Root().Lookup("testing_criteria"); !replace {
				models = append(models, prior.Models...)
			}
		}
		if err := validateEvalSource(source.Root(), e); err != nil {
			return nil, err
		}
		var walk func(oif.Value, string) error
		walk = func(value oif.Value, path string) error {
			switch value.Kind() {
			case oif.Array:
				for i, item := range value.Elements() {
					if err := walk(item, path+"/"+strconv.Itoa(i)); err != nil {
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
				for _, field := range value.Members() {
					if slices.Contains([]string{"metadata", "schema", "json_schema", "item_schema", "parameters"}, field.Name) || path == "/data_source/source" && field.Name == "content" {
						continue
					}
					if field.Name == "model" && field.Value.Kind() != oif.Null {
						model, ok := field.Value.Text()
						if !ok || !retainedEvalModelAllowed(p, route, owner, model) {
							return errors.New("evaluation model is not authorized on this connection and route")
						}
						if !slices.Contains(models, model) {
							models = append(models, model)
						}
					}
					if err := walk(field.Value, path+"/"+field.Name); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := walk(source.Root(), ""); err != nil {
			return nil, err
		}
		for _, model := range models {
			if !retainedEvalModelAllowed(p, route, owner, model) {
				return nil, resources.ErrContract
			}
		}
		slices.Sort(models)
		meta["authorized_models"] = models
	}
	if e.Kind == "container" && e.Action == "create" {
		if _, present := source.Root().Lookup("metadata"); present {
			return nil, errors.New("the pinned container API does not accept metadata")
		}
		tier := "1g"
		if value, present := source.Root().Lookup("memory_limit"); present {
			var ok bool
			tier, ok = value.Text()
			if !ok || !slices.Contains([]string{"1g", "4g", "16g", "64g"}, tier) {
				return nil, errors.New("unsupported container memory tier")
			}
		}
		meta["memory_tier"] = tier
	}
	return meta, nil
}

func retainedEvalModelAllowed(p *pin, route *routing.Route, owner, model string) bool {
	if model == "" || route == nil || !slices.Contains(route.Operations, "generation") || !p.slot.Allows(model, route.Slug, owner) || !p.provider.Supports(model, "eval", "openai", "unary") || !p.provider.Supports(model, "generation", "openai", "unary") {
		return false
	}
	return slices.ContainsFunc(route.Targets, func(target routing.Target) bool {
		return target.ProviderID == p.provider.ID && target.ProviderModel == model
	})
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
		source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: retainedBodyLimit})
		if err != nil {
			return nil, err
		}
		labels, present := source.Root().Lookup("labels")
		change := oif.Change{Pointer: "/labels/olp_submission_id", Value: strconv.Quote(r.UUID.String()), Origin: oif.ResourceBinding, Reason: "durable Vertex tuning correlation"}
		if !present || labels.Kind() == oif.Null {
			change.Pointer = "/labels"
			change.Value = `{"olp_submission_id":` + strconv.Quote(r.UUID.String()) + `}`
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
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: retainedBodyLimit})
	if err != nil {
		return nil, err
	}
	marker := oif.Change{Pointer: "/metadata/olp_submission_id", Value: strconv.Quote(r.UUID.String()), Origin: oif.ResourceBinding, Reason: "durable retained submission correlation"}
	metadata, present := source.Root().Lookup("metadata")
	if e.Action == "update" && !present {
		return body, nil
	}
	if !present || metadata.Kind() == oif.Null {
		marker.Pointer = "/metadata"
		marker.Value = `{"olp_submission_id":` + strconv.Quote(r.UUID.String()) + `}`
	} else if metadata.Kind() != oif.Object {
		return nil, resources.ErrContract
	}
	bound, err := oif.Apply(source, []oif.Change{marker})
	if err != nil {
		return nil, err
	}
	return bound.Bytes(), nil
}

func retainedObservation(resource *resources.Resource, e retainedapi.Endpoint, parsed oif.Document, now time.Time) (resources.RetainedObservation, error) {
	return resources.ObserveRetained(resource, e, parsed, now)
}

func (s *Server) retainedNativeResult(ctx context.Context, r *resources.Resource, e retainedapi.Endpoint, source oif.Document, persist bool) error {
	if r == nil || e.Action == "list" || e.Action == "delete" {
		return nil
	}
	if err := resources.EnrichRetained(r, e, source, s.now()); err != nil {
		return err
	}
	if persist {
		if err := s.Resources.SaveRetainedResult(ctx, r); err != nil {
			return err
		}
	}
	if e.Operation == "fine_tuning" {
		return s.Resources.DiscoverRetainedModels(ctx, r, source)
	}
	return nil
}
