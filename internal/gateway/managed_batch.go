package gateway

import (
	"bufio"
	"cmp"
	"context"
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
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	managedBatchLineLimit = 1 << 20
	managedBatchLines     = 50000
)

const geminiNativeManifest connectors.BatchAdapter = "gemini_native"

type batchManifest struct {
	Adapter        connectors.BatchAdapter `json:"adapter,omitempty"`
	Endpoint       string                  `json:"endpoint,omitempty"`
	LineOperation  wire.LineOperation      `json:"line_operation,omitempty"`
	LineCount      int                     `json:"line_count,omitempty"`
	ManifestDigest string                  `json:"manifest_digest,omitempty"`
}

func (m batchManifest) withAdapter(adapter connectors.BatchAdapter) batchManifest {
	m.Adapter = adapter
	return m
}

type managedFileObject struct {
	Bytes     *upstream.Count `json:"bytes,omitempty"`
	CreatedAt *int64          `json:"created_at,omitempty"`
	Filename  *string         `json:"filename,omitempty"`
	Purpose   *string         `json:"purpose,omitempty"`
}

type managedFileRecord struct {
	batchManifest
	UpstreamModel    string            `json:"upstream_model,omitempty"`
	Role             string            `json:"role,omitempty"`
	ProviderFile     managedFileObject `json:"provider_file"`
	ResponsesFile    string            `json:"gemini_responses_file,omitempty"`
	CompletionWindow string            `json:"completion_window,omitempty"`
}

func decodeManagedFileRecord(metadata json.RawMessage) (managedFileRecord, error) {
	var record managedFileRecord
	if err := json.Unmarshal(metadata, &record); err != nil {
		return managedFileRecord{}, errGeminiBatchFidelity
	}
	return record, nil
}

func resourceAdapter(resource *resources.Resource) (connectors.BatchAdapter, error) {
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

func managedFileMetadata(prior json.RawMessage, response []byte, adapter connectors.BatchAdapter, manifest batchManifest, role string) (json.RawMessage, error) {
	record, err := decodeManagedFileRecord(prior)
	if err != nil {
		return nil, err
	}
	file, err := decodeManagedProviderFile(adapter, response)
	if err != nil {
		return nil, err
	}
	if manifest.Adapter != "" {
		return json.Marshal(managedFileRecord{batchManifest: manifest.withAdapter(adapter), UpstreamModel: record.UpstreamModel, Role: role, ProviderFile: file})
	}
	return json.Marshal(managedFileRecord{
		batchManifest: record.batchManifest.withAdapter(adapter), UpstreamModel: record.UpstreamModel, Role: role, ProviderFile: file,
		ResponsesFile: record.ResponsesFile,
	})
}

func decodeManagedProviderFile(adapter connectors.BatchAdapter, response []byte) (managedFileObject, error) {
	if len(response) == 0 {
		return managedFileObject{}, nil
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
		return managedFileObject{}, err
	}
	return managedFileObject{Bytes: file.Bytes, CreatedAt: optionalUnix(file.CreatedAt), Filename: file.Filename, Purpose: managedPurpose(adapter, file.Purpose)}, nil
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

func decodeGeminiProviderFile(response []byte) (managedFileObject, error) {
	var envelope struct {
		File *geminiFileFields `json:"file"`
		geminiFileFields
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return managedFileObject{}, err
	}
	file := cmp.Or(envelope.File, &envelope.geminiFileFields)
	return managedFileObject{
		Bytes: file.SizeBytes, CreatedAt: optionalUnix(file.CreateTime),
		Filename: new(cmp.Or(file.DisplayName, strings.TrimPrefix(file.Name, "files/"))), Purpose: new("batch"),
	}, nil
}

type managedBatchLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

type translatedBatch struct {
	manifest batchManifest
	err      error
}

func (s *Server) translateManagedBatchInput(ctx context.Context, p *pin, route *routing.Route, file *media.Part, adapter connectors.BatchAdapter) (*media.Part, batchManifest, *Error) {
	opened, err := s.transport().Spool.Open(file.Handle)
	if err != nil {
		return nil, batchManifest{}, serverError(http.StatusInternalServerError, "media_content_unavailable", "The uploaded batch file could not be read.")
	}
	defer opened.File.Close()
	reader, writer := io.Pipe()
	done := make(chan translatedBatch, 1)
	go func() {
		manifest, err := translateManagedBatchLines(writer, opened.File, s.batchLineDigest, managedLineTarget{pin: p, route: route, adapter: adapter})
		done <- translatedBatch{manifest: manifest, err: err}
	}()
	artifact, spoolErr := s.transport().Spool.Put(ctx, media.Upload{
		Filename: file.Filename, ContentType: "application/jsonl", MaximumLength: file.Limit, Body: reader,
	})
	_ = reader.Close()
	translated := <-done
	if spoolErr != nil || translated.err != nil {
		if spoolErr == nil {
			_ = s.transport().Spool.Remove(artifact.Handle)
		}
		if failure, ok := translated.err.(*Error); ok {
			return nil, batchManifest{}, failure
		}
		return nil, batchManifest{}, invalidRequest("unsupported_parameter", "The batch file could not be translated.", strPtr("file"))
	}
	part := &media.Part{
		Handle: artifact.Handle, Digest: artifact.Digest, Filename: file.Filename,
		ContentType: artifact.ContentType, Size: artifact.ContentLength, Limit: file.Limit,
	}
	return part, translated.manifest, nil
}

type managedLineTarget struct {
	pin     *pin
	route   *routing.Route
	adapter connectors.BatchAdapter
}

func batchFileError(format string, lineNumber int) *Error {
	return invalidRequest("unsupported_parameter", fmt.Sprintf(format, lineNumber), strPtr("file"))
}

func translateManagedBatchLines(writer *io.PipeWriter, input io.Reader, digest func(string) ([]byte, bool), target managedLineTarget) (batchManifest, error) {
	fail := func(err error) (batchManifest, error) {
		_ = writer.CloseWithError(err)
		return batchManifest{}, err
	}
	defer writer.Close()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), managedBatchLineLimit+2)
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
		return fail(invalidRequest("unsupported_parameter", "The batch file must contain at least one request.", strPtr("file")))
	}
	operation, _ := wire.BatchLineOperation(endpoint)
	return batchManifest{
		Adapter: target.adapter, Endpoint: endpoint, LineOperation: operation, LineCount: count,
		ManifestDigest: hex.EncodeToString(manifestHash.Sum(nil)),
	}, nil
}

func (t managedLineTarget) translate(raw []byte, lineNumber int) (managedBatchLine, []byte, error) {
	if lineNumber > managedBatchLines || len(raw) > managedBatchLineLimit {
		return managedBatchLine{}, nil, batchFileError("Batch file line %d exceeds the supported bounds.", lineNumber)
	}
	document, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: managedBatchLineLimit})
	if err != nil || document.Root().Kind() != oif.Object {
		return managedBatchLine{}, nil, batchFileError("Batch file line %d is not a JSON object.", lineNumber)
	}
	for _, member := range document.Root().Members() {
		if !slices.Contains([]string{"custom_id", "method", "url", "body"}, member.Name) {
			return managedBatchLine{}, nil, batchFileError("Batch file line %d has an unsupported field.", lineNumber)
		}
	}
	var line managedBatchLine
	if json.Unmarshal(raw, &line) != nil || line.CustomID == "" || len(line.CustomID) > managedCustomIDLimit(t.adapter) ||
		line.Method != "POST" || len(line.Body) == 0 {
		return managedBatchLine{}, nil, batchFileError("Batch file line %d has an unsupported shape.", lineNumber)
	}
	classified := line.URL
	if t.adapter == connectors.BatchOpenAI && t.pin.provider.Kind == "azure_openai" {
		classified = wire.CanonicalBatchEndpoint(line.URL)
	}
	operation, family, ok := managedBatchEndpoint(t.adapter, classified)
	if !ok {
		return managedBatchLine{}, nil, batchFileError("Batch file line %d has an unsupported or mixed endpoint.", lineNumber)
	}
	var body struct {
		Model string `json:"model"`
	}
	upstreamModel := t.pin.provider.Connector().Model(t.pin.model)
	if json.Unmarshal(line.Body, &body) != nil || body.Model != t.route.Slug && body.Model != t.pin.model && body.Model != upstreamModel {
		return managedBatchLine{}, nil, batchFileError("Batch file line %d has an unsupported model.", lineNumber)
	}
	if t.adapter == connectors.BatchOpenAI {
		encoded, err := t.bindOpenAILine(document, line, upstreamModel)
		return line, encoded, err
	}
	encoded, err := t.encodeLine(line, operation, family, upstreamModel, lineNumber)
	return line, encoded, err
}

func (t managedLineTarget) bindOpenAILine(document oif.Document, line managedBatchLine, upstreamModel string) ([]byte, error) {
	if t.pin.provider.VendorID == "groq" && line.URL != "/v1/chat/completions" {
		return nil, invalidRequest("unsupported_parameter", "Groq batch files support only /v1/chat/completions.", strPtr("file"))
	}
	bound, err := oif.Apply(document, []oif.Change{{Pointer: "/body/model", Value: string(mustJSON(upstreamModel)), Origin: oif.ResourceBinding, Reason: "batch model affinity"}})
	if err != nil {
		return nil, err
	}
	return bound.Bytes(), nil
}

func (t managedLineTarget) encodeLine(line managedBatchLine, operation wire.LineOperation, family wire.Format, upstreamModel string, lineNumber int) ([]byte, error) {
	untranslatable := batchFileError("Batch file line %d cannot be translated.", lineNumber)
	bodyDocument, err := oif.ParseJSON(line.Body, oif.Limits{MaxBytes: managedBatchLineLimit})
	if err != nil {
		return nil, err
	}
	ingress, err := oif.Apply(bodyDocument, []oif.Change{{Pointer: "/model", Value: string(mustJSON(t.route.Slug)), Origin: oif.IdentityBinding, Reason: "batch ingress model alias"}})
	if err != nil {
		return nil, err
	}
	request, err := protocols.Parse(family, ingress.Bytes(), t.route.Slug)
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
	if t.adapter == connectors.BatchGeminiManaged {
		document, err := oif.ParseJSON(encoded, oif.Limits{MaxBytes: managedBatchLineLimit})
		if err != nil || geminiBatchHasFileReference(document.Root()) {
			return nil, invalidRequest("unsupported_parameter", "Gemini batch input cannot use unbound file or cached-content references.", strPtr("file"))
		}
	}
	encodedRequest, err := t.withoutModel(encoded)
	if err != nil {
		return nil, err
	}
	if t.adapter == connectors.BatchGeminiManaged {
		return json.Marshal(map[string]any{"key": line.CustomID, "request": encodedRequest})
	}
	return json.Marshal(map[string]any{"custom_id": line.CustomID, "body": encodedRequest})
}

func (t managedLineTarget) lineDefaults(operation wire.LineOperation) (protocols.Object, error) {
	if t.pin.provider.ProfileID == "" {
		return protocols.Object(t.pin.provider.ParameterDefaults), nil
	}
	defaults, _, err := t.pin.provider.Connector().DefaultsFor(string(operation), t.pin.model)
	return defaults, err
}

func (t managedLineTarget) encodeRequest(request *wire.Request, upstreamModel string, defaults protocols.Object) ([]byte, error) {
	provider := t.pin.provider
	if t.adapter == connectors.BatchGeminiManaged {
		encoded, _, err := protocols.EncodeTarget(request, wire.Gemini, provider.Kind, provider.VendorID, upstreamModel, defaults)
		return encoded, err
	}
	encoded, _, err := protocols.Encode(request, provider.Kind, provider.VendorID, upstreamModel, defaults)
	return encoded, err
}

func (t managedLineTarget) withoutModel(encoded []byte) (json.RawMessage, error) {
	if t.adapter != connectors.BatchMistral && t.adapter != connectors.BatchGeminiManaged {
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

func managedCustomIDLimit(adapter connectors.BatchAdapter) int {
	if adapter == connectors.BatchTogether {
		return 64
	}
	return 128
}

func managedBatchEndpoint(adapter connectors.BatchAdapter, endpoint string) (wire.LineOperation, wire.Format, bool) {
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

func (s *Server) batchLineDigest(customID string) ([]byte, bool) {
	digester, ok := s.deps.Runtime.(interface {
		Digest(secrets.DigestPurpose, string) []byte
	})
	if !ok {
		return nil, false
	}
	return digester.Digest(secrets.BatchLineDigest, customID), true
}

func managedBatchRequestBody(source oif.Document, adapter connectors.BatchAdapter, endpoint string, window batchWindow, upstreamFileID, model string, manifest batchManifest, id uuid.UUID) ([]byte, *Error) {
	metadata, err := batchClientMetadata(source)
	if adapter == connectors.BatchGeminiManaged {
		if err != nil {
			return nil, invalidRequest("invalid_request", "metadata must be a string map.", strPtr("metadata"))
		}
		_, windowSupplied := source.Root().Lookup("completion_window")
		if !windowSupplied || window.label != "24h" || endpoint != manifest.Endpoint || manifest.LineOperation != wire.LineGeneration ||
			manifest.LineCount < 1 || upstreamFileID == "" {
			return nil, invalidRequest("invalid_request", "Gemini managed batches require a matching generation manifest and completion_window=24h.", nil)
		}
		return marshalBatchBody(geminiManagedBatchBody("olp-"+strings.ReplaceAll(id.String(), "-", ""), upstreamFileID))
	}
	if endpoint != manifest.Endpoint {
		return nil, invalidRequest("unsupported_parameter", "The batch endpoint must match the uploaded file.", strPtr("endpoint"))
	}
	if err != nil {
		return nil, invalidRequest("unsupported_parameter", "Batch metadata must be a string map.", strPtr("metadata"))
	}
	if adapter == connectors.BatchTogether {
		return marshalBatchBody(map[string]any{"endpoint": endpoint, "input_file_id": upstreamFileID, "completion_window": "24h"})
	}
	return marshalBatchBody(mistralBatchBody{
		Endpoint: endpoint, InputFiles: []string{upstreamFileID}, Model: model,
		TimeoutHours: int64(window.length / time.Hour), Metadata: metadata,
	})
}

type mistralBatchBody struct {
	Endpoint     string            `json:"endpoint"`
	InputFiles   []string          `json:"input_files"`
	Model        string            `json:"model"`
	TimeoutHours int64             `json:"timeout_hours"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

func batchClientMetadata(source oif.Document) (map[string]string, error) {
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

func marshalBatchBody(body any) ([]byte, *Error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, serverError(http.StatusInternalServerError, "internal_error", "The batch request could not be encoded.")
	}
	return encoded, nil
}

func geminiManagedBatchBody(displayName, upstreamFileName string) map[string]any {
	return map[string]any{"batch": map[string]any{
		"display_name": displayName,
		"input_config": map[string]string{"file_name": upstreamFileName},
	}}
}
