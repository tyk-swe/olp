package kernel

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/staging"
)

var (
	ErrGeminiManagedBatchState = managedbatch.ErrGeminiManagedBatchState
	ErrGeminiBatchFidelity     = managedbatch.ErrGeminiBatchFidelity
)

type (
	geminiManagedRecord = managedbatch.GeminiRecord
	GeminiManagedState  = managedbatch.GeminiState
)

func NewGeminiManagedRecord(localInput string, manifest BatchManifest, clientMetadata json.RawMessage) geminiManagedRecord {
	return managedbatch.NewGeminiRecord(localInput, manifest, clientMetadata)
}

func DecodeGeminiManagedRecord(metadata json.RawMessage) (geminiManagedRecord, error) {
	return managedbatch.DecodeGeminiRecord(metadata)
}

func GeminiManagedBatchMetadata(body []byte, model string, base geminiManagedRecord) (GeminiManagedState, error) {
	return managedbatch.GeminiBatchMetadata(body, model, base)
}

func GeminiBatchHasFileReference(request oif.Value) bool {
	return managedbatch.GeminiBatchHasFileReference(request)
}

func (s *Server) mapGeminiManagedResultFiles(ctx context.Context, owner *resources.Resource, fileName string) (*resources.Resource, *resources.Resource, error) {
	fileID, err := GeminiFileID(fileName)
	if err != nil {
		return nil, nil, ErrGeminiBatchFidelity
	}
	batch, err := DecodeGeminiManagedRecord(owner.Metadata)
	if err != nil {
		return nil, nil, err
	}
	view := func(role string) *resources.Resource {
		metadata := managedFileRecord{
			Manifest: batch.Manifest.WithAdapter(connectors.BatchGeminiManaged), UpstreamModel: ResourceModel(owner), Role: role,
			ResponsesFile: fileName, CompletionWindow: batch.CompletionWindow,
			ProviderFile: managedFileObject{Filename: new(fileID + ".jsonl"), Purpose: new("batch")},
		}
		return &resources.Resource{
			Kind: resources.KindFile, APIKeyID: owner.APIKeyID, RouteSlug: owner.RouteSlug,
			ProviderID: owner.ProviderID, ProviderRevisionID: owner.ProviderRevisionID, RouteRevisionID: owner.RouteRevisionID,
			TargetID: owner.TargetID, SlotID: owner.SlotID, CredentialID: owner.CredentialID,
			ParentID:   new(owner.UUID),
			UpstreamID: fileName + "#" + role, State: "processed", Metadata: MustJSON(metadata),
		}
	}
	commitCtx, stop := ResourceCommitContext(ctx)
	defer stop()
	return s.Deps.Resources.EnsureGeminiResultViews(commitCtx, fileName, view("output"), view("error"))
}

func geminiManagedFileName(resource *resources.Resource) (string, error) {
	record, err := DecodeManagedFileRecord(resource.Metadata)
	if err != nil {
		return "", err
	}
	return cmp.Or(record.ResponsesFile, resource.UpstreamID), nil
}

func (s *Server) geminiManagedBatchObject(ctx context.Context, resource *resources.Resource) ([]byte, error) {
	record, err := DecodeGeminiManagedRecord(resource.Metadata)
	if err != nil || len(record.ProviderBatch) == 0 {
		return nil, ErrGeminiBatchFidelity
	}
	if record.Status == "" {
		return nil, ErrGeminiManagedBatchState
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
			return nil, ErrGeminiBatchFidelity
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

func (s *Server) GeminiManagedFileContent(ctx context.Context, w http.ResponseWriter, x *Call, resource *resources.Resource, p *Pin) *fault.Error {
	record, err := DecodeManagedFileRecord(resource.Metadata)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	fileID, err := GeminiFileID(cmp.Or(record.ResponsesFile, resource.UpstreamID))
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	generation, err := p.Provider.Connector().URL(wire.Gemini, p.Model, false)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini download address could not be resolved.")
	}
	parsed, err := url.Parse(generation)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini download address could not be resolved.")
	}
	endpoint := parsed.Scheme + "://" + parsed.Host + "/download/v1beta/files/" + url.PathEscape(fileID) + ":download?alt=media"
	x.PinnedBinary = true
	response, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		return UpstreamError(failure)
	}
	defer response.Body.Close()
	spool := s.Deps.Staging
	if spool == nil {
		response.Exchange.Interrupt(ClassProtocol, true)
		return fault.ServerError(http.StatusServiceUnavailable, "media_content_unavailable", "The bounded file spool is unavailable.")
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/jsonl"
	}
	limit := min(s.Config.MaxResponseBytes, batches.MaxBatchBytes)
	raw, err := response.StageBody(ctx, spool, "gemini-managed-results", contentType, limit)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Gemini batch file exceeded the bounded spool limit.")
	}
	defer spool.Remove(raw.Handle)
	part := &staging.Part{Handle: raw.Handle, Filename: "gemini-managed-results", ContentType: raw.ContentType, Size: raw.ContentLength, Limit: limit}
	if record.Role != "input" {
		part, err = s.translateGeminiManagedContent(ctx, resource.RouteSlug, part, record.Role)
		if err != nil {
			response.Exchange.Interrupt(ClassProtocol, !errors.Is(err, ErrGeminiBatchFidelity))
			var tooLarge *staging.TooLargeError
			if errors.As(err, &tooLarge) {
				return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The translated Gemini batch file exceeded the response limit.")
			}
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini batch results could not be translated.")
		}
		defer spool.Remove(part.Handle)
		contentType = "application/jsonl"
	}
	response.Exchange.Complete(ClassSuccess)
	opened, err := spool.Open(part.Handle)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini file could not be read.")
	}
	defer opened.File.Close()
	SetFileDownloadHeaders(w.Header(), contentType, part.Size)
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
		return fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini file response could not be bounded.")
	}
	s.DeliverResourceContent(ctx, w, x, response.Exchange, opened.File, part.Size, "The Gemini file response ended before all bytes were delivered.")
	return nil
}

func (s *Server) translateGeminiManagedContent(ctx context.Context, routeSlug string, source *staging.Part, role string) (*staging.Part, error) {
	if role != "output" && role != "error" {
		return nil, ErrGeminiBatchFidelity
	}
	opened, err := s.Deps.Staging.Open(source.Handle)
	if err != nil {
		return nil, err
	}
	defer opened.File.Close()
	reader, writer := io.Pipe()
	limit := min(s.Config.MaxResponseBytes, source.Limit, batches.MaxBatchBytes)
	done := make(chan error, 1)
	go func() {
		defer panics.Recover(s.Log, "gemini batch results", func(err error) {
			done <- err
			_ = writer.CloseWithError(err)
		})
		streamErr := managedbatch.TranslateGeminiStream(opened.File, writer, routeSlug, role)
		done <- streamErr
		_ = writer.CloseWithError(streamErr)
	}()
	artifact, spoolErr := s.Deps.Staging.Put(ctx, staging.Upload{
		Filename: source.Filename, ContentType: "application/jsonl", MaximumLength: limit, Body: reader,
	})
	_ = reader.Close()
	translateErr := <-done
	if spoolErr != nil || translateErr != nil {
		if spoolErr == nil {
			_ = s.Deps.Staging.Remove(artifact.Handle)
		}
		if spoolErr != nil {
			return nil, spoolErr
		}
		return nil, translateErr
	}
	return &staging.Part{Handle: artifact.Handle, Filename: source.Filename, ContentType: artifact.ContentType, Size: artifact.ContentLength, Limit: limit}, nil
}
