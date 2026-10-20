package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
)

var errGeminiManagedBatchState = errors.New("Gemini managed batch state is unknown")

func geminiManagedBatchMetadata(body []byte, model, localInput string, manifest batchManifest, old json.RawMessage, clientMetadata json.RawMessage) (json.RawMessage, string, error) {
	var provider map[string]json.RawMessage
	if json.Unmarshal(body, &provider) != nil || provider == nil {
		return nil, "", errors.New("Gemini batch object is malformed")
	}
	status, ok := managedBatchStatusProjection["gemini_managed"][geminiBatchState(body)]
	if !ok {
		return nil, "", errGeminiManagedBatchState
	}
	metadata := map[string]json.RawMessage{}
	metadata["provider_batch"] = bytes.Clone(body)
	metadata["upstream_model"], _ = json.Marshal(model)
	metadata["adapter"], _ = json.Marshal("gemini_managed")
	metadata["endpoint"], _ = json.Marshal(manifest.Endpoint)
	metadata["line_operation"], _ = json.Marshal(manifest.LineOperation)
	metadata["line_count"], _ = json.Marshal(manifest.LineCount)
	metadata["manifest_digest"], _ = json.Marshal(manifest.ManifestDigest)
	metadata["completion_window"], _ = json.Marshal("24h")
	metadata[batchLocalInputKey], _ = json.Marshal(localInput)
	metadata["status"], _ = json.Marshal(status)
	if len(clientMetadata) > 0 && string(clientMetadata) != "null" {
		metadata["client_metadata"] = bytes.Clone(clientMetadata)
	} else if len(old) > 0 {
		var prior map[string]json.RawMessage
		if json.Unmarshal(old, &prior) == nil {
			metadata["client_metadata"] = prior["client_metadata"]
			for _, field := range []string{"endpoint", "line_operation", "line_count", "manifest_digest", "completion_window", batchLocalInputKey} {
				if value := prior[field]; len(value) > 0 {
					metadata[field] = value
				}
			}
		}
	}
	var geminiMetadata map[string]json.RawMessage
	_ = json.Unmarshal(provider["metadata"], &geminiMetadata)
	if created, ok := providerUnix(geminiMetadata["createTime"]); ok {
		metadata["created_at"], _ = json.Marshal(created)
	}
	if completed, ok := providerUnix(geminiMetadata["endTime"]); ok {
		metadata["completed_at"], _ = json.Marshal(completed)
	}
	if counts := geminiManagedRequestCounts(body); counts != nil {
		metadata["request_counts"], _ = json.Marshal(counts)
	}
	encoded, err := json.Marshal(metadata)
	return encoded, status, err
}

func geminiManagedManifest(metadata []byte) batchManifest {
	var manifest batchManifest
	var fields map[string]json.RawMessage
	if json.Unmarshal(metadata, &fields) != nil {
		return manifest
	}
	_ = json.Unmarshal(fields["adapter"], &manifest.Adapter)
	_ = json.Unmarshal(fields["endpoint"], &manifest.Endpoint)
	_ = json.Unmarshal(fields["line_operation"], &manifest.LineOperation)
	_ = json.Unmarshal(fields["line_count"], &manifest.LineCount)
	_ = json.Unmarshal(fields["manifest_digest"], &manifest.ManifestDigest)
	return manifest
}

func geminiManagedRequestCounts(body []byte) map[string]int64 {
	var root map[string]json.RawMessage
	_ = json.Unmarshal(body, &root)
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(root["metadata"], &metadata)
	var stats map[string]json.RawMessage
	_ = json.Unmarshal(metadata["batchStats"], &stats)
	if stats == nil {
		_ = json.Unmarshal(root["batchStats"], &stats)
	}
	if stats == nil {
		return nil
	}
	counts := map[string]int64{}
	for source, target := range map[string]string{
		"requestCount": "total", "successfulRequestCount": "completed", "failedRequestCount": "failed",
	} {
		raw, present := stats[source]
		if !present {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			value, err := strconv.ParseInt(text, 10, 64)
			if err != nil || value < 0 {
				return nil
			}
			counts[target] = value
			continue
		}
		var value int64
		if json.Unmarshal(raw, &value) != nil || value < 0 {
			return nil
		}
		counts[target] = value
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}

func geminiManagedResponsesFile(body []byte) string {
	var root map[string]json.RawMessage
	_ = json.Unmarshal(body, &root)
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(root["metadata"], &metadata)
	for _, parent := range []map[string]json.RawMessage{metadata, root} {
		var output map[string]json.RawMessage
		if json.Unmarshal(parent["output"], &output) != nil {
			continue
		}
		var name string
		if json.Unmarshal(output["responsesFile"], &name) == nil && name != "" {
			return name
		}
	}
	return ""
}

func (s *Server) mapGeminiManagedResultFiles(ctx context.Context, owner *resources.Resource, fileName string) (*resources.Resource, *resources.Resource, error) {
	fileID, err := geminiFileID(fileName)
	if err != nil {
		return nil, nil, errGeminiBatchFidelity
	}
	var ownerMetadata map[string]json.RawMessage
	_ = json.Unmarshal(owner.Metadata, &ownerMetadata)
	views := make([]*resources.Resource, 0, 2)
	for _, role := range []string{"output", "error"} {
		metadata := map[string]any{
			"upstream_model": resourceModel(owner), "adapter": "gemini_managed", "role": role,
			"gemini_responses_file": fileName, "provider_file": map[string]any{"filename": fileID + ".jsonl", "purpose": "batch"},
		}
		for _, field := range []string{"endpoint", "line_operation", "line_count", "manifest_digest", "completion_window"} {
			if raw, ok := ownerMetadata[field]; ok {
				metadata[field] = raw
			}
		}
		views = append(views, &resources.Resource{
			Kind: resources.KindFile, APIKeyID: owner.APIKeyID, RouteSlug: owner.RouteSlug,
			ProviderID: owner.ProviderID, ProviderRevisionID: owner.ProviderRevisionID, RouteRevisionID: owner.RouteRevisionID,
			TargetID: owner.TargetID, SlotID: owner.SlotID, CredentialID: owner.CredentialID,
			UpstreamID: fileName + "#" + role, State: "processed", Metadata: mustJSON(metadata),
		})
	}
	commitCtx, stop := resourceCommitContext(ctx)
	defer stop()
	return s.Resources.EnsureGeminiResultViews(commitCtx, fileName, views[0], views[1])
}

func geminiManagedFileName(resource *resources.Resource) string {
	var metadata struct {
		ResponseFile string `json:"gemini_responses_file"`
	}
	_ = json.Unmarshal(resource.Metadata, &metadata)
	if metadata.ResponseFile != "" {
		return metadata.ResponseFile
	}
	return resource.UpstreamID
}

func (s *Server) geminiManagedBatchObject(ctx context.Context, resource *resources.Resource) ([]byte, error) {
	var metadata map[string]json.RawMessage
	if json.Unmarshal(resource.Metadata, &metadata) != nil || metadata == nil {
		return nil, errGeminiBatchFidelity
	}
	var providerBatch json.RawMessage
	if json.Unmarshal(metadata["provider_batch"], &providerBatch) != nil || len(providerBatch) == 0 {
		return nil, errGeminiBatchFidelity
	}
	var status string
	if json.Unmarshal(metadata["status"], &status) != nil || status == "" {
		return nil, errGeminiManagedBatchState
	}
	object := map[string]any{
		"id": resource.ID, "object": "batch", "endpoint": geminiRawString(metadata["endpoint"]),
		"input_file_id": batchLocalInput(resource), "completion_window": "24h", "status": status,
	}
	if created, ok := providerUnix(metadata["created_at"]); ok {
		object["created_at"] = created
	}
	if completed, ok := providerUnix(metadata["completed_at"]); ok {
		object["completed_at"] = completed
	}
	if len(metadata["request_counts"]) > 0 && string(metadata["request_counts"]) != "null" {
		var counts map[string]int64
		if json.Unmarshal(metadata["request_counts"], &counts) != nil {
			return nil, errGeminiBatchFidelity
		}
		object["request_counts"] = counts
	}
	if raw := metadata["client_metadata"]; len(raw) > 0 && string(raw) != "null" {
		var clientMetadata any
		if json.Unmarshal(raw, &clientMetadata) != nil {
			return nil, errGeminiBatchFidelity
		}
		object["metadata"] = clientMetadata
	}
	if slices.Contains([]string{"completed", "failed", "cancelled", "expired"}, status) {
		if responsesFile := geminiManagedResponsesFile(providerBatch); responsesFile != "" {
			outputFile, errorFile, err := s.mapGeminiManagedResultFiles(ctx, resource, responsesFile)
			if err != nil {
				return nil, err
			}
			object["output_file_id"], object["error_file_id"] = outputFile.ID, errorFile.ID
		}
	}
	return json.Marshal(object)
}

func geminiManagedFileRole(resource *resources.Resource) string {
	var metadata struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(resource.Metadata, &metadata)
	return metadata.Role
}

func (s *Server) geminiManagedFileContent(ctx context.Context, w http.ResponseWriter, x *call, resource *resources.Resource, p *pin) *Error {
	name := geminiManagedFileName(resource)
	fileID, err := geminiFileID(name)
	if err != nil {
		return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	generation, err := p.provider.Connector().URL(wire.Gemini, p.model, false)
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_error", "The Gemini download address could not be resolved.")
	}
	parsed, err := url.Parse(generation)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return serverError(http.StatusBadGateway, "upstream_error", "The Gemini download address could not be resolved.")
	}
	endpoint := parsed.Scheme + "://" + parsed.Host + "/download/v1beta/files/" + url.PathEscape(fileID) + ":download?alt=media"
	x.geminiFileBinary = true
	response, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		return upstreamError(failure)
	}
	defer response.Body.Close()
	spool := s.transport().Spool
	if spool == nil {
		return serverError(http.StatusServiceUnavailable, "media_content_unavailable", "The bounded file spool is unavailable.")
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/jsonl"
	}
	raw, err := spool.Put(ctx, media.Upload{
		Filename: "gemini-managed-results", ContentType: contentType, MaximumLength: batches.MaxBatchBytes, Body: response.Body,
	})
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_response_too_large", "The Gemini batch file exceeded the bounded spool limit.")
	}
	defer spool.Remove(raw.Handle)
	part := &media.Part{Handle: raw.Handle, Filename: "gemini-managed-results", ContentType: raw.ContentType, Size: raw.ContentLength, Limit: batches.MaxBatchBytes}
	role := geminiManagedFileRole(resource)
	if role != "input" {
		part, err = s.translateGeminiManagedContent(ctx, resource.RouteSlug, part, role)
		if err != nil {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini batch results could not be translated.")
		}
		defer spool.Remove(part.Handle)
		contentType = "application/jsonl"
	}
	opened, err := spool.Open(part.Handle)
	if err != nil {
		return serverError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini file could not be read.")
	}
	defer opened.File.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(part.Size, 10))
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
		return serverError(http.StatusInternalServerError, "internal_error", "The Gemini file response could not be bounded.")
	}
	x.responseHeaders(w.Header(), false)
	w.WriteHeader(http.StatusOK)
	written, err := io.Copy(w, opened.File)
	x.completePinned()
	if err != nil || written != part.Size {
		x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The Gemini file response ended before all bytes were delivered.")
		s.finish(x, &outcome{err: x.failure, committed: true, cancelled: true}, x.failure.Status)
		return nil
	}
	if written > 0 {
		x.delivered(s.now())
	}
	s.finish(x, &outcome{committed: true}, http.StatusOK)
	return nil
}

func (s *Server) translateGeminiManagedContent(ctx context.Context, routeSlug string, source *media.Part, role string) (*media.Part, error) {
	if role != "output" && role != "error" {
		return nil, errGeminiBatchFidelity
	}
	opened, err := s.transport().Spool.Open(source.Handle)
	if err != nil {
		return nil, err
	}
	defer opened.File.Close()
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(opened.File)
		scanner.Buffer(make([]byte, 64<<10), managedBatchLineLimit+2)
		for scanner.Scan() {
			var line struct {
				Key      string          `json:"key"`
				Response json.RawMessage `json:"response"`
				Error    json.RawMessage `json:"error"`
			}
			raw := scanner.Bytes()
			if json.Unmarshal(raw, &line) != nil || line.Key == "" ||
				(len(line.Response) == 0 || string(line.Response) == "null") && (len(line.Error) == 0 || string(line.Error) == "null") ||
				len(line.Response) > 0 && string(line.Response) != "null" && len(line.Error) > 0 && string(line.Error) != "null" {
				done <- errGeminiBatchFidelity
				_ = writer.CloseWithError(errGeminiBatchFidelity)
				return
			}
			if role == "output" && (len(line.Response) == 0 || string(line.Response) == "null") ||
				role == "error" && (len(line.Error) == 0 || string(line.Error) == "null") {
				continue
			}
			var translated map[string]any
			if role == "output" {
				completion, decodeErr := protocols.Decode(wire.Gemini, wire.Chat, line.Response, routeSlug, "")
				if decodeErr != nil || completion == nil {
					done <- errGeminiBatchFidelity
					_ = writer.CloseWithError(errGeminiBatchFidelity)
					return
				}
				body := completion.Body
				if completion.UpstreamID == "" {
					var fields map[string]json.RawMessage
					if json.Unmarshal(body, &fields) != nil {
						done <- errGeminiBatchFidelity
						_ = writer.CloseWithError(errGeminiBatchFidelity)
						return
					}
					delete(fields, "id")
					body, _ = json.Marshal(fields)
				}
				var decoded any
				if json.Unmarshal(body, &decoded) != nil {
					done <- errGeminiBatchFidelity
					_ = writer.CloseWithError(errGeminiBatchFidelity)
					return
				}
				translated = map[string]any{
					"custom_id": line.Key,
					"response":  map[string]any{"status_code": http.StatusOK, "body": decoded},
					"error":     nil,
				}
			} else {
				var providerError struct {
					Code    json.RawMessage `json:"code"`
					Message string          `json:"message"`
				}
				if json.Unmarshal(line.Error, &providerError) != nil {
					done <- errGeminiBatchFidelity
					_ = writer.CloseWithError(errGeminiBatchFidelity)
					return
				}
				code := ""
				if json.Unmarshal(providerError.Code, &code) != nil {
					var number json.Number
					if json.Unmarshal(providerError.Code, &number) == nil {
						code = number.String()
					}
				}
				translated = map[string]any{
					"custom_id": line.Key, "response": nil,
					"error": map[string]string{"code": code, "message": providerError.Message},
				}
			}
			encoded, err := json.Marshal(translated)
			if err != nil {
				done <- errGeminiBatchFidelity
				_ = writer.CloseWithError(errGeminiBatchFidelity)
				return
			}
			if _, err := writer.Write(append(encoded, '\n')); err != nil {
				done <- err
				_ = writer.CloseWithError(err)
				return
			}
		}
		if err := scanner.Err(); err != nil {
			done <- err
			_ = writer.CloseWithError(err)
			return
		}
		done <- nil
		_ = writer.Close()
	}()
	artifact, spoolErr := s.transport().Spool.Put(ctx, media.Upload{
		Filename: source.Filename, ContentType: "application/jsonl", MaximumLength: batches.MaxBatchBytes, Body: reader,
	})
	_ = reader.Close()
	translateErr := <-done
	if spoolErr != nil || translateErr != nil {
		if spoolErr == nil {
			_ = s.transport().Spool.Remove(artifact.Handle)
		}
		if spoolErr != nil {
			return nil, spoolErr
		}
		return nil, translateErr
	}
	return &media.Part{Handle: artifact.Handle, Filename: source.Filename, ContentType: artifact.ContentType, Size: artifact.ContentLength, Limit: batches.MaxBatchBytes}, nil
}

func geminiRawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
