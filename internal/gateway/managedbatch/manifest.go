package managedbatch

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	LineLimit = 1 << 20
	MaxLines  = 50000
)

const GeminiNativeManifest connectors.BatchAdapter = "gemini_native"

type Digester interface {
	Digest(secrets.DigestPurpose, string) []byte
}

func LineDigest(digester Digester) func(string) ([]byte, bool) {
	return func(customID string) ([]byte, bool) {
		if digester == nil {
			return nil, false
		}
		return digester.Digest(secrets.BatchLineDigest, customID), true
	}
}

type Manifest struct {
	Adapter        connectors.BatchAdapter `json:"adapter,omitempty"`
	Endpoint       string                  `json:"endpoint,omitempty"`
	LineOperation  wire.LineOperation      `json:"line_operation,omitempty"`
	LineCount      int                     `json:"line_count,omitempty"`
	ManifestDigest string                  `json:"manifest_digest,omitempty"`
}

func (m Manifest) Usable(adapter connectors.BatchAdapter) bool {
	return m.Adapter == adapter && m.Endpoint != "" && m.LineCount >= 1 && m.ManifestDigest != ""
}

func (m Manifest) WithAdapter(adapter connectors.BatchAdapter) Manifest {
	m.Adapter = adapter
	return m
}

type FileObject struct {
	Bytes     *upstream.Count `json:"bytes,omitempty"`
	CreatedAt *int64          `json:"created_at,omitempty"`
	Filename  *string         `json:"filename,omitempty"`
	Purpose   *string         `json:"purpose,omitempty"`
}

type FileRecord struct {
	Manifest
	UpstreamModel    string     `json:"upstream_model,omitempty"`
	Role             string     `json:"role,omitempty"`
	ProviderFile     FileObject `json:"provider_file"`
	ResponsesFile    string     `json:"gemini_responses_file,omitempty"`
	CompletionWindow string     `json:"completion_window,omitempty"`
}

func DecodeFileRecord(metadata json.RawMessage) (FileRecord, error) {
	var record FileRecord
	if err := json.Unmarshal(metadata, &record); err != nil {
		return FileRecord{}, ErrGeminiBatchFidelity
	}
	return record, nil
}

func ResourceAdapter(resource *resources.Resource) (connectors.BatchAdapter, error) {
	var metadata struct {
		Adapter string `json:"adapter"`
	}
	if err := json.Unmarshal(resource.Metadata, &metadata); err != nil {
		return "", err
	}
	if metadata.Adapter == "" {
		return connectors.BatchOpenAI, nil
	}
	return connectors.ParseBatchAdapter(metadata.Adapter)
}

var ErrProviderFileMetadata = errors.New("invalid provider file metadata")

func FileMetadata(prior json.RawMessage, response []byte, adapter connectors.BatchAdapter, manifest Manifest, role string) (json.RawMessage, error) {
	record, err := DecodeFileRecord(prior)
	if err != nil {
		return nil, err
	}
	file, err := decodeProviderFile(adapter, response)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProviderFileMetadata, err)
	}
	if manifest.Adapter != "" {
		return json.Marshal(FileRecord{Manifest: manifest.WithAdapter(adapter), UpstreamModel: record.UpstreamModel, Role: role, ProviderFile: file})
	}
	return json.Marshal(FileRecord{
		Manifest: record.Manifest.WithAdapter(adapter), UpstreamModel: record.UpstreamModel, Role: role, ProviderFile: file,
		ResponsesFile: record.ResponsesFile,
	})
}

func decodeProviderFile(adapter connectors.BatchAdapter, response []byte) (FileObject, error) {
	if len(response) == 0 {
		return FileObject{}, nil
	}
	if adapter == connectors.BatchGeminiManaged {
		return decodeGeminiProviderFile(response)
	}
	var file struct {
		Bytes     *upstream.Count `json:"bytes"`
		CreatedAt json.RawMessage `json:"created_at"`
		Filename  *string         `json:"filename"`
		Purpose   *string         `json:"purpose"`
	}
	if err := json.Unmarshal(response, &file); err != nil {
		return FileObject{}, err
	}
	return FileObject{Bytes: file.Bytes, CreatedAt: optionalUnix(file.CreatedAt), Filename: file.Filename, Purpose: managedPurpose(adapter, file.Purpose)}, nil
}

func managedPurpose(adapter connectors.BatchAdapter, purpose *string) *string {
	if adapter == connectors.BatchTogether && purpose != nil && strings.HasPrefix(*purpose, "batch") {
		return new("batch")
	}
	return purpose
}

type geminiFileFields struct {
	DisplayName string          `json:"displayName"`
	Name        string          `json:"name"`
	SizeBytes   *upstream.Count `json:"sizeBytes"`
	CreateTime  json.RawMessage `json:"createTime"`
}

func decodeGeminiProviderFile(response []byte) (FileObject, error) {
	var envelope struct {
		File *geminiFileFields `json:"file"`
		geminiFileFields
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return FileObject{}, err
	}
	file := cmp.Or(envelope.File, &envelope.geminiFileFields)
	return FileObject{
		Bytes: file.SizeBytes, CreatedAt: optionalUnix(file.CreateTime),
		Filename: new(cmp.Or(file.DisplayName, strings.TrimPrefix(file.Name, "files/"))), Purpose: new("batch"),
	}, nil
}

type batchLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

type LineTarget struct {
	Provider            routing.Provider
	Model               string
	Route               *routing.Route
	Adapter             connectors.BatchAdapter
	CheckOpenAIEndpoint func(endpoint, pointer string) *fault.Error
}

func batchFileError(format string, lineNumber int) *fault.Error {
	return fault.InvalidRequest("unsupported_parameter", fmt.Sprintf(format, lineNumber), new("file"))
}

func TranslateLines(writer *io.PipeWriter, input io.Reader, digest func(string) ([]byte, bool), target LineTarget) (Manifest, error) {
	fail := func(err error) (Manifest, error) {
		_ = writer.CloseWithError(err)
		return Manifest{}, err
	}
	defer writer.Close()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), LineLimit+2)
	seen := map[string]struct{}{}
	manifestHash := sha256.New()
	endpoint := ""
	count := 0
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line, encoded, err := target.translate(scanner.Bytes(), lineNumber)
		if err != nil {
			return fail(err)
		}
		if _, duplicate := seen[line.CustomID]; duplicate {
			return fail(batchFileError("Batch file line %d repeats a custom_id.", lineNumber))
		}
		if endpoint != "" && endpoint != line.URL {
			return fail(batchFileError("Batch file line %d has an unsupported or mixed endpoint.", lineNumber))
		}
		customIDDigest, ok := digest(line.CustomID)
		if !ok {
			return fail(errors.New("batch digest key is unavailable"))
		}
		seen[line.CustomID], endpoint = struct{}{}, line.URL
		_, _ = manifestHash.Write(customIDDigest)
		if _, err := writer.Write(append(encoded, '\n')); err != nil {
			return fail(err)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return fail(batchFileError("Batch file line %d exceeds the supported bounds.", count+1))
	}
	if count == 0 {
		return fail(fault.InvalidRequest("unsupported_parameter", "The batch file must contain at least one request.", new("file")))
	}
	operation, _ := wire.BatchLineOperation(endpoint)
	return Manifest{
		Adapter: target.Adapter, Endpoint: endpoint, LineOperation: operation, LineCount: count,
		ManifestDigest: hex.EncodeToString(manifestHash.Sum(nil)),
	}, nil
}

func (t LineTarget) translate(raw []byte, lineNumber int) (batchLine, []byte, error) {
	if lineNumber > MaxLines || len(raw) > LineLimit {
		return batchLine{}, nil, batchFileError("Batch file line %d exceeds the supported bounds.", lineNumber)
	}
	document, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: LineLimit})
	if err != nil || document.Root().Kind() != oif.Object {
		return batchLine{}, nil, batchFileError("Batch file line %d is not a JSON object.", lineNumber)
	}
	for _, member := range document.Root().Members() {
		if !slices.Contains([]string{"custom_id", "method", "url", "body"}, member.Name) {
			return batchLine{}, nil, batchFileError("Batch file line %d has an unsupported field.", lineNumber)
		}
	}
	var line batchLine
	if json.Unmarshal(raw, &line) != nil || line.CustomID == "" || len(line.CustomID) > customIDLimit(t.Adapter) ||
		line.Method != "POST" || len(line.Body) == 0 {
		return batchLine{}, nil, batchFileError("Batch file line %d has an unsupported shape.", lineNumber)
	}
	operation, family, ok := batchEndpoint(t.Adapter, t.Provider.Connector().BatchLineEndpoint(line.URL))
	if !ok {
		return batchLine{}, nil, batchFileError("Batch file line %d has an unsupported or mixed endpoint.", lineNumber)
	}
	var body struct {
		Model string `json:"model"`
	}
	upstreamModel := t.Provider.Connector().Model(t.Model)
	if json.Unmarshal(line.Body, &body) != nil || body.Model != t.Route.Slug && body.Model != t.Model && body.Model != upstreamModel {
		return batchLine{}, nil, batchFileError("Batch file line %d has an unsupported model.", lineNumber)
	}
	if t.Adapter == connectors.BatchOpenAI {
		encoded, err := t.bindOpenAILine(document, line, upstreamModel)
		return line, encoded, err
	}
	encoded, err := t.encodeLine(line, operation, family, upstreamModel, lineNumber)
	return line, encoded, err
}

func (t LineTarget) bindOpenAILine(document oif.Document, line batchLine, upstreamModel string) ([]byte, error) {
	if t.CheckOpenAIEndpoint != nil {
		if e := t.CheckOpenAIEndpoint(line.URL, "file"); e != nil {
			return nil, e
		}
	}
	bound, err := oif.Apply(document, []oif.Change{{Pointer: "/body/model", Value: string(mustJSON(upstreamModel)), Origin: oif.ResourceBinding, Reason: "batch model affinity"}})
	if err != nil {
		return nil, err
	}
	return bound.Bytes(), nil
}

func (t LineTarget) encodeLine(line batchLine, operation wire.LineOperation, family wire.Format, upstreamModel string, lineNumber int) ([]byte, error) {
	untranslatable := batchFileError("Batch file line %d cannot be translated.", lineNumber)
	bodyDocument, err := oif.ParseJSON(line.Body, oif.Limits{MaxBytes: LineLimit})
	if err != nil {
		return nil, err
	}
	ingress, err := oif.Apply(bodyDocument, []oif.Change{{Pointer: "/model", Value: string(mustJSON(t.Route.Slug)), Origin: oif.IdentityBinding, Reason: "batch ingress model alias"}})
	if err != nil {
		return nil, err
	}
	request, err := protocols.Parse(family, ingress.Bytes(), t.Route.Slug)
	if err != nil || request.Stream {
		return nil, untranslatable
	}
	defaults, err := t.lineDefaults(operation)
	if err != nil {
		return nil, untranslatable
	}
	encoded, err := t.encodeRequest(request, upstreamModel, defaults)
	if err != nil {
		return nil, untranslatable
	}
	if t.Adapter == connectors.BatchGeminiManaged {
		document, err := oif.ParseJSON(encoded, oif.Limits{MaxBytes: LineLimit})
		if err != nil || GeminiBatchHasFileReference(document.Root()) {
			return nil, fault.InvalidRequest("unsupported_parameter", "Gemini batch input cannot use unbound file or cached-content references.", new("file"))
		}
	}
	encodedRequest, err := t.withoutModel(encoded)
	if err != nil {
		return nil, err
	}
	if t.Adapter == connectors.BatchGeminiManaged {
		return json.Marshal(map[string]any{"key": line.CustomID, "request": encodedRequest})
	}
	return json.Marshal(map[string]any{"custom_id": line.CustomID, "body": encodedRequest})
}

func (t LineTarget) lineDefaults(operation wire.LineOperation) (protocols.Object, error) {
	if t.Provider.ProfileID == "" {
		return t.Provider.ParameterDefaults, nil
	}
	defaults, _, err := t.Provider.Connector().DefaultsFor(string(operation), t.Model)
	return defaults, err
}

func (t LineTarget) encodeRequest(request *wire.Request, upstreamModel string, defaults protocols.Object) ([]byte, error) {
	if t.Adapter == connectors.BatchGeminiManaged {
		encoded, _, err := protocols.EncodeTarget(request, wire.Gemini, string(t.Provider.Kind), t.Provider.VendorID, upstreamModel, defaults)
		return encoded, err
	}
	encoded, _, err := protocols.Encode(request, string(t.Provider.Kind), t.Provider.VendorID, upstreamModel, defaults)
	return encoded, err
}

func (t LineTarget) withoutModel(encoded []byte) (json.RawMessage, error) {
	if t.Adapter != connectors.BatchMistral && t.Adapter != connectors.BatchGeminiManaged {
		return encoded, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, errors.New("translated batch line is malformed")
	}
	return json.Marshal(maps.Collect(func(yield func(string, json.RawMessage) bool) {
		for name, value := range fields {
			if name != "model" && !yield(name, value) {
				return
			}
		}
	}))
}

func customIDLimit(adapter connectors.BatchAdapter) int {
	if adapter == connectors.BatchTogether {
		return 64
	}
	return 128
}

func batchEndpoint(adapter connectors.BatchAdapter, endpoint string) (wire.LineOperation, wire.Format, bool) {
	operation, ok := wire.BatchLineOperation(endpoint)
	if !ok {
		return "", "", false
	}
	switch endpoint {
	case "/v1/chat/completions":
		return operation, wire.Chat, adapter == connectors.BatchOpenAI || adapter.Managed()
	case "/v1/embeddings":
		return operation, wire.Embeddings, adapter == connectors.BatchOpenAI || adapter == connectors.BatchMistral
	case "/v1/responses":
		return operation, wire.Responses, adapter == connectors.BatchOpenAI
	case "/v1/completions":
		return operation, wire.Completions, adapter == connectors.BatchOpenAI
	default:
		return "", "", false
	}
}

type Window struct {
	Label  string
	Length time.Duration
}

func RequestBody(source oif.Document, adapter connectors.BatchAdapter, endpoint string, window Window, upstreamFileID, model string, manifest Manifest, id uuid.UUID) ([]byte, *fault.Error) {
	metadata, err := clientMetadata(source)
	if adapter == connectors.BatchGeminiManaged {
		if err != nil {
			return nil, fault.InvalidRequest("invalid_request", "metadata must be a string map.", new("metadata"))
		}
		_, windowSupplied := source.Root().Lookup("completion_window")
		if !windowSupplied || window.Label != "24h" || endpoint != manifest.Endpoint || manifest.LineOperation != wire.LineGeneration ||
			manifest.LineCount < 1 || upstreamFileID == "" {
			return nil, fault.InvalidRequest("invalid_request", "Gemini managed batches require a matching generation manifest and completion_window=24h.", nil)
		}
		return marshalBatchBody(geminiManagedBatchBody("olp-"+strings.ReplaceAll(id.String(), "-", ""), upstreamFileID))
	}
	if endpoint != manifest.Endpoint {
		return nil, fault.InvalidRequest("unsupported_parameter", "The batch endpoint must match the uploaded file.", new("endpoint"))
	}
	if err != nil {
		return nil, fault.InvalidRequest("unsupported_parameter", "Batch metadata must be a string map.", new("metadata"))
	}
	if adapter == connectors.BatchTogether {
		return marshalBatchBody(map[string]any{"endpoint": endpoint, "input_file_id": upstreamFileID, "completion_window": "24h"})
	}
	return marshalBatchBody(mistralBatchBody{
		Endpoint: endpoint, InputFiles: []string{upstreamFileID}, Model: model,
		TimeoutHours: int64(window.Length / time.Hour), Metadata: metadata,
	})
}

type mistralBatchBody struct {
	Endpoint     string            `json:"endpoint"`
	InputFiles   []string          `json:"input_files"`
	Model        string            `json:"model"`
	TimeoutHours int64             `json:"timeout_hours"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

func clientMetadata(source oif.Document) (map[string]string, error) {
	raw, present := source.Fields()["metadata"]
	if !present {
		return nil, nil
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	if values == nil {
		return nil, errors.New("batch metadata is null")
	}
	return values, nil
}

func marshalBatchBody(body any) ([]byte, *fault.Error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The batch request could not be encoded.")
	}
	return encoded, nil
}

func geminiManagedBatchBody(displayName, upstreamFileName string) map[string]any {
	return map[string]any{"batch": map[string]any{
		"display_name": displayName,
		"input_config": map[string]string{"file_name": upstreamFileName},
	}}
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}
