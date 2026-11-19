package anthropicstate

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"

	"github.com/tyk-swe/olp/internal/gateway/kernel"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
)

var customID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func commonModel(body []byte) (string, bool) {
	var object struct {
		Requests []struct {
			Params struct {
				Model string `json:"model"`
			} `json:"params"`
		} `json:"requests"`
	}
	if json.Unmarshal(body, &object) != nil || len(object.Requests) == 0 {
		return "", false
	}
	model := object.Requests[0].Params.Model
	for _, request := range object.Requests[1:] {
		if request.Params.Model != model {
			return "", false
		}
	}
	return model, model != ""
}

func batchFileReferences(body []byte) ([]string, error) {
	var request struct {
		Requests []batchEntry `json:"requests"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	refs := []string{}
	for _, item := range request.Requests {
		itemRefs, err := fileReferences(item.Params)
		if err != nil {
			return nil, err
		}
		refs = append(refs, itemRefs...)
	}
	return uniqueStrings(refs), nil
}

type batchEntry struct {
	CustomID string          `json:"custom_id"`
	Params   json.RawMessage `json:"params"`
}

func validateBatchRequest(body []byte, routeSlug, targetModel, boundModel string) ([]byte, []string, []string, *fault.Error) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil || source.Root().Kind() != oif.Object || len(source.Root().Members()) != 1 || source.Root().Members()[0].Name != "requests" {
		return nil, nil, nil, fault.InvalidRequest("invalid_request", "The request body must contain only requests.", nil)
	}
	requestsValue, present := source.Root().Lookup("requests")
	elements := requestsValue.Elements()
	if !present || requestsValue.Kind() != oif.Array || len(elements) < 1 || len(elements) > 100000 {
		return nil, nil, nil, fault.InvalidRequest("invalid_request", "requests must contain between 1 and 100000 entries.", nil)
	}
	var requests []batchEntry
	if json.Unmarshal(body, &struct {
		Requests *[]batchEntry `json:"requests"`
	}{Requests: &requests}) != nil || len(requests) != len(elements) {
		return nil, nil, nil, fault.InvalidRequest("invalid_request", "Every request must contain custom_id and params.", nil)
	}
	seen := make(map[string]struct{}, len(requests))
	fileIDs := []string{}
	model := routeSlug
	for i, request := range requests {
		members := elements[i].Members()
		if elements[i].Kind() != oif.Object || len(members) != 2 {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d] must contain only custom_id and params.", i), nil)
		}
		memberNames := map[string]bool{}
		for _, member := range members {
			memberNames[member.Name] = true
		}
		if !memberNames["custom_id"] || !memberNames["params"] {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d] must contain custom_id and params.", i), nil)
		}
		if !customID.MatchString(request.CustomID) {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].custom_id is invalid.", i), nil)
		}
		if _, exists := seen[request.CustomID]; exists {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].custom_id is duplicated.", i), nil)
		}
		seen[request.CustomID] = struct{}{}
		params, parseErr := oif.ParseJSON(request.Params, oif.Limits{MaxBytes: len(request.Params)})
		if parseErr != nil || params.Root().Kind() != oif.Object {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params must be an object.", i), nil)
		}
		if stream, ok := params.Root().Lookup("stream"); ok && stream.Raw() == "true" {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params.stream is unsupported.", i), nil)
		}
		modelValue, hasModel := params.Root().Lookup("model")
		requestModel, _ := modelValue.Text()
		parseModel := routeSlug
		if parseModel == "" {
			parseModel = requestModel
		}
		if parsed, parseErr := protocols.Parse(wire.Anthropic, request.Params, parseModel); parseErr != nil || parsed == nil {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params is invalid.", i), nil)
		}
		if routeSlug == "" {
			if !hasModel || requestModel == "" || i > 0 && model != requestModel {
				return nil, nil, nil, fault.InvalidRequest("invalid_request", "A single common params.model or X-OLP-Route is required.", nil)
			}
			model = requestModel
		}
		if requestModel == "" || targetModel != "" && requestModel != routeSlug && requestModel != targetModel {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params.model does not match the route.", i), nil)
		}
		refs, refErr := fileReferences(request.Params)
		if refErr != nil {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params contains an invalid file source.", i), nil)
		}
		fileIDs = append(fileIDs, refs...)
	}
	bound, err := bindBatchModel(requests, cmp.Or(boundModel, targetModel))
	if err != nil {
		return nil, nil, nil, fault.InvalidRequest("invalid_request", "Every request params must be an object.", nil)
	}
	return kernel.MustJSON(struct {
		Requests []batchEntry `json:"requests"`
	}{Requests: bound}), uniqueStrings(fileIDs), requestsCustomIDs(requests), nil
}

func bindBatchModel(requests []batchEntry, model string) ([]batchEntry, error) {
	bound := make([]batchEntry, len(requests))
	for i, request := range requests {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, err
		}
		if model != "" {
			params["model"] = kernel.MustJSON(model)
		}
		bound[i] = batchEntry{CustomID: request.CustomID, Params: kernel.MustJSON(params)}
	}
	return bound, nil
}

func requestsCustomIDs(requests []batchEntry) []string {
	out := make([]string, len(requests))
	for i := range requests {
		out[i] = requests[i].CustomID
	}
	return out
}

type batchView struct {
	body       []byte
	upstreamID string
	state      string
	metadata   json.RawMessage
}

var errMalformedBatch = errors.New("malformed Anthropic batch")

func projectBatch(body []byte, localID, origin, model string) (batchView, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return batchView{}, errMalformedBatch
	}
	var fields struct {
		ID               string  `json:"id"`
		ProcessingStatus string  `json:"processing_status"`
		ResultsURL       *string `json:"results_url"`
	}
	if json.Unmarshal(body, &fields) != nil || fields.ID == "" || len(fields.ID) > 512 {
		return batchView{}, errMalformedBatch
	}
	if fields.ResultsURL != nil {
		if _, err := url.Parse(*fields.ResultsURL); err != nil {
			return batchView{}, errMalformedBatch
		}
	}
	projected := maps.Clone(object)
	projected["id"] = kernel.MustJSON(localID)
	if fields.ResultsURL != nil {
		projected["results_url"] = kernel.MustJSON(origin + "/anthropic/v1/messages/batches/" + url.PathEscape(localID) + "/results")
	}
	stored := maps.Clone(projected)
	delete(stored, "id")
	stored["upstream_model"] = kernel.MustJSON(model)
	return batchView{body: kernel.MustJSON(projected), upstreamID: fields.ID, state: fields.ProcessingStatus, metadata: kernel.MustJSON(stored)}, nil
}
