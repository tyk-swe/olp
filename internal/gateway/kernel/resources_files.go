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

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	limit, after, e := ListArgs(r)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	kinds := []string{resources.KindFile}
	if authority.Policy.AllowProviderState {
		kinds = append(kinds, resources.KindStrictFile)
	}
	// One extra row reports whether another page follows; it is never
	// authorized or projected.
	rows, err := s.Deps.Resources.ListKinds(r.Context(), kinds, authority.ID, limit+1, after)
	if err != nil {
		s.Refuse(x, w, resourceListError(err, "The file list could not be read."))
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]json.RawMessage, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if retainedFileOperation(row) != "batch" {
			if !authority.Policy.AllowProviderState {
				s.Refuse(x, w, providerStateForbidden())
				return
			}
			if _, _, e := s.ResolveResource(r.Context(), x, authority, row, retainedFileOperation(row), RetainedHousekeeping); e != nil {
				s.Refuse(x, w, e)
				return
			}
		}
		if row.Kind == resources.KindStrictFile {
			_, contract, readErr := s.readDurable(r.Context(), row.Kind, authority.ID, row.ID)
			if readErr != nil || s.authorizeDurable(r.Context(), x, authority, row, contract, "batch", RetainedHousekeeping) != nil {
				s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A file in this list is unavailable."))
				return
			}
		}
		obj, err := s.fileObject(r.Context(), row)
		if err != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A file in this list could not be projected."))
			return
		}
		items = append(items, obj)
		ids = append(ids, row.ID)
	}
	s.WriteStateJSON(w, x, listBody(items, ids, hasMore))
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, _ *routing.Route) *fault.Error {
		return s.getPinnedFile(ctx, w, x, authority, res, p)
	})
}

func (s *Server) getPinnedFile(ctx context.Context, w http.ResponseWriter, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin) *fault.Error {
	adapter, e := storedAdapter(res)
	if e != nil {
		return e
	}
	path, e := providerFilePath(res, adapter)
	if e != nil {
		return e
	}
	endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, path, nil)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		if failure.Status == http.StatusNotFound {
			if err := s.Deps.Resources.Tombstone(ctx, res.ID); err != nil {
				return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
			}
			return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
		}
		return UpstreamError(failure)
	}
	defer resp.Body.Close()

	body, err := resp.ReadBody(s.Config.MaxResponseBytes)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	if e := s.verifyProviderFile(res, adapter, body); e != nil {
		return e
	}
	refreshed, err := refreshedFileMetadata(res, body, adapter)
	if errors.Is(err, errProviderFileMetadata) {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned malformed file metadata.")
	}
	if err != nil {
		resp.Exchange.Interrupt(ClassProtocol, true)
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored file mapping is malformed.")
	}
	var out []byte
	if res.Kind == resources.KindStrictFile {
		if _, _, err := strictResult(body, res.UpstreamID); err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result.")
		}
		resp.Exchange.completeRetainedControl()
		_, contract, readErr := s.readDurable(ctx, res.Kind, authority.ID, res.ID)
		if readErr != nil {
			return durableError(readErr)
		}
		contract.Result = body
		payload, marshalErr := json.Marshal(contract)
		if marshalErr != nil {
			return durableError(marshalErr)
		}
		commitCtx, stopCommit := ResourceCommitContext(ctx)
		defer stopCommit()
		if _, _, commitErr := s.Deps.Resources.UpdateDurableContract(commitCtx, res.Kind, authority.ID, res.ID, refreshed.state, payload); commitErr != nil {
			return durableError(commitErr)
		}
		out, err = durableProjection(body, res.ID, nil)
	} else {
		resp.Exchange.completeRetainedControl()
		if err := s.Deps.Resources.Update(ctx, res.ID, refreshed.state, refreshed.metadata, refreshed.expires); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed file state could not be committed.")
		}
		if adapter.Managed() {
			current := *res
			current.State, current.Metadata, current.ExpiresAt = refreshed.state, refreshed.metadata, refreshed.expires
			out, err = s.fileObject(ctx, &current)
		} else {
			out, err = RewriteID(body, "id", res.ID)
		}
	}
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
	}
	s.WriteStateJSON(w, x, out, resp.Exchange)
	return nil
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, RetainedHousekeeping, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, route *routing.Route) *fault.Error {
		writeDeleted := func(exchanges ...*PinnedExchange) {
			s.WriteStateJSON(w, x, MustJSON(map[string]any{"id": res.ID, "object": "file", "deleted": true}), exchanges...)
		}
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		if adapter == connectors.BatchGeminiManaged {
			detached, e := s.detachGeminiResultFile(ctx, res)
			if e != nil {
				return e
			}
			if detached {
				writeDeleted()
				return nil
			}
		} else if res.ParentID != nil {
			if err := s.RequireBatchEvidenceSettled(ctx, *res.ParentID); err != nil {
				return err
			}
		}
		path, e := providerFilePath(res, adapter)
		if e != nil {
			return e
		}
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, path, nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				commitCtx, stopCommit := ResourceCommitContext(ctx)
				defer stopCommit()
				if err := s.Deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
					return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
				}
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		defer resp.Body.Close()

		// A successful DELETE status is the operation's terminal contract.
		resp.Exchange.completeRetainedControl()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
		commitCtx, stopCommit := ResourceCommitContext(ctx)
		defer stopCommit()
		if err := s.Deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be deleted.")
		}
		writeDeleted(resp.Exchange)
		return nil
	})
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, route *routing.Route) *fault.Error {
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		if adapter == connectors.BatchGeminiManaged {
			return s.geminiManagedFileContent(ctx, w, x, res, p)
		}
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID)+"/content", nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		defer resp.Body.Close()

		// Stage the bounded upstream body before committing headers. Otherwise
		// a provider overflow would become a successful but truncated download.
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		spool := s.Deps.Staging
		if spool == nil {
			resp.Exchange.Interrupt(ClassProtocol, true)
			return fault.ServerError(http.StatusServiceUnavailable, "media_content_unavailable", "The bounded file spool is unavailable.")
		}
		artifact, err := resp.StageBody(ctx, spool, "file-content", contentType, s.Config.MaxResponseBytes)
		if err != nil {
			var tooLarge *staging.TooLargeError
			if errors.As(err, &tooLarge) {
				return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The provider file exceeded the configured response bound.")
			}
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}

		resp.Exchange.completeRetainedControl()
		defer spool.Remove(artifact.Handle)
		opened, err := spool.Open(artifact.Handle)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged provider file could not be read.")
		}
		defer opened.File.Close()
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The file response could not be bounded.")
		}
		SetFileDownloadHeaders(w.Header(), contentType, artifact.ContentLength)
		if res.Kind == resources.KindStrictFile {
			w.Header().Set("X-OLP-Content-SHA256", artifact.Digest)
		}
		x.ResponseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := CopyResourceContent(w, opened.File, artifact.ContentLength)
		if copyErr != nil || written != artifact.ContentLength {
			x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The file response ended before all bytes were delivered.")
			resp.Exchange.Delivery(DeliveryState(written))
			s.Finish(x, &Outcome{Err: x.Failure, Committed: true, Cancelled: true}, x.Failure.Status)
			return nil
		}
		if written > 0 {
			x.Delivered(s.Now())
		}
		resp.Exchange.Delivery(usage.ClientTerminal)
		s.Finish(x, &Outcome{Committed: true}, http.StatusOK)
		return nil
	})
}

func (s *Server) fileCall(w http.ResponseWriter, r *http.Request, use RetainedUse, op func(context.Context, *Call, keyauthority.Authority, *resources.Resource, *Pin, *routing.Route) *fault.Error) {
	x, authority, done := s.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	kind := durableKind(resources.KindFile, r.PathValue("id"))
	if kind == resources.KindStrictFile && !authority.Policy.AllowProviderState {
		s.Refuse(x, w, providerStateForbidden())
		return
	}
	res, contract, err := s.readDurable(r.Context(), kind, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No file with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."))
		return
	}
	if operation := retainedFileOperation(res); operation != "batch" {
		if !authority.Policy.AllowProviderState {
			s.Refuse(x, w, providerStateForbidden())
			return
		}
		x.operationOverride = operation
		if retainedUnsettled(res.State) {
			s.Refuse(x, w, fault.ServerError(http.StatusConflict, "submission_unknown", "The file upload has no reconciled native identity yet."))
			return
		}
		x.retained = retainedOperation{use: RetainedHousekeeping, usage: retainedTokenlessUsage}
		p, route, e := s.ResolveResource(r.Context(), x, authority, res, operation, use)
		if e != nil {
			s.Refuse(x, w, e)
			return
		}
		x.Route = route
		ctx, cancel := s.StateDeadline(r.Context(), route)
		defer cancel()
		defer s.retainedSettle(ctx, x, p)
		if e := s.admitRetainedRequest(ctx, x, authority, p, route, false); e != nil {
			s.Refuse(x, w, e)
			return
		}
		if e := op(ctx, x, authority, res, p, route); e != nil {
			s.Refuse(x, w, e)
		}
		return
	}
	p, route, e := s.admitDurable(r.Context(), x, authority, res, contract, "batch", use)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	x.Route = route
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if e := s.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.Refuse(x, w, e)
	}
}

func providerFilePath(res *resources.Resource, adapter connectors.BatchAdapter) (string, *fault.Error) {
	if adapter != connectors.BatchGeminiManaged {
		return "files/" + url.PathEscape(res.UpstreamID), nil
	}
	path, err := managedGeminiFilePath(res)
	if err != nil {
		return "", fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	return path, nil
}

func (s *Server) verifyProviderFile(res *resources.Resource, adapter connectors.BatchAdapter, body []byte) *fault.Error {
	if adapter == connectors.BatchGeminiManaged {
		var object struct {
			Name string `json:"name"`
		}
		name, err := geminiManagedFileName(res)
		if err != nil || json.Unmarshal(body, &object) != nil || object.Name != name {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini file name.")
		}
		return nil
	}
	if res.Kind != resources.KindStrictFile && !retainedIdentityMatches(body, res.UpstreamID, s.Config.MaxResponseBytes) {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
	}
	return nil
}

func (s *Server) detachGeminiResultFile(ctx context.Context, res *resources.Resource) (bool, *fault.Error) {
	record, err := decodeManagedFileRecord(res.Metadata)
	if err != nil {
		return false, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	if record.Role != "output" && record.Role != "error" {
		return false, nil
	}
	name := cmp.Or(record.ResponsesFile, res.UpstreamID)
	commitCtx, stop := ResourceCommitContext(ctx)
	defer stop()
	detached, err := s.Deps.Resources.DetachGeminiResultView(commitCtx, res, name)
	if errors.Is(err, resources.ErrNotFound) {
		return false, fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
	}
	if err != nil {
		return false, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The result file dependencies could not be read.")
	}
	if detached {
		return true, nil
	}
	pending, err := s.Deps.Batches.GeminiResultPending(ctx, res, name)
	if err != nil {
		return false, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The result file dependencies could not be read.")
	}
	if pending {
		return false, fault.ServerError(http.StatusConflict, "batch_settlement_pending", "The result file is still needed for settlement; retry after settlement closes.")
	}
	return false, nil
}

func uploadedFileMetadata(metadata json.RawMessage, body []byte, adapter connectors.BatchAdapter, manifest BatchManifest) (json.RawMessage, error) {
	switch {
	case adapter.Managed():
		return managedFileMetadata(metadata, body, adapter, manifest, "input")
	case manifest.Adapter == connectors.BatchOpenAI:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(metadata, &fields); err != nil {
			return nil, err
		}
		fields["batch_manifest"] = MustJSON(manifest)
		return json.Marshal(fields)
	default:
		return metadata, nil
	}
}
