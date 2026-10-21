package gateway

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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

var managedBatchStatusProjection = map[string]map[string]string{
	"together": {"VALIDATING": "validating", "IN_PROGRESS": "in_progress", "COMPLETED": "completed", "FAILED": "failed", "EXPIRED": "expired", "CANCELLING": "cancelling", "CANCELLED": "cancelled"},
	"mistral":  {"QUEUED": "validating", "RUNNING": "in_progress", "SUCCESS": "completed", "FAILED": "failed", "TIMEOUT_EXCEEDED": "expired", "CANCELLATION_REQUESTED": "cancelling", "CANCELLED": "cancelled"},
	"gemini_managed": {
		"BATCH_STATE_PENDING": "validating", "JOB_STATE_PENDING": "validating", "JOB_STATE_QUEUED": "validating",
		"BATCH_STATE_RUNNING": "in_progress", "JOB_STATE_RUNNING": "in_progress", "JOB_STATE_CANCELLING": "cancelling",
		"BATCH_STATE_SUCCEEDED": "completed", "JOB_STATE_SUCCEEDED": "completed",
		"BATCH_STATE_FAILED": "failed", "JOB_STATE_FAILED": "failed",
		"BATCH_STATE_CANCELLED": "cancelled", "JOB_STATE_CANCELLED": "cancelled",
		"BATCH_STATE_EXPIRED": "expired", "JOB_STATE_EXPIRED": "expired",
	},
}

type batchManifest struct {
	Adapter        string `json:"adapter"`
	Endpoint       string `json:"endpoint"`
	LineOperation  string `json:"line_operation"`
	LineCount      int    `json:"line_count"`
	ManifestDigest string `json:"manifest_digest"`
}

type managedBatchLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

func managedFileMetadata(existing, response []byte, adapter string, manifest batchManifest, role string) []byte {
	var prior map[string]json.RawMessage
	_ = json.Unmarshal(existing, &prior)
	meta := map[string]json.RawMessage{}
	if value, ok := prior["upstream_model"]; ok {
		meta["upstream_model"] = value
	}
	var provider map[string]json.RawMessage
	_ = json.Unmarshal(response, &provider)
	fields := map[string]json.RawMessage{}
	if adapter == "gemini_managed" {
		var file map[string]json.RawMessage
		_ = json.Unmarshal(provider["file"], &file)
		if file == nil {
			file = provider
		}
		var filename string
		_ = json.Unmarshal(file["displayName"], &filename)
		if filename == "" {
			_ = json.Unmarshal(file["name"], &filename)
			filename = strings.TrimPrefix(filename, "files/")
		}
		fields["filename"], _ = json.Marshal(filename)
		fields["purpose"], _ = json.Marshal("batch")
		var sizeText string
		if json.Unmarshal(file["sizeBytes"], &sizeText) == nil {
			if size, err := strconv.ParseInt(sizeText, 10, 64); err == nil && size >= 0 {
				fields["bytes"], _ = json.Marshal(size)
			}
		} else {
			var size int64
			if json.Unmarshal(file["sizeBytes"], &size) == nil && size >= 0 {
				fields["bytes"], _ = json.Marshal(size)
			}
		}
		var created string
		if json.Unmarshal(file["createTime"], &created) == nil {
			if at, err := time.Parse(time.RFC3339Nano, created); err == nil {
				fields["created_at"], _ = json.Marshal(at.Unix())
			}
		}
	} else {
		for _, name := range []string{"bytes", "created_at", "filename", "purpose"} {
			if value, ok := provider[name]; ok {
				fields[name] = value
			}
		}
	}
	if purpose, ok := fields["purpose"]; ok && adapter == "together" {
		var purposeValue string
		if json.Unmarshal(purpose, &purposeValue) == nil && strings.HasPrefix(purposeValue, "batch") {
			fields["purpose"], _ = json.Marshal("batch")
		}
	}
	meta["adapter"], _ = json.Marshal(adapter)
	meta["role"], _ = json.Marshal(role)
	meta["provider_file"], _ = json.Marshal(fields)
	if manifest.Adapter != "" {
		meta["endpoint"], _ = json.Marshal(manifest.Endpoint)
		meta["line_operation"], _ = json.Marshal(manifest.LineOperation)
		meta["line_count"], _ = json.Marshal(manifest.LineCount)
		meta["manifest_digest"], _ = json.Marshal(manifest.ManifestDigest)
	} else {
		for _, name := range []string{"endpoint", "line_operation", "line_count", "manifest_digest", "gemini_responses_file"} {
			if value, ok := prior[name]; ok {
				meta[name] = value
			}
		}
	}
	returnValue, _ := json.Marshal(meta)
	return returnValue
}

func (s *Server) translateManagedBatchInput(ctx context.Context, p *pin, route *routing.Route, file *media.Part, adapter string) (*media.Part, batchManifest, *Error) {
	opened, err := s.transport().Spool.Open(file.Handle)
	if err != nil {
		return nil, batchManifest{}, serverError(http.StatusInternalServerError, "media_content_unavailable", "The uploaded batch file could not be read.")
	}
	defer opened.File.Close()
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	manifest := batchManifest{Adapter: adapter}
	go func() {
		done <- translateManagedBatchLines(writer, opened.File, s.batchLineDigest, p, route, adapter, &manifest)
	}()
	artifact, spoolErr := s.transport().Spool.Put(ctx, media.Upload{
		Filename: file.Filename, ContentType: "application/jsonl", MaximumLength: file.Limit, Body: reader,
	})
	_ = reader.Close()
	translateErr := <-done
	if spoolErr != nil || translateErr != nil {
		if spoolErr == nil {
			_ = s.transport().Spool.Remove(artifact.Handle)
		}
		if failure, ok := translateErr.(*Error); ok {
			return nil, batchManifest{}, failure
		}
		param := "file"
		return nil, batchManifest{}, invalidRequest("unsupported_parameter", "The batch file could not be translated.", &param)
	}
	part := &media.Part{
		Handle: artifact.Handle, Digest: artifact.Digest, Filename: file.Filename,
		ContentType: artifact.ContentType, Size: artifact.ContentLength, Limit: file.Limit,
	}
	return part, manifest, nil
}

func translateManagedBatchLines(writer *io.PipeWriter, input io.Reader, digest func(string) ([]byte, bool), p *pin, route *routing.Route, adapter string, manifest *batchManifest) error {
	fail := func(err error) error {
		_ = writer.CloseWithError(err)
		return err
	}
	defer writer.Close()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), managedBatchLineLimit+2)
	seen := map[string]struct{}{}
	manifestHash := sha256.New()
	var endpoint string
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		raw := scanner.Bytes()
		if lineNumber > managedBatchLines || len(raw) > managedBatchLineLimit {
			param := "file"
			return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d exceeds the supported bounds.", lineNumber), &param))
		}
		document, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: managedBatchLineLimit})
		if err != nil || document.Root().Kind() != oif.Object {
			param := "file"
			return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d is not a JSON object.", lineNumber), &param))
		}
		for _, member := range document.Root().Members() {
			switch member.Name {
			case "custom_id", "method", "url", "body":
			default:
				param := "file"
				return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d has an unsupported field.", lineNumber), &param))
			}
		}
		var line managedBatchLine
		if json.Unmarshal(raw, &line) != nil || line.CustomID == "" || len([]byte(line.CustomID)) > managedCustomIDLimit(adapter) ||
			line.Method != "POST" || len(line.Body) == 0 {
			param := "file"
			return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d has an unsupported shape.", lineNumber), &param))
		}
		if _, duplicate := seen[line.CustomID]; duplicate {
			param := "file"
			return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d repeats a custom_id.", lineNumber), &param))
		}
		seen[line.CustomID] = struct{}{}
		classificationEndpoint := line.URL
		if adapter == "openai" && p.provider.Kind == "azure_openai" {
			classificationEndpoint = wire.CanonicalBatchEndpoint(line.URL)
		}
		operation, family, ok := managedBatchEndpoint(adapter, classificationEndpoint)
		if !ok || endpoint != "" && endpoint != line.URL {
			param := "file"
			return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d has an unsupported or mixed endpoint.", lineNumber), &param))
		}
		endpoint = line.URL
		var body struct {
			Model string `json:"model"`
		}
		upstreamModel := p.provider.Connector().Model(p.model)
		if json.Unmarshal(line.Body, &body) != nil || body.Model != route.Slug && body.Model != p.model && body.Model != upstreamModel {
			param := "file"
			return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d has an unsupported model.", lineNumber), &param))
		}
		if adapter == "openai" {
			if p.provider.VendorID == "groq" && line.URL != "/v1/chat/completions" {
				return fail(invalidRequest("unsupported_parameter", "Groq batch files support only /v1/chat/completions.", strPtr("file")))
			}
			modelJSON, _ := json.Marshal(p.provider.Connector().Model(p.model))
			bound, err := oif.Apply(document, []oif.Change{{Pointer: "/body/model", Value: string(modelJSON), Origin: oif.ResourceBinding, Reason: "batch model affinity"}})
			if err != nil {
				return fail(err)
			}
			customIDDigest, ok := digest(line.CustomID)
			if !ok {
				return fail(errors.New("batch digest key is unavailable"))
			}
			_, _ = manifestHash.Write(customIDDigest)
			if _, err = writer.Write(append(bound.Bytes(), '\n')); err != nil {
				return fail(err)
			}
			manifest.LineCount++
			continue
		}
		bodyDocument, err := oif.ParseJSON(line.Body, oif.Limits{MaxBytes: managedBatchLineLimit})
		if err != nil {
			return fail(err)
		}
		modelJSON, _ := json.Marshal(route.Slug)
		ingress, err := oif.Apply(bodyDocument, []oif.Change{{Pointer: "/model", Value: string(modelJSON), Origin: oif.IdentityBinding, Reason: "batch ingress model alias"}})
		if err != nil {
			return fail(err)
		}
		request, err := protocols.Parse(family, ingress.Bytes(), route.Slug)
		if err != nil || request.Stream {
			param := "file"
			return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d cannot be translated.", lineNumber), &param))
		}
		defaults := protocols.Object(p.provider.ParameterDefaults)
		if p.provider.ProfileID != "" {
			defaults, _, err = p.provider.Connector().DefaultsFor(operation, p.model)
			if err != nil {
				param := "file"
				return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d cannot be translated.", lineNumber), &param))
			}
		}
		var encoded []byte
		if adapter == "gemini_managed" {
			encoded, _, err = protocols.EncodeTarget(request, wire.Gemini, p.provider.Kind, p.provider.VendorID, upstreamModel, defaults)
		} else {
			encoded, _, err = protocols.Encode(request, p.provider.Kind, p.provider.VendorID, upstreamModel, defaults)
		}
		if err != nil {
			param := "file"
			return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d cannot be translated.", lineNumber), &param))
		}
		if adapter == "gemini_managed" {
			document, parseErr := oif.ParseJSON(encoded, oif.Limits{MaxBytes: managedBatchLineLimit})
			if parseErr != nil || geminiBatchHasFileReference(document.Root()) {
				return fail(invalidRequest("unsupported_parameter", "Gemini batch input cannot use unbound file or cached-content references.", strPtr("file")))
			}
		}
		if adapter == "mistral" || adapter == "gemini_managed" {
			var fields map[string]json.RawMessage
			if json.Unmarshal(encoded, &fields) != nil {
				return fail(errors.New("translated batch line is malformed"))
			}
			delete(fields, "model")
			encoded, _ = json.Marshal(fields)
		}
		customIDDigest, ok := digest(line.CustomID)
		if !ok {
			return fail(errors.New("batch digest key is unavailable"))
		}
		_, _ = manifestHash.Write(customIDDigest)
		resultValue := map[string]any{"custom_id": line.CustomID, "body": json.RawMessage(encoded)}
		if adapter == "gemini_managed" {
			resultValue = map[string]any{"key": line.CustomID, "request": json.RawMessage(encoded)}
		}
		result, _ := json.Marshal(resultValue)
		if _, err = writer.Write(append(result, '\n')); err != nil {
			return fail(err)
		}
		manifest.LineCount++
	}
	if err := scanner.Err(); err != nil {
		param := "file"
		return fail(invalidRequest("unsupported_parameter", fmt.Sprintf("Batch file line %d exceeds the supported bounds.", manifest.LineCount+1), &param))
	}
	if manifest.LineCount == 0 {
		param := "file"
		return fail(invalidRequest("unsupported_parameter", "The batch file must contain at least one request.", &param))
	}
	manifest.Endpoint = endpoint
	manifest.LineOperation = endpointOperation(endpoint)
	manifest.ManifestDigest = hex.EncodeToString(manifestHash.Sum(nil))
	return nil
}

func managedCustomIDLimit(adapter string) int {
	if adapter == "together" {
		return 64
	}
	return 128
}

func managedBatchEndpoint(adapter, endpoint string) (string, wire.Format, bool) {
	switch endpoint {
	case "/v1/chat/completions":
		return "generation", wire.Chat, adapter == "openai" || adapter == "together" || adapter == "mistral" || adapter == "gemini_managed"
	case "/v1/embeddings":
		return "embeddings", wire.Embeddings, adapter == "openai" || adapter == "mistral"
	case "/v1/responses":
		return "generation", wire.Responses, adapter == "openai"
	case "/v1/completions":
		return "generation", wire.Completions, adapter == "openai"
	default:
		return "", "", false
	}
}

func (s *Server) batchLineDigest(customID string) ([]byte, bool) {
	digester, ok := s.Runtime.(interface {
		Digest(secrets.DigestPurpose, string) []byte
	})
	if !ok {
		return nil, false
	}
	return digester.Digest(secrets.BatchLineDigest, customID), true
}

func endpointOperation(endpoint string) string {
	if wire.CanonicalBatchEndpoint(endpoint) == "/v1/embeddings" {
		return "embeddings"
	}
	return "generation"
}

func managedBatchRequestBody(source oif.Document, adapter, upstreamFileID, model string, manifest batchManifest) ([]byte, *int64, *Error) {
	endpointValue, _ := source.Root().Lookup("endpoint")
	endpoint, _ := endpointValue.Text()
	if adapter == "gemini_managed" {
		windowValue, present := source.Root().Lookup("completion_window")
		window, valid := windowValue.Text()
		var metadataValues map[string]string
		if raw, ok := source.Fields()["metadata"]; ok && (json.Unmarshal(raw, &metadataValues) != nil || metadataValues == nil) {
			param := "metadata"
			return nil, nil, invalidRequest("invalid_request", "metadata must be a string map.", &param)
		}
		if !present || !valid || window != "24h" || endpoint != manifest.Endpoint ||
			manifest.LineOperation != "generation" || manifest.LineCount < 1 || upstreamFileID == "" {
			return nil, nil, invalidRequest("invalid_request", "Gemini managed batches require a matching generation manifest and completion_window=24h.", nil)
		}
		body, _ := geminiManagedBatchBody("olp-pending", upstreamFileID)
		expected := int64(manifest.LineCount)
		return body, &expected, nil
	}
	windowValue, present := source.Root().Lookup("completion_window")
	window := "24h"
	if present {
		window, _ = windowValue.Text()
	}
	if endpoint != manifest.Endpoint {
		param := "endpoint"
		return nil, nil, invalidRequest("unsupported_parameter", "The batch endpoint must match the uploaded file.", &param)
	}
	var metadataValues map[string]string
	if metadata, ok := source.Fields()["metadata"]; ok {
		if json.Unmarshal(metadata, &metadataValues) != nil || metadataValues == nil {
			param := "metadata"
			return nil, nil, invalidRequest("unsupported_parameter", "Batch metadata must be a string map.", &param)
		}
	}
	if adapter == "together" {
		body, _ := json.Marshal(map[string]any{"endpoint": endpoint, "input_file_id": upstreamFileID, "completion_window": "24h"})
		count := int64(manifest.LineCount)
		return body, &count, nil
	}
	hours, err := strconv.Atoi(strings.TrimSuffix(window, "h"))
	if strings.HasSuffix(window, "d") {
		days, parseErr := strconv.Atoi(strings.TrimSuffix(window, "d"))
		if parseErr != nil {
			hours = 0
		} else {
			hours = days * 24
		}
	} else if err != nil {
		hours = 0
	}
	if hours < 1 || hours > 168 {
		param := "completion_window"
		return nil, nil, invalidRequest("invalid_value", "Mistral batch timeout_hours must be between 1 and 168.", &param)
	}
	bodyFields := map[string]any{"endpoint": endpoint, "input_files": []string{upstreamFileID}, "model": model, "timeout_hours": hours}
	if metadataValues != nil && adapter == "mistral" {
		bodyFields["metadata"] = metadataValues
	}
	body, _ := json.Marshal(bodyFields)
	count := int64(manifest.LineCount)
	return body, &count, nil
}

func geminiManagedBatchBody(displayName, upstreamFileName string) ([]byte, error) {
	return json.Marshal(map[string]any{"batch": map[string]any{
		"display_name": displayName,
		"input_config": map[string]string{"file_name": upstreamFileName},
	}})
}

func projectManagedBatch(body []byte, adapter, requestedWindow string) ([]byte, string, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, "", errors.New("provider batch response is malformed")
	}
	if adapter == "together" {
		if wrapped, ok := root["job"]; ok {
			if json.Unmarshal(wrapped, &root) != nil || root == nil {
				return nil, "", errors.New("Together batch job is malformed")
			}
		}
	}
	var job struct {
		ID            string          `json:"id"`
		Status        string          `json:"status"`
		Endpoint      string          `json:"endpoint"`
		InputFileID   string          `json:"input_file_id"`
		InputFiles    []string        `json:"input_files"`
		OutputFileID  string          `json:"output_file_id"`
		ErrorFileID   string          `json:"error_file_id"`
		OutputFile    string          `json:"output_file"`
		ErrorFile     string          `json:"error_file"`
		CreatedAt     json.RawMessage `json:"created_at"`
		CompletedAt   json.RawMessage `json:"completed_at"`
		TotalRequests json.RawMessage `json:"total_requests"`
		Succeeded     json.RawMessage `json:"succeeded_requests"`
		Failed        json.RawMessage `json:"failed_requests"`
	}
	jobBytes, _ := json.Marshal(root)
	if json.Unmarshal(jobBytes, &job) != nil || job.ID == "" {
		return nil, "", errors.New("provider batch identity is missing")
	}
	state, ok := managedBatchStatusProjection[adapter][job.Status]
	if !ok {
		return nil, "", errors.New("provider batch status is unknown")
	}
	input := job.InputFileID
	if input == "" && len(job.InputFiles) > 0 {
		input = job.InputFiles[0]
	}
	output := job.OutputFileID
	if output == "" {
		output = job.OutputFile
	}
	errorFile := job.ErrorFileID
	if errorFile == "" {
		errorFile = job.ErrorFile
	}
	if len(job.ID) > 512 || job.Endpoint == "" || input == "" || len(input) > 512 ||
		len(output) > 512 || len(errorFile) > 512 {
		return nil, "", errors.New("provider batch identity is malformed")
	}
	projected := map[string]any{
		"id": job.ID, "object": "batch", "endpoint": job.Endpoint,
		"input_file_id": input, "completion_window": requestedWindow, "status": state,
	}
	if metadata, ok := root["metadata"]; ok {
		projected["metadata"] = metadata
	}
	if output != "" {
		projected["output_file_id"] = output
	}
	if errorFile != "" {
		projected["error_file_id"] = errorFile
	}
	if created, ok := providerUnix(job.CreatedAt); ok {
		projected["created_at"] = created
	}
	if completed, ok := providerUnix(job.CompletedAt); ok {
		projected["completed_at"] = completed
	}
	if adapter == "mistral" {
		total, totalOK := providerCount(job.TotalRequests)
		succeeded, succeededOK := providerCount(job.Succeeded)
		failed, failedOK := providerCount(job.Failed)
		if totalOK && succeededOK && failedOK {
			projected["request_counts"] = map[string]int64{"total": total, "completed": succeeded, "failed": failed}
		}
	}
	out, err := json.Marshal(projected)
	return out, job.ID, err
}

func managedBatchResponse(body []byte, adapter, window string) ([]byte, string, bool) {
	projected, upstreamID, err := projectManagedBatch(body, adapter, window)
	return projected, upstreamID, err == nil
}

func attachBatchClientMetadata(body, metadata json.RawMessage) []byte {
	if len(metadata) == 0 || string(metadata) == "null" {
		return body
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return body
	}
	fields["metadata"] = metadata
	encoded, _ := json.Marshal(fields)
	return encoded
}

func managedBatchMetadata(body []byte, model, localInput, adapter string) (json.RawMessage, string) {
	metadata, state := batchMetadata(body, model, localInput)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(metadata, &fields)
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	fields["adapter"], _ = json.Marshal(adapter)
	encoded, _ := json.Marshal(fields)
	return encoded, state
}

func batchAdapter(resource *resources.Resource) string {
	var metadata struct {
		Adapter string `json:"adapter"`
	}
	if resource != nil {
		_ = json.Unmarshal(resource.Metadata, &metadata)
	}
	if metadata.Adapter == "" {
		return "openai"
	}
	return metadata.Adapter
}

func managedBatchPath(resource *resources.Resource, cancel bool) string {
	adapter := batchAdapter(resource)
	base := "batches/"
	if adapter == "mistral" {
		base = "batch/jobs/"
	}
	path := base + url.PathEscape(resource.UpstreamID)
	if cancel {
		if adapter == "gemini_managed" {
			path += ":cancel"
		} else {
			path += "/cancel"
		}
	}
	return path
}

func providerUnix(raw json.RawMessage) (int64, bool) {
	var seconds int64
	if json.Unmarshal(raw, &seconds) == nil && seconds >= 0 {
		return seconds, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.Unix(), true
		}
	}
	return 0, false
}

func providerCount(raw json.RawMessage) (int64, bool) {
	var number int64
	if json.Unmarshal(raw, &number) == nil && number >= 0 {
		return number, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		number, err := strconv.ParseInt(text, 10, 64)
		return number, err == nil && number >= 0
	}
	return 0, false
}
