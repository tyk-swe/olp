package geministate

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
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

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
		if !ok || !kernel.ValidGeminiFileID(localID) {
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
	canonical, err := kernel.CanonicalBatchJSON(body)
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
	if kernel.GeminiBatchHasFileReference(request) {
		return fault.InvalidRequest("unsupported_parameter", "Gemini batches do not support unbound file or cached-content references.", nil)
	}
	return nil
}

func qualifyGeminiBatchJSONL(input io.Reader, slug string, digest func(string) ([]byte, bool)) (kernel.BatchManifest, error) {
	manifest := kernel.BatchManifest{Adapter: kernel.GeminiNativeManifest, LineOperation: wire.LineGeneration}
	seen := map[[32]byte]struct{}{}
	hash := sha256.New()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), batches.MaxLineBytes+1)
	for scanner.Scan() {
		if manifest.LineCount >= 100000 {
			return kernel.BatchManifest{}, errors.New("Gemini batch input exceeds its request limit")
		}
		document, err := oif.ParseJSON(scanner.Bytes(), oif.Limits{MaxBytes: batches.MaxLineBytes})
		if err != nil || document.Root().Kind() != oif.Object || !geminiOnlyFields(document.Root(), "key", "request") {
			return kernel.BatchManifest{}, errors.New("invalid Gemini batch JSONL item")
		}
		keyValue, ok := document.Root().Lookup("key")
		key, valid := keyValue.Text()
		if !ok || !valid || key == "" || len(key) > 128 {
			return kernel.BatchManifest{}, errors.New("invalid Gemini batch item key")
		}
		request, present := document.Root().Lookup("request")
		if !present {
			return kernel.BatchManifest{}, errors.New("Gemini batch item has no request")
		}
		if err := validateGeminiBatchItem(request, slug); err != nil {
			return kernel.BatchManifest{}, err
		}
		identity, ok := digest(key)
		if !ok || len(identity) != 32 {
			return kernel.BatchManifest{}, errors.New("Gemini batch identity is unavailable")
		}
		var identityKey [32]byte
		copy(identityKey[:], identity)
		if _, duplicate := seen[identityKey]; duplicate {
			return kernel.BatchManifest{}, errors.New("duplicate Gemini batch item key")
		}
		seen[identityKey] = struct{}{}
		hash.Write(identity)
		manifest.LineCount++
	}
	if scanner.Err() != nil {
		return kernel.BatchManifest{}, scanner.Err()
	}
	if manifest.LineCount == 0 {
		return kernel.BatchManifest{}, errors.New("Gemini batch input is empty")
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

func geminiBatchCancelAcknowledgement(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(body, &value) == nil && value != nil && len(value) == 0
}

type geminiBatchObject struct {
	outer, metadata map[string]json.RawMessage
	outputs         []string
}

func prepareGeminiBatchObject(body []byte, batch upstream.GeminiBatch, names geminiInputNames) (geminiBatchObject, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return geminiBatchObject{}, kernel.ErrGeminiBatchFidelity
	}
	var metadata map[string]json.RawMessage
	if raw, present := object["metadata"]; present {
		if json.Unmarshal(raw, &metadata) != nil || metadata == nil {
			return geminiBatchObject{}, kernel.ErrGeminiBatchFidelity
		}
		if rawName, present := metadata["name"]; present {
			var name string
			if json.Unmarshal(rawName, &name) != nil || name != "batches/"+batch.ID {
				return geminiBatchObject{}, kernel.ErrGeminiBatchFidelity
			}
		}
	}
	var outputs []string
	for _, layer := range []map[string]json.RawMessage{object, metadata} {
		if err := projectGeminiBatchInput(layer, names); err != nil {
			return geminiBatchObject{}, err
		}
		files, err := geminiBatchOutputNames(layer)
		if err != nil {
			return geminiBatchObject{}, err
		}
		outputs = append(outputs, files...)
	}
	delete(object, "upstream_model")
	delete(object, "local_input_file_name")
	delete(object, "upstream_input_file_name")
	return geminiBatchObject{outer: object, metadata: metadata, outputs: slices.Compact(outputs)}, nil
}

func projectGeminiBatchInput(object map[string]json.RawMessage, names geminiInputNames) error {
	for _, field := range []string{"inputConfig", "input_config"} {
		raw, present := object[field]
		if !present {
			continue
		}
		var config map[string]json.RawMessage
		if json.Unmarshal(raw, &config) != nil || config == nil {
			return kernel.ErrGeminiBatchFidelity
		}
		for _, nameField := range []string{"fileName", "file_name"} {
			if rawName, present := config[nameField]; present {
				var name string
				if json.Unmarshal(rawName, &name) != nil || names.Local == "" || name != names.Upstream {
					return kernel.ErrGeminiBatchFidelity
				}
				config[nameField] = kernel.MustJSON(names.Local)
			}
		}
		object[field] = kernel.MustJSON(config)
	}
	return nil
}

func geminiBatchOutputNames(object map[string]json.RawMessage) ([]string, error) {
	var files []string
	for _, field := range upstream.GeminiOutputFields() {
		raw, present := object[field]
		if !present {
			continue
		}
		var destination map[string]json.RawMessage
		if json.Unmarshal(raw, &destination) != nil || destination == nil {
			return nil, kernel.ErrGeminiBatchFidelity
		}
		if rawName, present := destination["responsesFile"]; present {
			var name string
			if json.Unmarshal(rawName, &name) != nil {
				return nil, kernel.ErrGeminiBatchFidelity
			}
			if _, err := kernel.GeminiFileID(name); err != nil {
				return nil, kernel.ErrGeminiBatchFidelity
			}
			files = append(files, name)
		}
	}
	return files, nil
}

func (b geminiBatchObject) project(localID, routeSlug string, outputFiles map[string]string) ([]byte, error) {
	localName := kernel.MustJSON("batches/" + localID)
	for _, layer := range []map[string]json.RawMessage{b.outer, b.metadata} {
		if _, present := layer["name"]; present {
			layer["name"] = localName
		}
		if _, present := layer["model"]; present {
			layer["model"] = kernel.MustJSON("models/" + routeSlug)
		}
		for _, field := range upstream.GeminiOutputFields() {
			if raw, present := layer[field]; present {
				var destination map[string]json.RawMessage
				if err := json.Unmarshal(raw, &destination); err != nil {
					return nil, err
				}
				if file, present := destination["responsesFile"]; present {
					var upstreamName string
					if json.Unmarshal(file, &upstreamName) != nil || outputFiles[upstreamName] == "" {
						return nil, kernel.ErrGeminiBatchFidelity
					}
					destination["responsesFile"] = kernel.MustJSON(outputFiles[upstreamName])
				}
				layer[field] = kernel.MustJSON(destination)
			}
		}
	}
	if b.metadata != nil {
		b.outer["metadata"] = kernel.MustJSON(b.metadata)
	}
	return json.Marshal(b.outer)
}

func settleGeminiBatchResult(exchange *kernel.PinnedExchange, body []byte, batch upstream.GeminiBatch, model string, names geminiInputNames) (geminiBatchObject, json.RawMessage, error) {
	object, metadata, err := acceptGeminiBatchResult(body, batch, model, names)
	if err != nil {
		return geminiBatchObject{}, nil, err
	}
	exchange.Complete(kernel.ClassSuccess)
	return object, metadata, nil
}

func acceptGeminiBatchResult(body []byte, batch upstream.GeminiBatch, model string, names geminiInputNames) (geminiBatchObject, json.RawMessage, error) {
	object, err := prepareGeminiBatchObject(body, batch, names)
	if err != nil {
		return geminiBatchObject{}, nil, err
	}
	metadata, err := upstream.GeminiMetadata(body, model, names.Local, names.Upstream)
	if err != nil {
		return geminiBatchObject{}, nil, err
	}
	return object, metadata, nil
}

func (s surface) geminiCreateBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	slug := r.PathValue("model")
	if header := strings.TrimSpace(r.Header.Get(kernel.StateRouteHeader)); header != "" && header != slug {
		s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "The Gemini batch model path must match X-OLP-Route.", nil))
		return
	}
	route, err := s.kernel.NativeStateRoute(x, authority, r, slug)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	body, err := s.kernel.ReadResourceBody(w, r)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	input, err := validateGeminiBatchRequest(body, route.Slug)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	p, pinnedRoute, file, err := s.geminiBatchPin(r.Context(), x, authority, route, input)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	if pinnedRoute != nil {
		route = pinnedRoute
	}
	x.Route = route
	if file != nil {
		var metadata struct {
			Manifest kernel.BatchManifest `json:"batch_manifest"`
		}
		if json.Unmarshal(file.Metadata, &metadata) != nil || metadata.Manifest.Adapter != kernel.GeminiNativeManifest || metadata.Manifest.LineCount < 1 || metadata.Manifest.ManifestDigest == "" {
			s.kernel.ReleaseHold(r.Context(), p.Hold)
			s.kernel.Refuse(x, w, kernel.PolicyUnavailable("target_capability", "File-backed Gemini batches require qualified JSONL uploaded in one complete final chunk."))
			return
		}
		input.Expected = new(int64(metadata.Manifest.LineCount))

		input.UpstreamFile = "files/" + file.UpstreamID
		rewritten, rewriteErr := rewriteGeminiBatchInputFile(input.Upstream, input.UpstreamFile)
		if rewriteErr != nil {
			s.kernel.ReleaseHold(r.Context(), p.Hold)
			s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "The input file name could not be bound.", nil))
			return
		}
		input.Upstream = rewritten
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	source, sourceErr := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if sourceErr != nil {
		s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "The Gemini batch request is invalid.", nil))
		return
	}
	localInputFile, upstreamFile := geminiInputFiles(file)
	endpoint, endpointErr := kernel.ResourceURL(p.Provider.Connector(), p.Model, "models/"+url.PathEscape(p.Provider.Connector().Model(p.Model))+":batchGenerateContent", nil)
	if endpointErr != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved."))
		return
	}
	submission, e := s.kernel.BeginBatch(ctx, x, authority.ID, p, kernel.BatchIntent{
		Kind: resources.KindGeminiBatch, Adapter: connectors.BatchGemini, Surface: batches.SurfaceGemini,
		Operation: wire.LineGeneration, Window: 48 * time.Hour, Expected: input.Expected,
		Submission: geminiSubmission(localInputFile, upstreamFile), Source: source, Canonical: input.Canonical, Effective: input.Upstream,
	})
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	id := submission.Resource.UUID
	result, exchange, e := s.kernel.SendBatch(ctx, x, p, id, endpoint, input.Upstream)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	batch, decodeErr := upstream.DecodeGeminiBatch(result)
	if decodeErr != nil {
		s.kernel.Refuse(x, w, s.kernel.BatchSubmissionUnknown(ctx, id, "The provider returned an invalid batch object. The batch may have been accepted."))
		return
	}
	object, metadata, metadataErr := settleGeminiBatchResult(exchange, result, batch, p.Model, geminiInputNames{Local: localInputFile, Upstream: upstreamFile})
	if metadataErr != nil {
		s.kernel.Refuse(x, w, s.kernel.BatchSubmissionUnknown(ctx, id, "The provider returned an invalid batch object. The batch may have been accepted."))
		return
	}
	resource, e := s.kernel.ActivateBatch(ctx, submission, kernel.AcceptedBatch{UpstreamID: batch.ID, State: batch.State, Metadata: metadata})
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	projected, projectErr := s.projectGeminiBatchResource(ctx, resource, object)
	if projectErr != nil {
		s.kernel.Refuse(x, w, geminiBatchProjectionError(projectErr))
		return
	}
	s.kernel.WriteStateJSON(w, x, projected, exchange)
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
		return nil, kernel.ErrGeminiBatchFidelity
	}
	var batch map[string]json.RawMessage
	if json.Unmarshal(root["batch"], &batch) != nil {
		return nil, kernel.ErrGeminiBatchFidelity
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(batch["inputConfig"], &input) != nil {
		return nil, kernel.ErrGeminiBatchFidelity
	}
	input["fileName"] = kernel.MustJSON(upstreamName)
	batch["inputConfig"] = kernel.MustJSON(input)
	root["batch"] = kernel.MustJSON(batch)
	return json.Marshal(root)
}

func (s surface) geminiBatchPin(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, route *routing.Route, input geminiBatchInput) (*kernel.Pin, *routing.Route, *resources.Resource, *fault.Error) {
	if input.LocalFileID == "" {
		p, err := s.kernel.NativeStatePin(ctx, x, route)
		return p, route, nil, err
	}
	file, err := s.kernel.Deps.Resources.Get(ctx, resources.KindGeminiFile, authority.ID, input.LocalFileID)
	if err != nil {
		return nil, nil, nil, fault.NotFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	if file.RouteSlug != route.Slug {
		return nil, nil, nil, fault.NotFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	p, pinnedRoute, pinErr := s.kernel.ResolveResource(ctx, x, authority, file, "batch", kernel.RetainedNewWork)
	if pinErr != nil || pinnedRoute == nil || pinnedRoute.Slug != route.Slug {
		if pinErr != nil && pinErr.Code != "not_found" {
			return nil, nil, nil, pinErr
		}
		return nil, nil, nil, fault.NotFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	return p, pinnedRoute, file, nil
}

func geminiBatchProjectionError(err error) *fault.Error {
	if errors.Is(err, kernel.ErrGeminiBatchFidelity) {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unprojectable Gemini batch object.")
	}
	return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch object could not be projected.")
}

func (s surface) mapUpstreamGeminiFile(ctx context.Context, owner *resources.Resource, upstreamName string) (*resources.Resource, error) {
	upstreamID, err := kernel.GeminiFileID(upstreamName)
	if err != nil {
		return nil, kernel.ErrGeminiBatchFidelity
	}
	identity := owner.NativeIdentity()
	identity.Kind, identity.UpstreamID = resources.KindGeminiFile, upstreamID
	if existing, err := s.kernel.Deps.Resources.GetByUpstreamIncludingDeleted(ctx, identity); err == nil {
		return existing, nil
	} else if !errors.Is(err, resources.ErrNotFound) {
		return nil, err
	}
	metadata := map[string]json.RawMessage{}
	metadata["upstream_model"] = kernel.MustJSON(kernel.ResourceModel(owner))
	metadata["name"] = kernel.MustJSON(upstreamName)
	resource := &resources.Resource{
		Kind: resources.KindGeminiFile, APIKeyID: owner.APIKeyID, RouteSlug: owner.RouteSlug,
		ProviderID: owner.ProviderID, ProviderRevisionID: owner.ProviderRevisionID,
		RouteRevisionID: owner.RouteRevisionID, TargetID: owner.TargetID, SlotID: owner.SlotID,
		CredentialID: owner.CredentialID, UpstreamID: upstreamID, State: "processed",
		Metadata: kernel.MustJSON(metadata), ParentID: new(owner.UUID),
	}
	if owner.ExpiresAt != nil {
		expires := *owner.ExpiresAt
		resource.ExpiresAt = &expires
	}
	commitCtx, stop := kernel.ResourceCommitContext(ctx)
	defer stop()
	created, err := s.kernel.Deps.Resources.Put(commitCtx, resource)
	if err == nil {
		return created, nil
	}
	if existing, readErr := s.kernel.Deps.Resources.GetByUpstreamIncludingDeleted(commitCtx, identity); readErr == nil {
		return existing, nil
	}
	return nil, err
}

func (s surface) geminiBatchCall(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, op func(context.Context, *kernel.Call, keyauthority.Authority, *resources.Resource, *kernel.Pin) *fault.Error) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	localID := strings.TrimPrefix(r.PathValue("name"), "batches/")
	resource, err := s.kernel.Deps.Resources.Get(r.Context(), resources.KindGeminiBatch, authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No Gemini batch with this name exists for this key."))
		return
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini batch mapping could not be read."))
		return
	}
	p, route, pinErr := s.kernel.ResolveResource(r.Context(), x, authority, resource, "batch", use)
	if pinErr != nil {
		s.kernel.Refuse(x, w, pinErr)
		return
	}
	if !p.Provider.Connector().Supports("batch", "gemini", "unary") || !p.Provider.Supports(p.Model, "batch", "gemini", "unary") {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No Gemini batch with this name exists for this key."))
		return
	}
	if route.Fidelity.Strict() {
		s.kernel.Refuse(x, w, kernel.PolicyUnavailable("target_capability", "Strict routes do not serve Gemini batches."))
		return
	}
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, false); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	if err := op(ctx, x, authority, resource, p); err != nil {
		s.kernel.Refuse(x, w, err)
	}
}

func (s surface) geminiBatchResourceObject(ctx context.Context, resource *resources.Resource, body []byte) ([]byte, error) {
	names, err := geminiBatchInputNames(resource)
	if err != nil {
		return nil, kernel.ErrGeminiBatchFidelity
	}
	batch, err := upstream.DecodeGeminiBatch(body)
	if err != nil {
		return nil, kernel.ErrGeminiBatchFidelity
	}
	object, err := prepareGeminiBatchObject(body, batch, names)
	if err != nil {
		return nil, err
	}
	return s.projectGeminiBatchResource(ctx, resource, object)
}

func (s surface) projectGeminiBatchResource(ctx context.Context, resource *resources.Resource, object geminiBatchObject) ([]byte, error) {
	outputs := make(map[string]string, len(object.outputs))
	for _, file := range object.outputs {
		mapped, err := s.mapUpstreamGeminiFile(ctx, resource, file)
		if err != nil {
			return nil, err
		}
		outputs[file] = "files/" + mapped.ID
	}
	return object.project(resource.ID, resource.RouteSlug, outputs)
}

func (s surface) geminiListBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	pageSize, pageToken, cursorErr := geminiBatchPageCursor(r.URL.Query())
	if cursorErr != nil {
		s.kernel.Refuse(x, w, cursorErr)
		return
	}
	rows, continuation, listErr := s.kernel.ListedResourcePage(authority.ID, "pageToken", pageSize, pageToken, func(cursor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
		batch, err := s.kernel.Deps.Resources.ListKindsPage(r.Context(), []string{resources.KindGeminiBatch}, authority.ID, count, cursor, false)
		if errors.Is(err, resources.ErrNotFound) {
			param := "pageToken"
			return nil, fault.InvalidRequest("invalid_value", "pageToken must identify a Gemini batch owned by this key.", &param)
		}
		if err != nil {
			return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini batch list could not be read.")
		}
		return batch, nil
	}, func(row *resources.Resource) *fault.Error {
		return s.kernel.AuthorizeListedResource(r.Context(), authority, row)
	})
	if listErr != nil {
		s.kernel.Refuse(x, w, listErr)
		return
	}
	operations := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		operation, projectErr := s.geminiBatchResourceObject(r.Context(), row, row.Metadata)
		if projectErr != nil {
			s.kernel.Refuse(x, w, geminiBatchProjectionError(projectErr))
			return
		}
		operations = append(operations, operation)
	}
	response := map[string]any{"operations": operations}
	if continuation != "" {
		response["nextPageToken"] = continuation
	}
	s.kernel.WriteStateJSON(w, x, kernel.MustJSON(response))
}

func (s surface) geminiBatchRequest(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, method string, cancel bool) {
	s.geminiBatchCall(w, r, use, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, resource *resources.Resource, p *kernel.Pin) *fault.Error {
		path := "batches/" + url.PathEscape(resource.UpstreamID)
		if cancel {
			path += ":cancel"
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, path, nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved.")
		}
		response, failure := s.kernel.PinnedDo(ctx, x, p, method, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.kernel.Deps.Resources.Tombstone(ctx, resource.ID)
				return fault.NotFoundError("not_found", "No Gemini batch with this name exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		defer response.Body.Close()
		body, err := response.ReadBody(s.kernel.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch response could not be read.")
		}
		if cancel && geminiBatchCancelAcknowledgement(body) {
			response.Exchange.Complete(kernel.ClassSuccess)

			projected, projectErr := s.geminiBatchResourceObject(ctx, resource, resource.Metadata)
			if projectErr != nil {
				return geminiBatchProjectionError(projectErr)
			}
			s.kernel.WriteStateJSON(w, x, projected, response.Exchange)
			return nil
		}
		batch, decodeErr := upstream.DecodeGeminiBatch(body)
		if decodeErr != nil || batch.ID != resource.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini batch identity.")
		}
		names, namesErr := geminiBatchInputNames(resource)
		if namesErr != nil {
			response.Exchange.Interrupt(kernel.ClassProtocol, true)
			return geminiBatchProjectionError(kernel.ErrGeminiBatchFidelity)
		}
		object, metadata, metadataErr := settleGeminiBatchResult(response.Exchange, body, batch, kernel.ResourceModel(resource), names)
		if metadataErr != nil {
			return geminiBatchProjectionError(kernel.ErrGeminiBatchFidelity)
		}
		if err := s.kernel.Deps.Resources.Update(ctx, resource.ID, batch.State, metadata, nil); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch refresh could not be committed.")
		}

		projected, projectErr := s.projectGeminiBatchResource(ctx, resource, object)
		if projectErr != nil {
			return geminiBatchProjectionError(projectErr)
		}
		s.kernel.WriteStateJSON(w, x, projected, response.Exchange)
		return nil
	})
}

func (s surface) geminiGetBatch(w http.ResponseWriter, r *http.Request) {
	s.geminiBatchRequest(w, r, kernel.RetainedRetrieval, http.MethodGet, false)
}

func (s surface) geminiBatchCancel(w http.ResponseWriter, r *http.Request) {
	localID, action, ok := strings.Cut(r.PathValue("action"), ":")
	if !ok || action != "cancel" || localID == "" {
		x, _, done := s.kernel.StateBegin(w, r, wire.GeminiBatch)
		if !done {
			defer s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "Unknown Gemini batch action."))
		}
		return
	}
	r.SetPathValue("name", localID)
	s.geminiBatchRequest(w, r, kernel.RetainedHousekeeping, http.MethodPost, true)
}

func (s surface) geminiDeleteBatch(w http.ResponseWriter, r *http.Request) {
	s.geminiBatchCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, resource *resources.Resource, p *kernel.Pin) *fault.Error {
		if err := s.kernel.RequireBatchEvidenceSettled(ctx, resource.UUID); err != nil {
			return err
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "batches/"+url.PathEscape(resource.UpstreamID), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved.")
		}
		response, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil && failure.Status != http.StatusNotFound {
			return kernel.UpstreamError(failure)
		}
		if response != nil {
			_ = response.Body.Close()
			response.Exchange.Complete(kernel.ClassSuccess)
		}
		if err := s.kernel.Deps.Resources.Tombstone(ctx, resource.ID); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch mapping could not be deleted.")
		}

		if response == nil {
			s.kernel.WriteStateJSON(w, x, []byte(`{}`))
			return nil
		}
		s.kernel.WriteStateJSON(w, x, []byte(`{}`), response.Exchange)
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
	if token != "" && !strings.HasPrefix(token, kernel.RetainedListCursorPrefix) && !validGeminiResourceID(token, resources.KindGeminiBatch) {
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
