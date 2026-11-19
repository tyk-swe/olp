package geministate

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
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
