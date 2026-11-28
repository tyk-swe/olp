package openaistate

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
)

func (s surface) mapGeminiManagedResultFiles(ctx context.Context, owner *resources.Resource, fileName string) (*resources.Resource, *resources.Resource, error) {
	fileID, err := kernel.GeminiFileID(fileName)
	if err != nil {
		return nil, nil, managedbatch.ErrGeminiBatchFidelity
	}
	batch, err := managedbatch.DecodeGeminiRecord(owner.Metadata)
	if err != nil {
		return nil, nil, err
	}
	view := func(role string) *resources.Resource {
		metadata := managedbatch.FileRecord{
			Manifest: batch.Manifest.WithAdapter(connectors.BatchGeminiManaged), UpstreamModel: kernel.ResourceModel(owner), Role: role,
			ResponsesFile: fileName, CompletionWindow: batch.CompletionWindow,
			ProviderFile: managedbatch.FileObject{Filename: new(fileID + ".jsonl"), Purpose: new("batch")},
		}
		return &resources.Resource{
			Kind: resources.KindFile, APIKeyID: owner.APIKeyID, RouteSlug: owner.RouteSlug,
			ProviderID: owner.ProviderID, ProviderRevisionID: owner.ProviderRevisionID, RouteRevisionID: owner.RouteRevisionID,
			TargetID: owner.TargetID, SlotID: owner.SlotID, CredentialID: owner.CredentialID,
			ParentID:   new(owner.UUID),
			UpstreamID: fileName + "#" + role, State: "processed", Metadata: kernel.MustJSON(metadata),
		}
	}
	commitCtx, stop := kernel.ResourceCommitContext(ctx)
	defer stop()
	return s.store.EnsureGeminiResultViews(commitCtx, fileName, view("output"), view("error"))
}

func geminiManagedFileName(resource *resources.Resource) (string, error) {
	record, err := managedbatch.DecodeFileRecord(resource.Metadata)
	if err != nil {
		return "", err
	}
	return cmp.Or(record.ResponsesFile, resource.UpstreamID), nil
}

func managedGeminiFilePath(res *resources.Resource) (string, error) {
	name, err := geminiManagedFileName(res)
	if err != nil {
		return "", err
	}
	id, err := kernel.GeminiFileID(name)
	if err != nil {
		return "", err
	}
	return "files/" + url.PathEscape(id), nil
}

func (s surface) geminiManagedBatchObject(ctx context.Context, resource *resources.Resource) ([]byte, error) {
	record, err := managedbatch.DecodeGeminiRecord(resource.Metadata)
	if err != nil || len(record.ProviderBatch) == 0 {
		return nil, managedbatch.ErrGeminiBatchFidelity
	}
	if record.Status == "" {
		return nil, managedbatch.ErrGeminiManagedBatchState
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
			return nil, managedbatch.ErrGeminiBatchFidelity
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

func (s surface) geminiManagedFileContent(ctx context.Context, w http.ResponseWriter, x *kernel.Call, resource *resources.Resource, p *kernel.Pin) *fault.Error {
	record, err := managedbatch.DecodeFileRecord(resource.Metadata)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	fileID, err := kernel.GeminiFileID(cmp.Or(record.ResponsesFile, resource.UpstreamID))
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
	response, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		return kernel.UpstreamError(failure)
	}
	defer func() { _ = response.Body.Close() }()
	if s.spool == nil {
		response.Exchange.Interrupt(kernel.ClassProtocol, true)
		return fault.ServerError(http.StatusServiceUnavailable, "media_content_unavailable", "The bounded file spool is unavailable.")
	}
	contentType := cmp.Or(response.Header.Get("Content-Type"), "application/jsonl")
	limit := min(s.kernel.Config.MaxResponseBytes, batches.MaxBatchBytes)
	raw, err := response.StageBody(ctx, s.spool, "gemini-managed-results", contentType, limit)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Gemini batch file exceeded the bounded spool limit.")
	}
	defer func() { _ = s.spool.Remove(raw.Handle) }()
	staged := &staging.Part{Handle: raw.Handle, Filename: "gemini-managed-results", ContentType: raw.ContentType, Size: raw.ContentLength, Limit: limit}
	part, projectedType := staged, contentType
	if record.Role != "input" {
		translated, e := s.translatedGeminiResults(ctx, resource.RouteSlug, record.Role, staged, response.Exchange)
		if e != nil {
			return e
		}
		defer func() { _ = s.spool.Remove(translated.Handle) }()
		part, projectedType = translated, "application/jsonl"
	}
	response.Exchange.Complete(kernel.ClassSuccess)
	opened, err := s.spool.Open(part.Handle)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini file could not be read.")
	}
	defer func() { _ = opened.File.Close() }()
	kernel.SetFileDownloadHeaders(w.Header(), projectedType, part.Size)
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
		return fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini file response could not be bounded.")
	}
	s.kernel.DeliverResourceContent(ctx, w, x, response.Exchange, opened.File, part.Size, "The Gemini file response ended before all bytes were delivered.")
	return nil
}
