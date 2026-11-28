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
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
)

type batchInput struct {
	Canonical    []byte
	Upstream     []byte
	Expected     *int64
	Keys         []string
	LocalFileID  string
	UpstreamFile string
	DisplayName  *string
}

func validateBatch(body []byte, slug string) (batchInput, *fault.Error) {
	batch, err := requestedBatch(body)
	if err != nil {
		return batchInput{}, err
	}
	displayName, err := batchDisplayName(batch)
	if err != nil {
		return batchInput{}, err
	}
	source, err := batchSource(batch)
	if err != nil {
		return batchInput{}, err
	}
	localFileID, upstreamFile, err := sourceFile(source)
	if err != nil {
		return batchInput{}, err
	}
	keys, expected, err := sourceKeys(source, slug)
	if err != nil {
		return batchInput{}, err
	}
	canonical, canonicalErr := kernel.CanonicalBatchJSON(body)
	if canonicalErr != nil {
		return batchInput{}, fault.InvalidRequest("invalid_request", "The batch request is invalid JSON.", nil)
	}
	upstream, upstreamErr := json.Marshal(map[string]any{"batch": upstreamBatch(displayName, source, upstreamFile)})
	if upstreamErr != nil {
		return batchInput{}, fault.InvalidRequest("invalid_request", "The batch request could not be translated.", nil)
	}
	return batchInput{
		Canonical: canonical, Upstream: upstream, Expected: expected, Keys: keys,
		LocalFileID: localFileID, UpstreamFile: upstreamFile, DisplayName: displayName,
	}, nil
}

func requestedBatch(body []byte) (oif.Value, *fault.Error) {
	doc, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil || doc.Root().Kind() != oif.Object || !onlyFields(doc.Root(), "batch") {
		return oif.Value{}, fault.InvalidRequest("invalid_request", "The request must contain only a batch object.", nil)
	}
	batch, present := doc.Root().Lookup("batch")
	if !present || batch.Kind() != oif.Object {
		return oif.Value{}, fault.InvalidRequest("invalid_request", "batch must be an object.", nil)
	}
	if !onlyFields(batch, "display_name", "displayName", "input_config", "inputConfig") {
		return oif.Value{}, fault.InvalidRequest("invalid_request", "The batch contains an unsupported field.", nil)
	}
	return batch, nil
}

func batchDisplayName(batch oif.Value) (*string, *fault.Error) {
	raw, present, err := aliased(batch, "display_name", "displayName")
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", "The batch display name is invalid.", nil)
	}
	name, err := readDisplayName(raw, present)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", "displayName must be a string no longer than 512 bytes.", nil)
	}
	return name, nil
}

func batchSource(batch oif.Value) (oif.Member, *fault.Error) {
	input, present, err := aliased(batch, "input_config", "inputConfig")
	if err != nil || !present {
		return oif.Member{}, fault.InvalidRequest("invalid_request", "The batch must contain exactly one input_config.", nil)
	}
	doc, err := oif.ParseJSON(input, oif.Limits{})
	if err != nil || doc.Root().Kind() != oif.Object {
		return oif.Member{}, fault.InvalidRequest("invalid_request", "input_config must be an object.", nil)
	}
	members := doc.Root().Members()
	if len(members) != 1 || !inlineSource(members[0]) && !fileSource(members[0]) {
		return oif.Member{}, fault.InvalidRequest("invalid_request", "input_config must contain requests or one file name.", nil)
	}
	return members[0], nil
}

func inlineSource(source oif.Member) bool {
	return source.Name == "requests"
}

func fileSource(source oif.Member) bool {
	return source.Name == "file_name" || source.Name == "fileName"
}

func sourceFile(source oif.Member) (string, string, *fault.Error) {
	if !fileSource(source) {
		return "", "", nil
	}
	var name string
	if json.Unmarshal([]byte(source.Value.Raw()), &name) != nil {
		return "", "", fault.InvalidRequest("invalid_request", "The input file name is invalid.", nil)
	}
	localID, ok := strings.CutPrefix(name, "files/")
	if !ok || !kernel.ValidGeminiFileID(localID) {
		return "", "", fault.InvalidRequest("invalid_request", "The input file name must identify a local Gemini file.", nil)
	}
	return localID, name, nil
}

func sourceKeys(source oif.Member, slug string) ([]string, *int64, *fault.Error) {
	if !inlineSource(source) {
		return nil, nil, nil
	}
	wrapper := source.Value
	if wrapper.Kind() != oif.Object || len(wrapper.Members()) != 1 || wrapper.Members()[0].Name != "requests" {
		return nil, nil, fault.InvalidRequest("invalid_request", "input_config.requests must contain only requests.", nil)
	}
	list := wrapper.Members()[0].Value
	if list.Kind() != oif.Array || len(list.Elements()) < 1 || len(list.Elements()) > 100000 {
		return nil, nil, fault.InvalidRequest("invalid_request", "A Gemini batch must contain between 1 and 100000 requests.", nil)
	}
	seen := map[string]struct{}{}
	keys := make([]string, 0, len(list.Elements()))
	for _, item := range list.Elements() {
		key, err := itemKey(item, slug, seen)
		if err != nil {
			return nil, nil, err
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	count := int64(len(keys))
	return keys, &count, nil
}

func itemKey(item oif.Value, slug string, seen map[string]struct{}) (string, *fault.Error) {
	if item.Kind() != oif.Object || !onlyFields(item, "request", "metadata") {
		return "", fault.InvalidRequest("invalid_request", "Each inline batch item must contain only request and metadata.", nil)
	}
	request, requestPresent := item.Lookup("request")
	metadata, metadataPresent := item.Lookup("metadata")
	if !requestPresent || !metadataPresent || metadata.Kind() != oif.Object || !onlyFields(metadata, "key") {
		return "", fault.InvalidRequest("invalid_request", "Each inline batch item requires request and metadata.key.", nil)
	}
	keyValue, keyPresent := metadata.Lookup("key")
	var key string
	if !keyPresent || json.Unmarshal([]byte(keyValue.Raw()), &key) != nil || key == "" || len(key) > 128 {
		return "", fault.InvalidRequest("invalid_request", "metadata.key must be a non-empty string of at most 128 bytes.", nil)
	}
	if _, exists := seen[key]; exists {
		return "", fault.InvalidRequest("invalid_request", "metadata.key values must be unique.", nil)
	}
	if err := validateItem(request, slug); err != nil {
		return "", err
	}
	return key, nil
}

func upstreamBatch(displayName *string, source oif.Member, upstreamFile string) map[string]any {
	if displayName == nil {
		return map[string]any{"inputConfig": upstreamInput(source, upstreamFile)}
	}
	return map[string]any{"displayName": *displayName, "inputConfig": upstreamInput(source, upstreamFile)}
}

func upstreamInput(source oif.Member, upstreamFile string) map[string]any {
	if inlineSource(source) {
		return map[string]any{"requests": json.RawMessage(source.Value.Raw())}
	}
	return map[string]any{"fileName": upstreamFile}
}

func validateItem(request oif.Value, slug string) *fault.Error {
	if request.Kind() != oif.Object {
		return fault.InvalidRequest("invalid_request", "A batch request must be an object.", nil)
	}
	if _, err := protocols.Parse(wire.Gemini, []byte(request.Raw()), slug); err != nil {
		return fault.InvalidRequest("invalid_request", "A batch request is not a valid Gemini GenerateContent request.", nil)
	}
	if managedbatch.GeminiBatchHasFileReference(request) {
		return fault.InvalidRequest("unsupported_parameter", "Gemini batches do not support unbound file or cached-content references.", nil)
	}
	return nil
}

func qualifyJSONL(input io.Reader, slug string, digest func(string) ([]byte, bool)) (managedbatch.Manifest, error) {
	manifest := managedbatch.Manifest{Adapter: managedbatch.GeminiNativeManifest, LineOperation: wire.LineGeneration}
	seen := map[[32]byte]struct{}{}
	hash := sha256.New()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), batches.MaxLineBytes+1)
	for scanner.Scan() {
		if manifest.LineCount >= 100000 {
			return managedbatch.Manifest{}, errors.New("Gemini batch input exceeds its request limit")
		}
		document, err := oif.ParseJSON(scanner.Bytes(), oif.Limits{MaxBytes: batches.MaxLineBytes})
		if err != nil || document.Root().Kind() != oif.Object || !onlyFields(document.Root(), "key", "request") {
			return managedbatch.Manifest{}, errors.New("invalid Gemini batch JSONL item")
		}
		keyValue, ok := document.Root().Lookup("key")
		key, valid := keyValue.Text()
		if !ok || !valid || key == "" || len(key) > 128 {
			return managedbatch.Manifest{}, errors.New("invalid Gemini batch item key")
		}
		request, present := document.Root().Lookup("request")
		if !present {
			return managedbatch.Manifest{}, errors.New("Gemini batch item has no request")
		}
		if err := validateItem(request, slug); err != nil {
			return managedbatch.Manifest{}, err
		}
		identity, ok := digest(key)
		if !ok || len(identity) != 32 {
			return managedbatch.Manifest{}, errors.New("Gemini batch identity is unavailable")
		}
		var identityKey [32]byte
		copy(identityKey[:], identity)
		if _, duplicate := seen[identityKey]; duplicate {
			return managedbatch.Manifest{}, errors.New("duplicate Gemini batch item key")
		}
		seen[identityKey] = struct{}{}
		hash.Write(identity)
		manifest.LineCount++
	}
	if scanner.Err() != nil {
		return managedbatch.Manifest{}, scanner.Err()
	}
	if manifest.LineCount == 0 {
		return managedbatch.Manifest{}, errors.New("Gemini batch input is empty")
	}
	manifest.ManifestDigest = hex.EncodeToString(hash.Sum(nil))
	return manifest, nil
}

func onlyFields(value oif.Value, allowed ...string) bool {
	for _, member := range value.Members() {
		if !slices.Contains(allowed, member.Name) {
			return false
		}
	}
	return true
}

func aliased(object oif.Value, snake, camel string) (json.RawMessage, bool, error) {
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

func readDisplayName(raw json.RawMessage, present bool) (*string, error) {
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

func rewriteInputFile(body []byte, upstreamName string) ([]byte, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil, managedbatch.ErrGeminiBatchFidelity
	}
	var batch map[string]json.RawMessage
	if json.Unmarshal(root["batch"], &batch) != nil {
		return nil, managedbatch.ErrGeminiBatchFidelity
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(batch["inputConfig"], &input) != nil {
		return nil, managedbatch.ErrGeminiBatchFidelity
	}
	input["fileName"] = kernel.MustJSON(upstreamName)
	batch["inputConfig"] = kernel.MustJSON(input)
	root["batch"] = kernel.MustJSON(batch)
	return json.Marshal(root)
}
