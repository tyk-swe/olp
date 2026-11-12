package gateway

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/staging"
)

var errGeminiManagedBatchState = errors.New("Gemini managed batch state is unknown")

type geminiManagedCounts struct {
	Total     *int64 `json:"total,omitempty"`
	Completed *int64 `json:"completed,omitempty"`
	Failed    *int64 `json:"failed,omitempty"`
}

type geminiManagedRecord struct {
	batchManifest
	ProviderBatch    json.RawMessage      `json:"provider_batch"`
	UpstreamModel    string               `json:"upstream_model"`
	CompletionWindow string               `json:"completion_window"`
	LocalInputFileID string               `json:"local_input_file_id"`
	Status           string               `json:"status"`
	ClientMetadata   json.RawMessage      `json:"client_metadata,omitempty"`
	CreatedAt        *int64               `json:"created_at,omitempty"`
	CompletedAt      *int64               `json:"completed_at,omitempty"`
	RequestCounts    *geminiManagedCounts `json:"request_counts,omitempty"`
	ResponsesFile    string               `json:"gemini_responses_file,omitempty"`
}

type geminiManagedState struct {
	metadata json.RawMessage
	status   string
}

func newGeminiManagedRecord(localInput string, manifest batchManifest, clientMetadata json.RawMessage) geminiManagedRecord {
	record := geminiManagedRecord{batchManifest: manifest, CompletionWindow: "24h", LocalInputFileID: localInput}
	if string(clientMetadata) != "null" {
		record.ClientMetadata = clientMetadata
	}
	return record
}

func decodeGeminiManagedRecord(metadata json.RawMessage) (geminiManagedRecord, error) {
	var record geminiManagedRecord
	if err := json.Unmarshal(metadata, &record); err != nil {
		return geminiManagedRecord{}, errGeminiBatchFidelity
	}
	return record, nil
}

func geminiManagedBatchMetadata(body []byte, model string, base geminiManagedRecord) (geminiManagedState, error) {
	var provider struct {
		State    string `json:"state"`
		Metadata struct {
			State      string            `json:"state"`
			CreateTime json.RawMessage   `json:"createTime"`
			EndTime    json.RawMessage   `json:"endTime"`
			BatchStats *geminiBatchStats `json:"batchStats"`
		} `json:"metadata"`
		BatchStats *geminiBatchStats `json:"batchStats"`
	}
	if err := json.Unmarshal(body, &provider); err != nil {
		return geminiManagedState{}, errors.New("Gemini batch object is malformed")
	}
	status, ok := upstream.GeminiStatus(cmp.Or(provider.Metadata.State, provider.State))
	if !ok {
		return geminiManagedState{}, errGeminiManagedBatchState
	}
	lifecycle, err := upstream.GeminiMetadata(body, model, "", "")
	if err != nil {
		return geminiManagedState{}, err
	}
	record := base
	record.Adapter = connectors.BatchGeminiManaged
	record.ProviderBatch, record.UpstreamModel, record.Status = lifecycle, model, status
	record.CreatedAt = optionalUnix(provider.Metadata.CreateTime)
	record.CompletedAt = optionalUnix(provider.Metadata.EndTime)
	record.RequestCounts = cmp.Or(provider.Metadata.BatchStats, provider.BatchStats).counts()
	encoded, err := json.Marshal(record)
	return geminiManagedState{metadata: encoded, status: status}, err
}

type geminiBatchStats struct {
	RequestCount *upstream.Count `json:"requestCount"`
	Successful   *upstream.Count `json:"successfulRequestCount"`
	Failed       *upstream.Count `json:"failedRequestCount"`
}

func (s *geminiBatchStats) counts() *geminiManagedCounts {
	if s == nil || s.RequestCount == nil && s.Successful == nil && s.Failed == nil {
		return nil
	}
	return &geminiManagedCounts{Total: countValue(s.RequestCount), Completed: countValue(s.Successful), Failed: countValue(s.Failed)}
}

func countValue(count *upstream.Count) *int64 {
	if count == nil {
		return nil
	}
	return count.Int64()
}

func optionalUnix(raw json.RawMessage) *int64 {
	seconds, ok := upstream.UnixSeconds(raw)
	if !ok {
		return nil
	}
	return &seconds
}

func (s *Server) mapGeminiManagedResultFiles(ctx context.Context, owner *resources.Resource, fileName string) (*resources.Resource, *resources.Resource, error) {
	fileID, err := geminiFileID(fileName)
	if err != nil {
		return nil, nil, errGeminiBatchFidelity
	}
	batch, err := decodeGeminiManagedRecord(owner.Metadata)
	if err != nil {
		return nil, nil, err
	}
	view := func(role string) *resources.Resource {
		metadata := managedFileRecord{
			batchManifest: batch.batchManifest.withAdapter(connectors.BatchGeminiManaged), UpstreamModel: resourceModel(owner), Role: role,
			ResponsesFile: fileName, CompletionWindow: batch.CompletionWindow,
			ProviderFile: managedFileObject{Filename: new(fileID + ".jsonl"), Purpose: new("batch")},
		}
		return &resources.Resource{
			Kind: resources.KindFile, APIKeyID: owner.APIKeyID, RouteSlug: owner.RouteSlug,
			ProviderID: owner.ProviderID, ProviderRevisionID: owner.ProviderRevisionID, RouteRevisionID: owner.RouteRevisionID,
			TargetID: owner.TargetID, SlotID: owner.SlotID, CredentialID: owner.CredentialID,
			ParentID:   new(owner.UUID),
			UpstreamID: fileName + "#" + role, State: "processed", Metadata: mustJSON(metadata),
		}
	}
	commitCtx, stop := resourceCommitContext(ctx)
	defer stop()
	return s.deps.Resources.EnsureGeminiResultViews(commitCtx, fileName, view("output"), view("error"))
}

func geminiManagedFileName(resource *resources.Resource) (string, error) {
	record, err := decodeManagedFileRecord(resource.Metadata)
	if err != nil {
		return "", err
	}
	return cmp.Or(record.ResponsesFile, resource.UpstreamID), nil
}

func (s *Server) geminiManagedBatchObject(ctx context.Context, resource *resources.Resource) ([]byte, error) {
	record, err := decodeGeminiManagedRecord(resource.Metadata)
	if err != nil || len(record.ProviderBatch) == 0 {
		return nil, errGeminiBatchFidelity
	}
	if record.Status == "" {
		return nil, errGeminiManagedBatchState
	}
	object := map[string]any{
		"id": resource.ID, "object": "batch", "endpoint": record.Endpoint,
		"input_file_id": record.LocalInputFileID, "completion_window": "24h", "status": record.Status,
	}
	if record.CreatedAt != nil {
		object["created_at"] = *record.CreatedAt
	}
	if record.CompletedAt != nil {
		object["completed_at"] = *record.CompletedAt
	}
	if record.RequestCounts != nil {
		object["request_counts"] = record.RequestCounts
	}
	if len(record.ClientMetadata) > 0 {
		object["metadata"] = record.ClientMetadata
	}
	if wire.BatchStageOf(record.Status) == wire.BatchTerminal {
		batch, err := upstream.DecodeGeminiBatch(record.ProviderBatch)
		if err != nil {
			return nil, errGeminiBatchFidelity
		}
		if batch.OutputFile != "" {
			outputFile, errorFile, err := s.mapGeminiManagedResultFiles(ctx, resource, batch.OutputFile)
			if err != nil {
				return nil, err
			}
			object["output_file_id"], object["error_file_id"] = outputFile.ID, errorFile.ID
		}
	}
	return json.Marshal(object)
}

func (s *Server) geminiManagedFileContent(ctx context.Context, w http.ResponseWriter, x *call, resource *resources.Resource, p *pin) *fault.Error {
	record, err := decodeManagedFileRecord(resource.Metadata)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	fileID, err := geminiFileID(cmp.Or(record.ResponsesFile, resource.UpstreamID))
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	generation, err := p.provider.Connector().URL(wire.Gemini, p.model, false)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini download address could not be resolved.")
	}
	parsed, err := url.Parse(generation)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini download address could not be resolved.")
	}
	endpoint := parsed.Scheme + "://" + parsed.Host + "/download/v1beta/files/" + url.PathEscape(fileID) + ":download?alt=media"
	x.geminiFileBinary = true
	response, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		return upstreamError(failure)
	}
	defer response.Body.Close()
	spool := s.deps.Staging
	if spool == nil {
		return fault.ServerError(http.StatusServiceUnavailable, "media_content_unavailable", "The bounded file spool is unavailable.")
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/jsonl"
	}
	limit := min(s.cfg.MaxResponseBytes, batches.MaxBatchBytes)
	raw, err := spool.Put(ctx, staging.Upload{
		Filename: "gemini-managed-results", ContentType: contentType, MaximumLength: limit, Body: response.Body,
	})
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Gemini batch file exceeded the bounded spool limit.")
	}
	defer spool.Remove(raw.Handle)
	part := &staging.Part{Handle: raw.Handle, Filename: "gemini-managed-results", ContentType: raw.ContentType, Size: raw.ContentLength, Limit: limit}
	if record.Role != "input" {
		part, err = s.translateGeminiManagedContent(ctx, resource.RouteSlug, part, record.Role)
		if err != nil {
			var tooLarge *staging.TooLargeError
			if errors.As(err, &tooLarge) {
				return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The translated Gemini batch file exceeded the response limit.")
			}
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini batch results could not be translated.")
		}
		defer spool.Remove(part.Handle)
		contentType = "application/jsonl"
	}
	opened, err := spool.Open(part.Handle)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini file could not be read.")
	}
	defer opened.File.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(part.Size, 10))
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
		return fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini file response could not be bounded.")
	}
	x.responseHeaders(w.Header(), false)
	w.WriteHeader(http.StatusOK)
	written, err := copyResourceContent(w, opened.File, part.Size)
	x.completePinned()
	if err != nil || written != part.Size {
		if len(x.facts) > 0 {
			x.facts[len(x.facts)-1].Class = classCancelled
		}
		x.failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The Gemini file response ended before all bytes were delivered.")
		s.finish(x, &outcome{err: x.failure, committed: true, cancelled: true}, x.failure.Status)
		return nil
	}
	if written > 0 {
		x.delivered(s.now())
	}
	s.finish(x, &outcome{committed: true}, http.StatusOK)
	return nil
}

func (s *Server) translateGeminiManagedContent(ctx context.Context, routeSlug string, source *staging.Part, role string) (*staging.Part, error) {
	if role != "output" && role != "error" {
		return nil, errGeminiBatchFidelity
	}
	opened, err := s.deps.Staging.Open(source.Handle)
	if err != nil {
		return nil, err
	}
	defer opened.File.Close()
	reader, writer := io.Pipe()
	limit := min(s.cfg.MaxResponseBytes, source.Limit, batches.MaxBatchBytes)
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
					body = mustJSON(fields)
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
	artifact, spoolErr := s.deps.Staging.Put(ctx, staging.Upload{
		Filename: source.Filename, ContentType: "application/jsonl", MaximumLength: limit, Body: reader,
	})
	_ = reader.Close()
	translateErr := <-done
	if spoolErr != nil || translateErr != nil {
		if spoolErr == nil {
			_ = s.deps.Staging.Remove(artifact.Handle)
		}
		if spoolErr != nil {
			return nil, spoolErr
		}
		return nil, translateErr
	}
	return &staging.Part{Handle: artifact.Handle, Filename: source.Filename, ContentType: artifact.ContentType, Size: artifact.ContentLength, Limit: limit}, nil
}
