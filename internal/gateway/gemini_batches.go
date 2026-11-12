package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
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

func validateGeminiBatchRequest(body []byte, slug string) (geminiBatchInput, *fault.Error) {
	doc, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil || doc.Root().Kind() != oif.Object || !geminiOnlyFields(doc.Root(), "batch") {
		return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "The request must contain only a batch object.", nil)
	}
	batch, present := doc.Root().Lookup("batch")
	if !present || batch.Kind() != oif.Object {
		return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "batch must be an object.", nil)
	}
	for _, member := range batch.Members() {
		if member.Name != "display_name" && member.Name != "displayName" && member.Name != "input_config" && member.Name != "inputConfig" {
			return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "The batch contains an unsupported field.", nil)
		}
	}
	displayName, displayPresent, err := geminiBatchAlias(batch, "display_name", "displayName")
	if err != nil {
		return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "The batch display name is invalid.", nil)
	}
	displayValue, err := geminiDisplayName(displayName, displayPresent)
	if err != nil {
		return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "displayName must be a string no longer than 512 bytes.", nil)
	}
	input, inputPresent, err := geminiBatchAlias(batch, "input_config", "inputConfig")
	if err != nil || !inputPresent {
		return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "The batch must contain exactly one input_config.", nil)
	}
	inputDoc, err := oif.ParseJSON(input, oif.Limits{})
	if err != nil || inputDoc.Root().Kind() != oif.Object {
		return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "input_config must be an object.", nil)
	}
	inputMembers := inputDoc.Root().Members()
	if len(inputMembers) != 1 || inputMembers[0].Name != "requests" &&
		inputMembers[0].Name != "file_name" && inputMembers[0].Name != "fileName" {
		return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "input_config must contain requests or one file name.", nil)
	}
	inputResult := geminiBatchInput{DisplayName: displayValue}
	if inputMembers[0].Name == "file_name" || inputMembers[0].Name == "fileName" {
		var name string
		if json.Unmarshal([]byte(inputMembers[0].Value.Raw()), &name) != nil {
			return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "The input file name is invalid.", nil)
		}
		localID, ok := strings.CutPrefix(name, "files/")
		if !ok || !validGeminiFileID(localID) {
			return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "The input file name must identify a local Gemini file.", nil)
		}
		inputResult.LocalFileID = localID
		inputResult.UpstreamFile = name
	}
	if inputMembers[0].Name == "requests" {
		requestsObject := inputMembers[0].Value
		if requestsObject.Kind() != oif.Object || len(requestsObject.Members()) != 1 || requestsObject.Members()[0].Name != "requests" {
			return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "input_config.requests must contain only requests.", nil)
		}
		list := requestsObject.Members()[0].Value
		if list.Kind() != oif.Array || len(list.Elements()) < 1 || len(list.Elements()) > 100000 {
			return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "A Gemini batch must contain between 1 and 100000 requests.", nil)
		}
		seen := map[string]struct{}{}
		inputItems := make([]json.RawMessage, 0, len(list.Elements()))
		keys := make([]string, 0, len(list.Elements()))
		for _, item := range list.Elements() {
			if item.Kind() != oif.Object || !geminiOnlyFields(item, "request", "metadata") {
				return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "Each inline batch item must contain only request and metadata.", nil)
			}
			request, requestPresent := item.Lookup("request")
			metadata, metadataPresent := item.Lookup("metadata")
			if !requestPresent || !metadataPresent || metadata.Kind() != oif.Object || !geminiOnlyFields(metadata, "key") {
				return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "Each inline batch item requires request and metadata.key.", nil)
			}
			keyValue, keyPresent := metadata.Lookup("key")
			var key string
			if !keyPresent || json.Unmarshal([]byte(keyValue.Raw()), &key) != nil || key == "" || len(key) > 128 {
				return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "metadata.key must be a non-empty string of at most 128 bytes.", nil)
			}
			if _, exists := seen[key]; exists {
				return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "metadata.key values must be unique.", nil)
			}
			seen[key] = struct{}{}
			if err := validateGeminiBatchItem(request, slug); err != nil {
				return geminiBatchInput{}, err
			}
			keys = append(keys, key)
			inputItems = append(inputItems, json.RawMessage(item.Raw()))
		}
		count := int64(len(inputItems))
		inputResult.Expected = &count
		inputResult.Keys = keys
	}
	canonical, err := canonicalBatchJSON(body)
	if err != nil {
		return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "The batch request is invalid JSON.", nil)
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
		return geminiBatchInput{}, fault.InvalidRequest("invalid_request", "The batch request could not be translated.", nil)
	}
	inputResult.Canonical, inputResult.Upstream = canonical, upstream
	return inputResult, nil
}

func validateGeminiBatchItem(request oif.Value, slug string) *fault.Error {
	if request.Kind() != oif.Object {
		return fault.InvalidRequest("invalid_request", "A batch request must be an object.", nil)
	}
	if _, err := protocols.Parse(wire.Gemini, []byte(request.Raw()), slug); err != nil {
		return fault.InvalidRequest("invalid_request", "A batch request is not a valid Gemini GenerateContent request.", nil)
	}
	if geminiBatchHasFileReference(request) {
		return fault.InvalidRequest("unsupported_parameter", "Gemini batches do not support unbound file or cached-content references.", nil)
	}
	return nil
}

func geminiBatchHasFileReference(request oif.Value) bool {
	var parts func(oif.Value) bool
	parts = func(values oif.Value) bool {
		for _, part := range values.Elements() {
			for _, field := range part.Members() {
				switch field.Name {
				case "fileData", "file_data":
					for _, source := range field.Value.Members() {
						if source.Name == "fileUri" || source.Name == "file_uri" {
							return true
						}
					}
				case "functionResponse", "function_response":
					for _, response := range field.Value.Members() {
						if response.Name == "parts" && parts(response.Value) {
							return true
						}
					}
				}
			}
		}
		return false
	}
	content := func(value oif.Value) bool {
		for _, field := range value.Members() {
			if field.Name == "parts" && parts(field.Value) {
				return true
			}
		}
		return false
	}
	for _, field := range request.Members() {
		switch field.Name {
		case "cachedContent", "cached_content":
			if field.Value.Kind() != oif.Null {
				return true
			}
		case "systemInstruction", "system_instruction":
			if content(field.Value) {
				return true
			}
		case "contents":
			for _, item := range field.Value.Elements() {
				if content(item) {
					return true
				}
			}
		}
	}
	return false
}

func qualifyGeminiBatchJSONL(input io.Reader, slug string, digest func(string) ([]byte, bool)) (batchManifest, error) {
	manifest := batchManifest{Adapter: geminiNativeManifest, LineOperation: wire.LineGeneration}
	seen := map[[32]byte]struct{}{}
	hash := sha256.New()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), batches.MaxLineBytes+1)
	for scanner.Scan() {
		if manifest.LineCount >= 100000 {
			return batchManifest{}, errors.New("Gemini batch input exceeds its request limit")
		}
		document, err := oif.ParseJSON(scanner.Bytes(), oif.Limits{MaxBytes: batches.MaxLineBytes})
		if err != nil || document.Root().Kind() != oif.Object || !geminiOnlyFields(document.Root(), "key", "request") {
			return batchManifest{}, errors.New("invalid Gemini batch JSONL item")
		}
		keyValue, ok := document.Root().Lookup("key")
		key, valid := keyValue.Text()
		if !ok || !valid || key == "" || len(key) > 128 {
			return batchManifest{}, errors.New("invalid Gemini batch item key")
		}
		request, present := document.Root().Lookup("request")
		if !present {
			return batchManifest{}, errors.New("Gemini batch item has no request")
		}
		if err := validateGeminiBatchItem(request, slug); err != nil {
			return batchManifest{}, err
		}
		identity, ok := digest(key)
		if !ok || len(identity) != 32 {
			return batchManifest{}, errors.New("Gemini batch identity is unavailable")
		}
		var identityKey [32]byte
		copy(identityKey[:], identity)
		if _, duplicate := seen[identityKey]; duplicate {
			return batchManifest{}, errors.New("duplicate Gemini batch item key")
		}
		seen[identityKey] = struct{}{}
		hash.Write(identity)
		manifest.LineCount++
	}
	if scanner.Err() != nil {
		return batchManifest{}, scanner.Err()
	}
	if manifest.LineCount == 0 {
		return batchManifest{}, errors.New("Gemini batch input is empty")
	}
	manifest.ManifestDigest = hex.EncodeToString(hash.Sum(nil))
	return manifest, nil
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

func canonicalBatchJSON(body []byte) ([]byte, error) {
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

func geminiBatchResponseFiles(body []byte) ([]string, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, errGeminiBatchFidelity
	}
	var metadata map[string]json.RawMessage
	if raw, ok := root["metadata"]; ok {
		if json.Unmarshal(raw, &metadata) != nil || metadata == nil {
			return nil, errGeminiBatchFidelity
		}
	}
	var files []string
	for _, parent := range []map[string]json.RawMessage{root, metadata} {
		for _, field := range upstream.GeminiOutputFields() {
			raw, ok := parent[field]
			if !ok {
				continue
			}
			var destination map[string]json.RawMessage
			if json.Unmarshal(raw, &destination) != nil || destination == nil {
				return nil, errGeminiBatchFidelity
			}
			if rawName, present := destination["responsesFile"]; present {
				var name string
				if json.Unmarshal(rawName, &name) != nil {
					return nil, errGeminiBatchFidelity
				}
				if _, err := geminiFileID(name); err != nil {
					return nil, errGeminiBatchFidelity
				}
				files = append(files, name)
			}
		}
	}
	return slices.Compact(files), nil
}

func geminiBatchCancelAcknowledgement(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(body, &value) == nil && value != nil && len(value) == 0
}

func projectGeminiBatchObject(body []byte, localID, routeSlug, localInputFile, upstreamInputFile string, outputFiles map[string]string) ([]byte, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return nil, errGeminiBatchFidelity
	}
	batch, err := upstream.DecodeGeminiBatch(body)
	if err != nil {
		return nil, errGeminiBatchFidelity
	}
	upstreamID, foundInput := batch.ID, batch.InputFile
	delete(object, "upstream_model")
	delete(object, "local_input_file_name")
	delete(object, "upstream_input_file_name")
	cleanBody, err := json.Marshal(object)
	if err != nil {
		return nil, errGeminiBatchFidelity
	}
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
	for _, file := range files {
		if outputFiles[file] == "" {
			return nil, errGeminiBatchFidelity
		}
	}
	localName := mustJSON("batches/" + localID)
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
			metadata["model"] = mustJSON("models/" + routeSlug)
		}
		if err := projectGeminiBatchFiles(metadata, localInputFile, upstreamInputFile, outputFiles); err != nil {
			return nil, err
		}
		object["metadata"] = mustJSON(metadata)
	}
	if _, ok := object["model"]; ok {
		object["model"] = mustJSON("models/" + routeSlug)
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
					config[nameField] = mustJSON(localInputFile)
				}
			}
			object[field] = mustJSON(config)
		}
	}
	for _, outputField := range upstream.GeminiOutputFields() {
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
				destination["responsesFile"] = mustJSON(outputFiles[upstream])
			}
			object[outputField] = mustJSON(destination)
		}
	}
	return nil
}

func (s *Server) geminiCreateBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	slug := r.PathValue("model")
	if header := strings.TrimSpace(r.Header.Get(stateRouteHeader)); header != "" && header != slug {
		s.refuse(x, w, fault.InvalidRequest("invalid_request", "The Gemini batch model path must match X-OLP-Route.", nil))
		return
	}
	route, err := s.nativeStateRoute(x, authority, r, slug)
	if err != nil {
		s.refuse(x, w, err)
		return
	}
	body, err := s.readResourceBody(w, r)
	if err != nil {
		s.refuse(x, w, err)
		return
	}
	input, err := validateGeminiBatchRequest(body, route.Slug)
	if err != nil {
		s.refuse(x, w, err)
		return
	}
	p, pinnedRoute, file, err := s.geminiBatchPin(r.Context(), x, authority, route, input)
	if err != nil {
		s.refuse(x, w, err)
		return
	}
	if pinnedRoute != nil {
		route = pinnedRoute
	}
	x.route = route
	if file != nil {
		var metadata struct {
			Manifest batchManifest `json:"batch_manifest"`
		}
		if json.Unmarshal(file.Metadata, &metadata) != nil || metadata.Manifest.Adapter != geminiNativeManifest || metadata.Manifest.LineCount < 1 || metadata.Manifest.ManifestDigest == "" {
			s.releaseHold(r.Context(), p.hold)
			s.refuse(x, w, policyUnavailable("target_capability", "File-backed Gemini batches require qualified JSONL uploaded in one complete final chunk."))
			return
		}
		input.Expected = new(int64(metadata.Manifest.LineCount))

		input.UpstreamFile = "files/" + file.UpstreamID
		rewritten, rewriteErr := rewriteGeminiBatchInputFile(input.Upstream, input.UpstreamFile)
		if rewriteErr != nil {
			s.releaseHold(r.Context(), p.hold)
			s.refuse(x, w, fault.InvalidRequest("invalid_request", "The input file name could not be bound.", nil))
			return
		}
		input.Upstream = rewritten
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.refuse(x, w, err)
		return
	}
	source, sourceErr := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if sourceErr != nil {
		s.refuse(x, w, fault.InvalidRequest("invalid_request", "The Gemini batch request is invalid.", nil))
		return
	}
	localInputFile, upstreamFile := geminiInputFiles(file)
	endpoint, endpointErr := resourceURL(p.provider.Connector(), p.model, "models/"+url.PathEscape(p.provider.Connector().Model(p.model))+":batchGenerateContent", nil)
	if endpointErr != nil {
		s.refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved."))
		return
	}
	submission, e := s.beginBatch(ctx, x, authority.ID, p, batchIntent{
		kind: resources.KindGeminiBatch, adapter: connectors.BatchGemini, surface: batches.SurfaceGemini,
		operation: wire.LineGeneration, window: 48 * time.Hour, expected: input.Expected,
		submission: geminiSubmission(localInputFile, upstreamFile), source: source, canonical: input.Canonical, effective: input.Upstream,
	})
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	id := submission.resource.UUID
	result, e := s.sendBatch(ctx, x, p, id, endpoint, input.Upstream)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	batch, decodeErr := upstream.DecodeGeminiBatch(result)
	if decodeErr != nil {
		s.refuse(x, w, s.batchSubmissionUnknown(ctx, id, "The provider returned an invalid batch object. The batch may have been accepted."))
		return
	}
	metadata, metadataErr := upstream.GeminiMetadata(result, p.model, localInputFile, upstreamFile)
	if metadataErr != nil {
		s.refuse(x, w, s.batchSubmissionUnknown(ctx, id, "The provider returned an invalid batch object. The batch may have been accepted."))
		return
	}
	resource, e := s.activateBatch(ctx, submission, acceptedBatch{upstreamID: batch.ID, state: batch.State, metadata: metadata})
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	projected, projectErr := s.geminiBatchResourceObject(ctx, resource, result)
	if projectErr != nil {
		s.refuse(x, w, geminiBatchProjectionError(projectErr))
		return
	}
	x.completePinned()
	s.writeStateJSON(w, x, projected)
}

func geminiInputFiles(file *resources.Resource) (string, string) {
	if file == nil {
		return "", ""
	}
	return "files/" + file.ID, "files/" + file.UpstreamID
}

func geminiSubmission(localInputFile, upstreamFile string) batches.Submission {
	if upstreamFile == "" {
		return batches.GeminiInlineSubmission{}
	}
	return batches.GeminiFileSubmission{GeminiFile: upstreamFile, LocalInputFileID: localInputFile}
}

type geminiInputNames struct {
	Local    string `json:"local_input_file_name"`
	Upstream string `json:"upstream_input_file_name"`
}

func geminiBatchInputNames(resource *resources.Resource) (geminiInputNames, error) {
	var names geminiInputNames
	err := json.Unmarshal(resource.Metadata, &names)
	return names, err
}

func geminiDisplayName(raw json.RawMessage, present bool) (*string, error) {
	if !present {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if len(value) > 512 {
		return nil, errors.New("display name exceeds 512 bytes")
	}
	return &value, nil
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
	input["fileName"] = mustJSON(upstreamName)
	batch["inputConfig"] = mustJSON(input)
	root["batch"] = mustJSON(batch)
	return json.Marshal(root)
}

func (s *Server) geminiBatchPin(ctx context.Context, x *call, authority keyauthority.Authority, route *routing.Route, input geminiBatchInput) (*pin, *routing.Route, *resources.Resource, *fault.Error) {
	if input.LocalFileID == "" {
		p, err := s.nativeStatePin(ctx, x, route)
		return p, route, nil, err
	}
	file, err := s.deps.Resources.Get(ctx, resources.KindGeminiFile, authority.ID, input.LocalFileID)
	if err != nil {
		return nil, nil, nil, fault.NotFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	if file.RouteSlug != route.Slug {
		return nil, nil, nil, fault.NotFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	p, pinnedRoute, pinErr := s.resolveResource(ctx, x, authority, file, "batch", retainedNewWork)
	if pinErr != nil || pinnedRoute == nil || pinnedRoute.Slug != route.Slug {
		if pinErr != nil && pinErr.Code != "not_found" {
			return nil, nil, nil, pinErr
		}
		return nil, nil, nil, fault.NotFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	return p, pinnedRoute, file, nil
}

func geminiBatchProjectionError(err error) *fault.Error {
	if errors.Is(err, errGeminiBatchFidelity) {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unprojectable Gemini batch object.")
	}
	return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch object could not be projected.")
}

func (s *Server) mapUpstreamGeminiFile(ctx context.Context, owner *resources.Resource, upstreamName string) (*resources.Resource, error) {
	upstreamID, err := geminiFileID(upstreamName)
	if err != nil {
		return nil, errGeminiBatchFidelity
	}
	identity := owner.NativeIdentity()
	identity.Kind, identity.UpstreamID = resources.KindGeminiFile, upstreamID
	if existing, err := s.deps.Resources.GetByUpstreamIncludingDeleted(ctx, identity); err == nil {
		return existing, nil
	} else if !errors.Is(err, resources.ErrNotFound) {
		return nil, err
	}
	metadata := map[string]json.RawMessage{}
	metadata["upstream_model"] = mustJSON(resourceModel(owner))
	metadata["name"] = mustJSON(upstreamName)
	resource := &resources.Resource{
		Kind: resources.KindGeminiFile, APIKeyID: owner.APIKeyID, RouteSlug: owner.RouteSlug,
		ProviderID: owner.ProviderID, ProviderRevisionID: owner.ProviderRevisionID,
		RouteRevisionID: owner.RouteRevisionID, TargetID: owner.TargetID, SlotID: owner.SlotID,
		CredentialID: owner.CredentialID, UpstreamID: upstreamID, State: "processed",
		Metadata: mustJSON(metadata), ParentID: new(owner.UUID),
	}
	if owner.ExpiresAt != nil {
		expires := *owner.ExpiresAt
		resource.ExpiresAt = &expires
	}
	commitCtx, stop := resourceCommitContext(ctx)
	defer stop()
	created, err := s.deps.Resources.Put(commitCtx, resource)
	if err == nil {
		return created, nil
	}
	if existing, readErr := s.deps.Resources.GetByUpstreamIncludingDeleted(commitCtx, identity); readErr == nil {
		return existing, nil
	}
	return nil, err
}

func (s *Server) geminiBatchCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, keyauthority.Authority, *resources.Resource, *pin) *fault.Error) {
	x, authority, done := s.stateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	localID := strings.TrimPrefix(r.PathValue("name"), "batches/")
	resource, err := s.deps.Resources.Get(r.Context(), resources.KindGeminiBatch, authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		s.refuse(x, w, fault.NotFoundError("not_found", "No Gemini batch with this name exists for this key."))
		return
	}
	if err != nil {
		s.refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini batch mapping could not be read."))
		return
	}
	p, route, pinErr := s.resolveResource(r.Context(), x, authority, resource, "batch", use)
	if pinErr != nil {
		s.refuse(x, w, pinErr)
		return
	}
	if !p.provider.Connector().Supports("batch", "gemini", "unary") || !p.provider.Supports(p.model, "batch", "gemini", "unary") {
		s.refuse(x, w, fault.NotFoundError("not_found", "No Gemini batch with this name exists for this key."))
		return
	}
	if route.Fidelity.Strict() {
		s.refuse(x, w, policyUnavailable("target_capability", "Strict routes do not serve Gemini batches."))
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, false); err != nil {
		s.refuse(x, w, err)
		return
	}
	if err := op(ctx, x, authority, resource, p); err != nil {
		s.refuse(x, w, err)
	}
}

func (s *Server) geminiBatchResourceObject(ctx context.Context, resource *resources.Resource, body []byte) ([]byte, error) {
	var metadata map[string]json.RawMessage
	if json.Unmarshal(body, &metadata) != nil || metadata == nil {
		return nil, errGeminiBatchFidelity
	}
	names, err := geminiBatchInputNames(resource)
	if err != nil {
		return nil, errGeminiBatchFidelity
	}
	delete(metadata, "upstream_model")
	delete(metadata, "local_input_file_name")
	delete(metadata, "upstream_input_file_name")
	clean, err := json.Marshal(metadata)
	if err != nil {
		return nil, errGeminiBatchFidelity
	}
	files, err := geminiBatchResponseFiles(clean)
	if err != nil {
		return nil, err
	}
	outputs := make(map[string]string, len(files))
	for _, file := range files {
		mapped, mapErr := s.mapUpstreamGeminiFile(ctx, resource, file)
		if mapErr != nil {
			return nil, mapErr
		}
		outputs[file] = "files/" + mapped.ID
	}
	return projectGeminiBatchObject(clean, resource.ID, resource.RouteSlug, names.Local, names.Upstream, outputs)
}

func (s *Server) geminiListBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	pageSize, pageToken, cursorErr := geminiBatchPageCursor(r.URL.Query())
	if cursorErr != nil {
		s.refuse(x, w, cursorErr)
		return
	}
	rows, err := s.deps.Resources.ListKinds(r.Context(), []string{resources.KindGeminiBatch}, authority.ID, pageSize+1, pageToken)
	if errors.Is(err, resources.ErrNotFound) {
		param := "pageToken"
		s.refuse(x, w, fault.InvalidRequest("invalid_value", "pageToken must identify a Gemini batch owned by this key.", &param))
		return
	}
	if err != nil {
		s.refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini batch list could not be read."))
		return
	}
	hasMore := len(rows) > pageSize
	if hasMore {
		rows = rows[:pageSize]
	}
	operations := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if authErr := s.authorizeListedResource(r.Context(), authority, row); authErr != nil {
			s.refuse(x, w, authErr)
			return
		}
		operation, projectErr := s.geminiBatchResourceObject(r.Context(), row, row.Metadata)
		if projectErr != nil {
			s.refuse(x, w, geminiBatchProjectionError(projectErr))
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
	s.geminiBatchCall(w, r, use, func(ctx context.Context, x *call, _ keyauthority.Authority, resource *resources.Resource, p *pin) *fault.Error {
		path := "batches/" + url.PathEscape(resource.UpstreamID)
		if cancel {
			path += ":cancel"
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, path, nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved.")
		}
		response, failure := s.pinnedDo(ctx, x, p, method, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.deps.Resources.Tombstone(ctx, resource.ID)
				return fault.NotFoundError("not_found", "No Gemini batch with this name exists for this key.")
			}
			return upstreamError(failure)
		}
		defer response.Body.Close()
		body, err := readBounded(response.Body, s.cfg.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch response could not be read.")
		}
		if cancel && geminiBatchCancelAcknowledgement(body) {
			x.dispatched = true
			projected, projectErr := s.geminiBatchResourceObject(ctx, resource, resource.Metadata)
			if projectErr != nil {
				return geminiBatchProjectionError(projectErr)
			}
			x.completePinned()
			s.writeStateJSON(w, x, projected)
			return nil
		}
		batch, decodeErr := upstream.DecodeGeminiBatch(body)
		if decodeErr != nil || batch.ID != resource.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini batch identity.")
		}
		names, namesErr := geminiBatchInputNames(resource)
		if namesErr != nil {
			return geminiBatchProjectionError(errGeminiBatchFidelity)
		}
		metadata, metadataErr := upstream.GeminiMetadata(body, resourceModel(resource), names.Local, names.Upstream)
		if metadataErr != nil {
			return geminiBatchProjectionError(errGeminiBatchFidelity)
		}
		if err := s.deps.Resources.Update(ctx, resource.ID, batch.State, metadata, nil); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch refresh could not be committed.")
		}
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
		x, _, done := s.stateBegin(w, r, wire.GeminiBatch)
		if !done {
			defer s.release(r.Context())
			s.refuse(x, w, fault.NotFoundError("not_found", "Unknown Gemini batch action."))
		}
		return
	}
	r.SetPathValue("name", localID)
	s.geminiBatchRequest(w, r, retainedHousekeeping, http.MethodPost, true)
}

func (s *Server) geminiDeleteBatch(w http.ResponseWriter, r *http.Request) {
	s.geminiBatchCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, _ keyauthority.Authority, resource *resources.Resource, p *pin) *fault.Error {
		if err := s.requireBatchEvidenceSettled(ctx, resource.UUID); err != nil {
			return err
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "batches/"+url.PathEscape(resource.UpstreamID), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved.")
		}
		response, failure := s.pinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil && failure.status != http.StatusNotFound {
			return upstreamError(failure)
		}
		if response != nil {
			_ = response.Body.Close()
		}
		if err := s.deps.Resources.Tombstone(ctx, resource.ID); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch mapping could not be deleted.")
		}
		x.dispatched = true
		x.completePinned()
		s.writeStateJSON(w, x, []byte(`{}`))
		return nil
	})
}

func geminiBatchPageCursor(query url.Values) (int, string, *fault.Error) {
	pageSize := 10
	if raw := query.Get("pageSize"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			param := "pageSize"
			return 0, "", fault.InvalidRequest("invalid_value", "pageSize must be an integer between 1 and 100.", &param)
		}
		pageSize = parsed
	}
	token := query.Get("pageToken")
	if token != "" && !validGeminiResourceID(token, resources.KindGeminiBatch) {
		param := "pageToken"
		return 0, "", fault.InvalidRequest("invalid_value", "pageToken must be a local Gemini batch identifier.", &param)
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
