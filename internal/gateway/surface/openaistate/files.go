package openaistate

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
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) listFiles(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	limit, after, e := kernel.ListArgs(r)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	kinds := []string{resources.KindFile}
	if authority.Policy.AllowProviderState {
		kinds = append(kinds, resources.KindStrictFile)
	}
	// One extra row reports whether another page follows; it is never
	// authorized or projected.
	rows, err := s.kernel.Deps.Resources.ListKinds(r.Context(), kinds, authority.ID, limit+1, after)
	if err != nil {
		s.kernel.Refuse(x, w, kernel.ResourceListError(err, "The file list could not be read."))
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]json.RawMessage, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if kernel.RetainedFileOperation(row) != "batch" {
			if !authority.Policy.AllowProviderState {
				s.kernel.Refuse(x, w, kernel.ProviderStateForbidden())
				return
			}
			if _, _, e := s.kernel.ResolveResource(r.Context(), x, authority, row, kernel.RetainedFileOperation(row), kernel.RetainedHousekeeping); e != nil {
				s.kernel.Refuse(x, w, e)
				return
			}
		}
		if row.Kind == resources.KindStrictFile {
			_, contract, readErr := s.kernel.ReadDurable(r.Context(), row.Kind, authority.ID, row.ID)
			if readErr != nil || s.kernel.AuthorizeDurable(r.Context(), x, authority, row, contract, "batch", kernel.RetainedHousekeeping) != nil {
				s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A file in this list is unavailable."))
				return
			}
		}
		obj, err := s.kernel.FileObject(r.Context(), row)
		if err != nil {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A file in this list could not be projected."))
			return
		}
		items = append(items, obj)
		ids = append(ids, row.ID)
	}
	s.kernel.WriteStateJSON(w, x, kernel.ListBody(items, ids, hasMore))
}

func (s surface) getFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin, _ *routing.Route) *fault.Error {
		return s.kernel.GetPinnedFile(ctx, w, x, authority, res, p)
	})
}

func (s surface) deleteFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin, route *routing.Route) *fault.Error {
		writeDeleted := func(exchanges ...*kernel.PinnedExchange) {
			s.kernel.WriteStateJSON(w, x, kernel.MustJSON(map[string]any{"id": res.ID, "object": "file", "deleted": true}), exchanges...)
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
			if err := s.kernel.RequireBatchEvidenceSettled(ctx, *res.ParentID); err != nil {
				return err
			}
		}
		path, e := kernel.ProviderFilePath(res, adapter)
		if e != nil {
			return e
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, path, nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
				defer stopCommit()
				if err := s.kernel.Deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
					return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
				}
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		defer resp.Body.Close()

		// A successful DELETE status is the operation's terminal contract.
		resp.Exchange.CompleteRetainedControl()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, kernel.ErrorBodyLimit))
		commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
		defer stopCommit()
		if err := s.kernel.Deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be deleted.")
		}
		writeDeleted(resp.Exchange)
		return nil
	})
}

func (s surface) fileContent(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin, route *routing.Route) *fault.Error {
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		if adapter == connectors.BatchGeminiManaged {
			return s.kernel.GeminiManagedFileContent(ctx, w, x, res, p)
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID)+"/content", nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		defer resp.Body.Close()

		// Stage the bounded upstream body before committing headers. Otherwise
		// a provider overflow would become a successful but truncated download.
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		spool := s.kernel.Deps.Staging
		if spool == nil {
			resp.Exchange.Interrupt(kernel.ClassProtocol, true)
			return fault.ServerError(http.StatusServiceUnavailable, "media_content_unavailable", "The bounded file spool is unavailable.")
		}
		artifact, err := resp.StageBody(ctx, spool, "file-content", contentType, s.kernel.Config.MaxResponseBytes)
		if err != nil {
			var tooLarge *staging.TooLargeError
			if errors.As(err, &tooLarge) {
				return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The provider file exceeded the configured response bound.")
			}
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}

		resp.Exchange.CompleteRetainedControl()
		defer spool.Remove(artifact.Handle)
		opened, err := spool.Open(artifact.Handle)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged provider file could not be read.")
		}
		defer opened.File.Close()
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The file response could not be bounded.")
		}
		kernel.SetFileDownloadHeaders(w.Header(), contentType, artifact.ContentLength)
		if res.Kind == resources.KindStrictFile {
			w.Header().Set("X-OLP-Content-SHA256", artifact.Digest)
		}
		s.kernel.DeliverResourceContent(context.Background(), w, x, resp.Exchange, opened.File, artifact.ContentLength, "The file response ended before all bytes were delivered.")
		return nil
	})
}

func (s surface) fileCall(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, op func(context.Context, *kernel.Call, keyauthority.Authority, *resources.Resource, *kernel.Pin, *routing.Route) *fault.Error) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	kind := kernel.DurableKind(resources.KindFile, r.PathValue("id"))
	if kind == resources.KindStrictFile && !authority.Policy.AllowProviderState {
		s.kernel.Refuse(x, w, kernel.ProviderStateForbidden())
		return
	}
	res, contract, err := s.kernel.ReadDurable(r.Context(), kind, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No file with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."))
		return
	}
	if operation := kernel.RetainedFileOperation(res); operation != "batch" {
		if !authority.Policy.AllowProviderState {
			s.kernel.Refuse(x, w, kernel.ProviderStateForbidden())
			return
		}
		if kernel.RetainedUnsettled(res.State) {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusConflict, "submission_unknown", "The file upload has no reconciled native identity yet."))
			return
		}
		x.HousekeepingOperation(operation)
		p, route, e := s.kernel.ResolveResource(r.Context(), x, authority, res, operation, use)
		if e != nil {
			s.kernel.Refuse(x, w, e)
			return
		}
		x.Route = route
		ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
		defer cancel()
		defer s.kernel.SettleRetained(ctx, x, p)
		if e := s.kernel.AdmitRetainedRequest(ctx, x, authority, p, route, false); e != nil {
			s.kernel.Refuse(x, w, e)
			return
		}
		if e := op(ctx, x, authority, res, p, route); e != nil {
			s.kernel.Refuse(x, w, e)
		}
		return
	}
	p, route, e := s.kernel.AdmitDurable(r.Context(), x, authority, res, contract, "batch", use)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if e := s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
	}
}

func (s surface) detachGeminiResultFile(ctx context.Context, res *resources.Resource) (bool, *fault.Error) {
	record, err := kernel.DecodeManagedFileRecord(res.Metadata)
	if err != nil {
		return false, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	if record.Role != "output" && record.Role != "error" {
		return false, nil
	}
	name := cmp.Or(record.ResponsesFile, res.UpstreamID)
	commitCtx, stop := kernel.ResourceCommitContext(ctx)
	defer stop()
	detached, err := s.kernel.Deps.Resources.DetachGeminiResultView(commitCtx, res, name)
	if errors.Is(err, resources.ErrNotFound) {
		return false, fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
	}
	if err != nil {
		return false, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The result file dependencies could not be read.")
	}
	if detached {
		return true, nil
	}
	pending, err := s.kernel.Deps.Batches.GeminiResultPending(ctx, res, name)
	if err != nil {
		return false, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The result file dependencies could not be read.")
	}
	if pending {
		return false, fault.ServerError(http.StatusConflict, "batch_settlement_pending", "The result file is still needed for settlement; retry after settlement closes.")
	}
	return false, nil
}

func uploadedFileMetadata(metadata json.RawMessage, body []byte, adapter connectors.BatchAdapter, manifest kernel.BatchManifest) (json.RawMessage, error) {
	switch {
	case adapter.Managed():
		return kernel.ManagedFileMetadata(metadata, body, adapter, manifest, "input")
	case manifest.Adapter == connectors.BatchOpenAI:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(metadata, &fields); err != nil {
			return nil, err
		}
		fields["batch_manifest"] = kernel.MustJSON(manifest)
		return json.Marshal(fields)
	default:
		return metadata, nil
	}
}
