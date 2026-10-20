package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

var errGeminiBatchFidelity = errors.New("Gemini batch contains an unprojected provider identifier")

type geminiBatchInput struct {
	Canonical    []byte
	Upstream     []byte
	Expected     *int64
	Keys         []string
	LocalFileID  string
	UpstreamFile string
	DisplayName  *string
}

func validateGeminiBatchRequest(body []byte, slug string) (geminiBatchInput, *Error) {
	doc, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil || doc.Root().Kind() != oif.Object || !geminiOnlyFields(doc.Root(), "batch") {
		return geminiBatchInput{}, invalidRequest("invalid_request", "The request must contain only a batch object.", nil)
	}
	batch, present := doc.Root().Lookup("batch")
	if !present || batch.Kind() != oif.Object {
		return geminiBatchInput{}, invalidRequest("invalid_request", "batch must be an object.", nil)
	}
	for _, member := range batch.Members() {
		if member.Name != "display_name" && member.Name != "displayName" && member.Name != "input_config" && member.Name != "inputConfig" {
			return geminiBatchInput{}, invalidRequest("invalid_request", "The batch contains an unsupported field.", nil)
		}
	}
	displayName, displayPresent, err := geminiBatchAlias(batch, "display_name", "displayName")
	if err != nil {
		return geminiBatchInput{}, invalidRequest("invalid_request", "The batch display name is invalid.", nil)
	}
	if displayPresent {
		var value string
		if json.Unmarshal(displayName, &value) != nil || len(value) > 512 {
			return geminiBatchInput{}, invalidRequest("invalid_request", "displayName must be a string no longer than 512 bytes.", nil)
		}
	}
	input, inputPresent, err := geminiBatchAlias(batch, "input_config", "inputConfig")
	if err != nil || !inputPresent {
		return geminiBatchInput{}, invalidRequest("invalid_request", "The batch must contain exactly one input_config.", nil)
	}
	inputDoc, err := oif.ParseJSON(input, oif.Limits{})
	if err != nil || inputDoc.Root().Kind() != oif.Object {
		return geminiBatchInput{}, invalidRequest("invalid_request", "input_config must be an object.", nil)
	}
	inputMembers := inputDoc.Root().Members()
	if len(inputMembers) != 1 || inputMembers[0].Name != "requests" &&
		inputMembers[0].Name != "file_name" && inputMembers[0].Name != "fileName" {
		return geminiBatchInput{}, invalidRequest("invalid_request", "input_config must contain requests or one file name.", nil)
	}
	inputResult := geminiBatchInput{DisplayName: nil}
	if displayPresent {
		var value string
		_ = json.Unmarshal(displayName, &value)
		inputResult.DisplayName = &value
	}
	if inputMembers[0].Name == "file_name" || inputMembers[0].Name == "fileName" {
		var name string
		if json.Unmarshal([]byte(inputMembers[0].Value.Raw()), &name) != nil {
			return geminiBatchInput{}, invalidRequest("invalid_request", "The input file name is invalid.", nil)
		}
		localID, ok := strings.CutPrefix(name, "files/")
		if !ok || !validGeminiFileID(localID) {
			return geminiBatchInput{}, invalidRequest("invalid_request", "The input file name must identify a local Gemini file.", nil)
		}
		inputResult.LocalFileID = localID
		inputResult.UpstreamFile = name
	}
	if inputMembers[0].Name == "requests" {
		requestsObject := inputMembers[0].Value
		if requestsObject.Kind() != oif.Object || len(requestsObject.Members()) != 1 || requestsObject.Members()[0].Name != "requests" {
			return geminiBatchInput{}, invalidRequest("invalid_request", "input_config.requests must contain only requests.", nil)
		}
		list := requestsObject.Members()[0].Value
		if list.Kind() != oif.Array || len(list.Elements()) < 1 || len(list.Elements()) > 100000 {
			return geminiBatchInput{}, invalidRequest("invalid_request", "A Gemini batch must contain between 1 and 100000 requests.", nil)
		}
		seen := map[string]struct{}{}
		inputItems := make([]json.RawMessage, 0, len(list.Elements()))
		keys := make([]string, 0, len(list.Elements()))
		for _, item := range list.Elements() {
			if item.Kind() != oif.Object || !geminiOnlyFields(item, "request", "metadata") {
				return geminiBatchInput{}, invalidRequest("invalid_request", "Each inline batch item must contain only request and metadata.", nil)
			}
			request, requestPresent := item.Lookup("request")
			metadata, metadataPresent := item.Lookup("metadata")
			if !requestPresent || !metadataPresent || metadata.Kind() != oif.Object || !geminiOnlyFields(metadata, "key") {
				return geminiBatchInput{}, invalidRequest("invalid_request", "Each inline batch item requires request and metadata.key.", nil)
			}
			keyValue, keyPresent := metadata.Lookup("key")
			var key string
			if !keyPresent || json.Unmarshal([]byte(keyValue.Raw()), &key) != nil || key == "" || len(key) > 128 {
				return geminiBatchInput{}, invalidRequest("invalid_request", "metadata.key must be a non-empty string of at most 128 bytes.", nil)
			}
			if _, exists := seen[key]; exists {
				return geminiBatchInput{}, invalidRequest("invalid_request", "metadata.key values must be unique.", nil)
			}
			seen[key] = struct{}{}
			if _, err := protocols.Parse(wire.Gemini, []byte(request.Raw()), slug); err != nil {
				return geminiBatchInput{}, invalidRequest("invalid_request", "A batch request is not a valid Gemini GenerateContent request.", nil)
			}
			keys = append(keys, key)
			inputItems = append(inputItems, json.RawMessage(item.Raw()))
		}
		count := int64(len(inputItems))
		inputResult.Expected = &count
		inputResult.Keys = keys
	}
	canonical, err := canonicalGeminiJSON(body)
	if err != nil {
		return geminiBatchInput{}, invalidRequest("invalid_request", "The batch request is invalid JSON.", nil)
	}
	upstreamBatch := map[string]any{}
	if inputResult.DisplayName != nil {
		upstreamBatch["displayName"] = *inputResult.DisplayName
	}
	upstreamInput := map[string]any{}
	if inputMembers[0].Name == "requests" {
		upstreamInput["requests"] = json.RawMessage(inputMembers[0].Value.Raw())
	} else {
		upstreamInput["fileName"] = inputResult.UpstreamFile
	}
	upstreamBatch["inputConfig"] = upstreamInput
	upstream, err := json.Marshal(map[string]any{"batch": upstreamBatch})
	if err != nil {
		return geminiBatchInput{}, invalidRequest("invalid_request", "The batch request could not be translated.", nil)
	}
	inputResult.Canonical, inputResult.Upstream = canonical, upstream
	return inputResult, nil
}

func geminiOnlyFields(value oif.Value, allowed ...string) bool {
	for _, member := range value.Members() {
		if !slices.Contains(allowed, member.Name) {
			return false
		}
	}
	return true
}

func geminiBatchAlias(object oif.Value, snake, camel string) (json.RawMessage, bool, error) {
	left, leftOK := object.Lookup(snake)
	right, rightOK := object.Lookup(camel)
	if leftOK && rightOK {
		return nil, false, errors.New("duplicate alias")
	}
	if leftOK {
		return json.RawMessage(left.Raw()), true, nil
	}
	if rightOK {
		return json.RawMessage(right.Raw()), true, nil
	}
	return nil, false, nil
}

func canonicalGeminiJSON(body []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return json.Marshal(value)
}

func geminiBatchName(body []byte) (string, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return "", errors.New("malformed Gemini batch")
	}
	var name string
	if json.Unmarshal(object["name"], &name) != nil {
		return "", errors.New("Gemini batch name is missing")
	}
	id, ok := strings.CutPrefix(name, "batches/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", errors.New("Gemini batch name is invalid")
	}
	return id, nil
}

func geminiBatchState(body []byte) string {
	var object struct {
		State    string `json:"state"`
		Metadata struct {
			State string `json:"state"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(body, &object)
	if object.Metadata.State != "" {
		return object.Metadata.State
	}
	return object.State
}

func geminiBatchInputName(body []byte) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return ""
	}
	var batch map[string]json.RawMessage
	_ = json.Unmarshal(object["batch"], &batch)
	var input map[string]json.RawMessage
	var name string
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(object["metadata"], &metadata)
	for _, parent := range []map[string]json.RawMessage{batch, metadata, object} {
		for _, raw := range []json.RawMessage{parent["inputConfig"], parent["input_config"]} {
			input = nil
			if json.Unmarshal(raw, &input) != nil || input == nil {
				continue
			}
			for _, field := range []string{"fileName", "file_name"} {
				if json.Unmarshal(input[field], &name) == nil && name != "" {
					return name
				}
			}
		}
	}
	return ""
}

func geminiBatchResponseFiles(body []byte) ([]string, error) {
	var root any
	if json.Unmarshal(body, &root) != nil {
		return nil, errGeminiBatchFidelity
	}
	var out []string
	var visit func(any, []string) error
	allowed := map[string]bool{
		"name": true, "metadata.name": true,
		"inputConfig.fileName": true, "inputConfig.file_name": true,
		"input_config.fileName": true, "input_config.file_name": true,
		"metadata.inputConfig.fileName": true, "metadata.inputConfig.file_name": true,
		"metadata.input_config.fileName": true, "metadata.input_config.file_name": true,
		"output.responsesFile": true, "metadata.output.responsesFile": true,
		"response.responsesFile": true, "metadata.response.responsesFile": true,
	}
	visit = func(node any, path []string) error {
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				if err := visit(child, append(append([]string(nil), path...), key)); err != nil {
					return err
				}
			}
		case []any:
			for index, child := range value {
				if err := visit(child, append(append([]string(nil), path...), strconv.Itoa(index))); err != nil {
					return err
				}
			}
		case string:
			joined := strings.Join(path, ".")
			if strings.Contains(value, "batches/") && !allowed[joined] {
				return errGeminiBatchFidelity
			}
			if strings.Contains(value, "files/") {
				if !allowed[joined] {
					return errGeminiBatchFidelity
				}
				if strings.Contains(joined, "responsesFile") {
					out = append(out, value)
				}
			}
		}
		return nil
	}
	if err := visit(root, nil); err != nil {
		return nil, err
	}
	return slices.Compact(out), nil
}

func projectGeminiBatchObject(body []byte, localID, routeSlug, localInputFile, upstreamInputFile string, outputFiles map[string]string) ([]byte, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return nil, errGeminiBatchFidelity
	}
	upstreamID, err := geminiBatchName(body)
	if err != nil {
		return nil, errGeminiBatchFidelity
	}
	delete(object, "upstream_model")
	delete(object, "local_input_file_name")
	delete(object, "upstream_input_file_name")
	cleanBody, _ := json.Marshal(object)
	foundInput := geminiBatchInputName(cleanBody)
	if foundInput != "" && foundInput != upstreamInputFile || foundInput != "" && localInputFile == "" {
		return nil, errGeminiBatchFidelity
	}
	var metadataIdentity map[string]json.RawMessage
	if json.Unmarshal(object["metadata"], &metadataIdentity) == nil && metadataIdentity != nil && len(metadataIdentity["name"]) > 0 {
		var metadataName string
		if json.Unmarshal(metadataIdentity["name"], &metadataName) != nil || metadataName != "batches/"+upstreamID {
			return nil, errGeminiBatchFidelity
		}
	}
	files, err := geminiBatchResponseFiles(cleanBody)
	if err != nil {
		return nil, err
	}
	for _, upstream := range files {
		if outputFiles[upstream] == "" {
			return nil, errGeminiBatchFidelity
		}
	}
	localName, _ := json.Marshal("batches/" + localID)
	object["name"] = localName
	var metadata map[string]json.RawMessage
	if raw := object["metadata"]; len(raw) > 0 {
		if json.Unmarshal(raw, &metadata) != nil || metadata == nil {
			return nil, errGeminiBatchFidelity
		}
		if _, ok := metadata["name"]; ok {
			metadata["name"] = localName
		}
		if _, ok := metadata["model"]; ok {
			metadata["model"], _ = json.Marshal("models/" + routeSlug)
		}
		if err := projectGeminiBatchFiles(metadata, localInputFile, upstreamInputFile, outputFiles); err != nil {
			return nil, err
		}
		object["metadata"], _ = json.Marshal(metadata)
	}
	if _, ok := object["model"]; ok {
		object["model"], _ = json.Marshal("models/" + routeSlug)
	}
	if err := projectGeminiBatchFiles(object, localInputFile, upstreamInputFile, outputFiles); err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

func projectGeminiBatchFiles(object map[string]json.RawMessage, localInputFile, upstreamInputFile string, outputFiles map[string]string) error {
	for _, field := range []string{"inputConfig", "input_config"} {
		if raw, ok := object[field]; ok {
			var config map[string]json.RawMessage
			if json.Unmarshal(raw, &config) != nil || config == nil {
				return errGeminiBatchFidelity
			}
			for _, nameField := range []string{"fileName", "file_name"} {
				if rawName, present := config[nameField]; present {
					var upstreamName string
					if json.Unmarshal(rawName, &upstreamName) != nil || localInputFile == "" || upstreamName != upstreamInputFile {
						return errGeminiBatchFidelity
					}
					config[nameField], _ = json.Marshal(localInputFile)
				}
			}
			object[field], _ = json.Marshal(config)
		}
	}
	for _, outputField := range []string{"output", "response"} {
		if raw, ok := object[outputField]; ok {
			var destination map[string]json.RawMessage
			if json.Unmarshal(raw, &destination) != nil || destination == nil {
				return errGeminiBatchFidelity
			}
			if file, present := destination["responsesFile"]; present {
				var upstream string
				if json.Unmarshal(file, &upstream) != nil || outputFiles[upstream] == "" {
					return errGeminiBatchFidelity
				}
				destination["responsesFile"], _ = json.Marshal(outputFiles[upstream])
			}
			object[outputField], _ = json.Marshal(destination)
		}
	}
	return nil
}

func geminiBatchMetadata(body []byte, model, localInputFile, upstreamInputFile string, old json.RawMessage) json.RawMessage {
	if localInputFile == "" && len(old) > 0 {
		localInputFile, upstreamInputFile = geminiBatchInternalNames(&resources.Resource{Metadata: old})
	}
	return batches.GeminiMetadata(body, model, localInputFile, upstreamInputFile)
}

func geminiBatchInternalNames(resource *resources.Resource) (local, upstream string) {
	var metadata struct {
		Local    string `json:"local_input_file_name"`
		Upstream string `json:"upstream_input_file_name"`
	}
	_ = json.Unmarshal(resource.Metadata, &metadata)
	return metadata.Local, metadata.Upstream
}

func (s *Server) geminiCreateBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.geminiBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	slug := r.PathValue("model")
	if header := strings.TrimSpace(r.Header.Get(stateRouteHeader)); header != "" && header != slug {
		s.stateFail(x, w, invalidRequest("invalid_request", "The Gemini batch model path must match X-OLP-Route.", nil), x.family)
		return
	}
	route, err := s.geminiRoute(x, authority, r, slug)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	input, err := validateGeminiBatchRequest(body, route.Slug)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	p, pinnedRoute, file, err := s.geminiBatchPin(r.Context(), x, authority, route, input)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	if pinnedRoute != nil {
		route = pinnedRoute
	}
	x.route = route
	if file != nil {
		input.UpstreamFile = "files/" + file.UpstreamID
		rewritten, rewriteErr := rewriteGeminiBatchInputFile(input.Upstream, input.UpstreamFile)
		if rewriteErr != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("invalid_request", "The input file name could not be bound.", nil), x.family)
			return
		}
		input.Upstream = rewritten
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	source, sourceErr := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if sourceErr != nil {
		s.stateFail(x, w, invalidRequest("invalid_request", "The Gemini batch request is invalid.", nil), x.family)
		return
	}
	cursor := json.RawMessage(`{"submission":{"gemini_inline":true}}`)
	localInputFile := ""
	upstreamFile := ""
	if file != nil {
		localInputFile = "files/" + file.ID
		upstreamFile = "files/" + file.UpstreamID
		cursor = mustJSON(map[string]any{"submission": map[string]string{"gemini_file": upstreamFile, "local_input_file_id": localInputFile}})
	}
	endpoint, endpointErr := resourceURL(p.provider.Connector(), p.model, "models/"+url.PathEscape(p.model)+":batchGenerateContent", nil)
	if endpointErr != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved."), x.family)
		return
	}
	submission, err := s.prepareBatchSubmission(ctx, x, authority.ID, p, source, input.Canonical, input.Upstream,
		oif.ServingIdentity{}, localInputFile, upstreamFile, resources.KindGeminiBatch,
		"gemini", "gemini", "generation", 48*time.Hour, input.Expected, cursor)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	resp, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, input.Upstream, "application/json")
	if failure != nil {
		x.dispatched = failure.dispatched
		if failure.status == 0 && failure.dispatched {
			commitCtx, stop := resourceCommitContext(r.Context())
			defer stop()
			if markErr := s.Batches.MarkUnknown(commitCtx, submission.resource.UUID, "submission_unknown", s.now()); markErr != nil {
				s.log.Warn("Gemini batch submission outcome could not be recorded", "resource_id", submission.resource.UUID, "error", markErr)
			}
			s.stateFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The provider may have accepted this batch, but its outcome is unknown."), x.family)
			return
		}
		commitCtx, stop := resourceCommitContext(r.Context())
		defer stop()
		if deleteErr := s.Batches.DeleteSubmission(commitCtx, submission.resource.UUID); deleteErr != nil {
			s.log.Warn("Gemini batch submission intent could not be removed", "resource_id", submission.resource.UUID, "error", deleteErr)
		}
		s.stateFail(x, w, upstreamError(failure), x.family)
		return
	}
	defer resp.Body.Close()
	x.dispatched = true
	result, readErr := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if readErr != nil {
		s.stateFail(x, w, s.batchSubmissionUnknown(r.Context(), submission.resource.UUID, "The provider response could not be read. The batch may have been accepted."), x.family)
		return
	}
	upstreamID, nameErr := geminiBatchName(result)
	if nameErr != nil {
		s.stateFail(x, w, s.batchSubmissionUnknown(r.Context(), submission.resource.UUID, "The provider returned an invalid batch name. The batch may have been accepted."), x.family)
		return
	}
	resource := submission.resource
	resource.UpstreamID = upstreamID
	resource.State = geminiBatchState(result)
	resource.Metadata = geminiBatchMetadata(result, p.model, localInputFile, upstreamFile, nil)
	commitCtx, stop := resourceCommitContext(ctx)
	defer stop()
	tx, txErr := s.Resources.Begin(commitCtx)
	if txErr == nil {
		defer tx.Rollback(commitCtx)
		var stored *resources.Resource
		stored, txErr = s.Resources.PutTx(commitCtx, tx, resource)
		if txErr == nil {
			resource = stored
			txErr = s.Batches.ActivateTx(commitCtx, tx, resource.UUID, upstreamID, input.Expected)
		}
		if txErr == nil {
			txErr = tx.Commit(commitCtx)
		}
	}
	if txErr != nil {
		if markErr := s.Batches.MarkUnknown(commitCtx, resource.UUID, "submission_commit_unavailable", s.now()); markErr != nil {
			s.log.Warn("accepted Gemini batch intent could not be marked unknown", "resource_id", resource.UUID, "error", markErr)
		}
		s.stateFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The accepted Gemini batch could not be stored."), x.family)
		return
	}
	projected, projectErr := s.geminiBatchResourceObject(commitCtx, resource, result)
	if projectErr != nil {
		s.stateFail(x, w, geminiBatchProjectionError(projectErr), x.family)
		return
	}
	x.completePinned()
	s.writeStateJSON(w, x, projected)
}

func rewriteGeminiBatchInputFile(body []byte, upstreamName string) ([]byte, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil, errGeminiBatchFidelity
	}
	var batch map[string]json.RawMessage
	if json.Unmarshal(root["batch"], &batch) != nil {
		return nil, errGeminiBatchFidelity
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(batch["inputConfig"], &input) != nil {
		return nil, errGeminiBatchFidelity
	}
	input["fileName"], _ = json.Marshal(upstreamName)
	batch["inputConfig"], _ = json.Marshal(input)
	root["batch"], _ = json.Marshal(batch)
	return json.Marshal(root)
}

func (s *Server) geminiBatchPin(ctx context.Context, x *call, authority access.Authority, route *routing.Route, input geminiBatchInput) (*pin, *routing.Route, *resources.Resource, *Error) {
	if input.LocalFileID == "" {
		p, err := s.geminiPin(ctx, x, route)
		return p, route, nil, err
	}
	file, err := s.Resources.Get(ctx, resources.KindGeminiFile, authority.ID, input.LocalFileID)
	if err != nil {
		return nil, nil, nil, notFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	if file.RouteSlug != route.Slug {
		return nil, nil, nil, notFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	p, pinnedRoute, pinErr := s.resolveResource(ctx, x, authority, file, "batch", retainedNewWork)
	if pinErr != nil || pinnedRoute == nil || pinnedRoute.Slug != route.Slug {
		if pinErr != nil && pinErr.Code != "not_found" {
			return nil, nil, nil, pinErr
		}
		return nil, nil, nil, notFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	return p, pinnedRoute, file, nil
}

func geminiBatchProjectionError(err error) *Error {
	if errors.Is(err, errGeminiBatchFidelity) {
		return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unprojectable Gemini batch object.")
	}
	return serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch object could not be projected.")
}

func (s *Server) mapUpstreamGeminiFile(ctx context.Context, owner *resources.Resource, upstreamName string) (*resources.Resource, error) {
	upstreamID, err := geminiFileID(upstreamName)
	if err != nil {
		return nil, errGeminiBatchFidelity
	}
	identity := owner.NativeIdentity()
	identity.Kind, identity.UpstreamID = resources.KindGeminiFile, upstreamID
	if existing, err := s.Resources.GetByUpstream(ctx, identity); err == nil {
		return existing, nil
	} else if !errors.Is(err, resources.ErrNotFound) {
		return nil, err
	}
	metadata := map[string]json.RawMessage{}
	metadata["upstream_model"], _ = json.Marshal(resourceModel(owner))
	metadata["name"], _ = json.Marshal(upstreamName)
	resource := &resources.Resource{
		Kind: resources.KindGeminiFile, APIKeyID: owner.APIKeyID, RouteSlug: owner.RouteSlug,
		ProviderID: owner.ProviderID, ProviderRevisionID: owner.ProviderRevisionID,
		RouteRevisionID: owner.RouteRevisionID, TargetID: owner.TargetID, SlotID: owner.SlotID,
		CredentialID: owner.CredentialID, UpstreamID: upstreamID, State: "processed",
		Metadata: mustJSON(metadata),
	}
	if owner.ExpiresAt != nil {
		expires := *owner.ExpiresAt
		resource.ExpiresAt = &expires
	}
	commitCtx, stop := resourceCommitContext(ctx)
	defer stop()
	created, err := s.Resources.Put(commitCtx, resource)
	if err == nil {
		return created, nil
	}
	if existing, readErr := s.Resources.GetByUpstream(commitCtx, identity); readErr == nil {
		return existing, nil
	}
	return nil, err
}

func (s *Server) geminiBatchCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, access.Authority, *resources.Resource, *pin) *Error) {
	x, authority, done := s.geminiBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	localID := r.PathValue("name")
	if strings.HasPrefix(localID, "batches/") {
		localID = strings.TrimPrefix(localID, "batches/")
	}
	resource, err := s.Resources.Get(r.Context(), resources.KindGeminiBatch, authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No Gemini batch with this name exists for this key."), "gemini")
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The Gemini batch mapping could not be read."), "gemini")
		return
	}
	p, route, pinErr := s.resolveResource(r.Context(), x, authority, resource, "batch", use)
	if pinErr != nil {
		s.stateFail(x, w, pinErr, "gemini")
		return
	}
	if !p.provider.Connector().Supports("batch", "gemini", "unary") || !p.provider.Supports(p.model, "batch", "gemini", "unary") {
		s.stateFail(x, w, notFoundError("not_found", "No Gemini batch with this name exists for this key."), "gemini")
		return
	}
	if route.Fidelity.Strict() {
		s.stateFail(x, w, policyUnavailable("target_capability", "Strict routes do not serve Gemini batches."), "gemini")
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, false); err != nil {
		s.stateFail(x, w, err, "gemini")
		return
	}
	if err := op(ctx, x, authority, resource, p); err != nil {
		s.stateFail(x, w, err, "gemini")
	}
}

func (s *Server) geminiBatchResourceObject(ctx context.Context, resource *resources.Resource, transient ...[]byte) ([]byte, error) {
	var metadata map[string]json.RawMessage
	body := []byte(resource.Metadata)
	if len(transient) > 0 {
		body = transient[0]
	}
	if json.Unmarshal(body, &metadata) != nil || metadata == nil {
		return nil, errGeminiBatchFidelity
	}
	localInput, upstreamInput := geminiBatchInternalNames(resource)
	delete(metadata, "upstream_model")
	delete(metadata, "local_input_file_name")
	delete(metadata, "upstream_input_file_name")
	body, err := json.Marshal(metadata)
	if err != nil {
		return nil, errGeminiBatchFidelity
	}
	files, err := geminiBatchResponseFiles(body)
	if err != nil {
		return nil, err
	}
	outputs := make(map[string]string, len(files))
	for _, upstream := range files {
		mapped, mapErr := s.mapUpstreamGeminiFile(ctx, resource, upstream)
		if mapErr != nil {
			return nil, mapErr
		}
		outputs[upstream] = "files/" + mapped.ID
	}
	return projectGeminiBatchObject(body, resource.ID, resource.RouteSlug, localInput, upstreamInput, outputs)
}

func (s *Server) geminiListBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.geminiBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	pageSize, pageToken, cursorErr := geminiBatchPageCursor(r.URL.Query())
	if cursorErr != nil {
		s.stateFail(x, w, cursorErr, "gemini")
		return
	}
	rows, err := s.Resources.ListKinds(r.Context(), []string{resources.KindGeminiBatch}, authority.ID, pageSize+1, pageToken)
	if errors.Is(err, resources.ErrNotFound) {
		param := "pageToken"
		s.stateFail(x, w, invalidRequest("invalid_value", "pageToken must identify a Gemini batch owned by this key.", &param), "gemini")
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The Gemini batch list could not be read."), "gemini")
		return
	}
	hasMore := len(rows) > pageSize
	if hasMore {
		rows = rows[:pageSize]
	}
	operations := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if authErr := s.authorizeListedResource(r.Context(), authority, row); authErr != nil {
			s.stateFail(x, w, authErr, "gemini")
			return
		}
		operation, projectErr := s.geminiBatchResourceObject(r.Context(), row)
		if projectErr != nil {
			s.stateFail(x, w, geminiBatchProjectionError(projectErr), "gemini")
			return
		}
		operations = append(operations, operation)
	}
	response := map[string]any{"operations": operations}
	if hasMore && len(rows) > 0 {
		response["nextPageToken"] = rows[len(rows)-1].ID
	}
	s.writeStateJSON(w, x, mustJSON(response))
}

func (s *Server) geminiBatchRequest(w http.ResponseWriter, r *http.Request, use retainedUse, method string, cancel bool) {
	s.geminiBatchCall(w, r, use, func(ctx context.Context, x *call, _ access.Authority, resource *resources.Resource, p *pin) *Error {
		path := "batches/" + url.PathEscape(resource.UpstreamID)
		if cancel {
			path += ":cancel"
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, path, nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved.")
		}
		response, failure := s.pinnedDo(ctx, x, p, method, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.Resources.Tombstone(ctx, resource.ID)
				return notFoundError("not_found", "No Gemini batch with this name exists for this key.")
			}
			return upstreamError(failure)
		}
		defer response.Body.Close()
		body, err := readBounded(response.Body, s.cfg.MaxResponseBytes)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The Gemini batch response could not be read.")
		}
		if len(bytes.TrimSpace(body)) == 0 && cancel {
			x.dispatched = true
			projected, projectErr := s.geminiBatchResourceObject(ctx, resource)
			if projectErr != nil {
				return geminiBatchProjectionError(projectErr)
			}
			x.completePinned()
			s.writeStateJSON(w, x, projected)
			return nil
		}
		upstreamID, nameErr := geminiBatchName(body)
		if nameErr != nil || upstreamID != resource.UpstreamID {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini batch identity.")
		}
		_, oldMetadata, localFileName, upstreamFileName := geminiBatchResourceMetadata(resource)
		metadata := geminiBatchMetadata(body, resourceModel(resource), localFileName, upstreamFileName, oldMetadata)
		if err := s.Resources.Update(ctx, resource.ID, geminiBatchState(body), metadata, nil); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch refresh could not be committed.")
		}
		resource.Metadata = metadata
		resource.State = geminiBatchState(body)
		x.dispatched = true
		projected, projectErr := s.geminiBatchResourceObject(ctx, resource, body)
		if projectErr != nil {
			return geminiBatchProjectionError(projectErr)
		}
		x.completePinned()
		s.writeStateJSON(w, x, projected)
		return nil
	})
}

func (s *Server) geminiGetBatch(w http.ResponseWriter, r *http.Request) {
	s.geminiBatchRequest(w, r, retainedRetrieval, http.MethodGet, false)
}

func (s *Server) geminiBatchCancel(w http.ResponseWriter, r *http.Request) {
	localID, action, ok := strings.Cut(r.PathValue("action"), ":")
	if !ok || action != "cancel" || localID == "" {
		x, _, done := s.geminiBegin(w, r, wire.GeminiBatch)
		if !done {
			defer s.release(r.Context())
			s.stateFail(x, w, notFoundError("not_found", "Unknown Gemini batch action."), "gemini")
		}
		return
	}
	r.SetPathValue("name", localID)
	s.geminiBatchRequest(w, r, retainedHousekeeping, http.MethodPost, true)
}

func (s *Server) geminiDeleteBatch(w http.ResponseWriter, r *http.Request) {
	s.geminiBatchCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, _ access.Authority, resource *resources.Resource, p *pin) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "batches/"+url.PathEscape(resource.UpstreamID), nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved.")
		}
		response, failure := s.pinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil && failure.status != http.StatusNotFound {
			return upstreamError(failure)
		}
		if response != nil {
			_ = response.Body.Close()
		}
		if err := s.Resources.Tombstone(ctx, resource.ID); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch mapping could not be deleted.")
		}
		x.dispatched = true
		x.completePinned()
		s.writeStateJSON(w, x, []byte(`{}`))
		return nil
	})
}

func geminiBatchPageCursor(query url.Values) (int, string, *Error) {
	pageSize := 10
	if raw := query.Get("pageSize"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			param := "pageSize"
			return 0, "", invalidRequest("invalid_value", "pageSize must be an integer between 1 and 100.", &param)
		}
		pageSize = parsed
	}
	token := query.Get("pageToken")
	if token != "" && !validGeminiResourceID(token, resources.KindGeminiBatch) {
		param := "pageToken"
		return 0, "", invalidRequest("invalid_value", "pageToken must be a local Gemini batch identifier.", &param)
	}
	return pageSize, token, nil
}

func validGeminiResourceID(value, kind string) bool {
	prefix := kind + "_"
	raw := strings.TrimPrefix(value, prefix)
	if raw == value || len(raw) != 32 {
		return false
	}
	id, err := uuid.Parse(raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:])
	return err == nil && resources.LocalID(kind, id) == value
}

func geminiBatchResourceMetadata(resource *resources.Resource) (map[string]json.RawMessage, json.RawMessage, string, string) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(resource.Metadata, &fields)
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	local, upstream := geminiBatchInternalNames(resource)
	return fields, resource.Metadata, local, upstream
}
