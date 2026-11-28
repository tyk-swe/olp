package anthropicstate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/kernel"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
)

const filesBeta = "files-api-2025-04-14"

func filesHeaders() http.Header {
	return http.Header{"Anthropic-Beta": {filesBeta}}
}

func filesQuery() url.Values {
	return url.Values{"beta": []string{"true"}}
}

func (s surface) uploadFile(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.begin(w, r, wire.AnthropicFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	route, err := s.kernel.NativeStateRoute(x, authority, r, "")
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	form, err := s.kernel.ParseMultipartForm(w, r, authority.ID, s.kernel.Config.MaxMediaBodyBytes, 1)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	defer form.Cleanup()
	file := form.TakeSingleFile("file")
	if form.Err() != nil || file == nil {
		s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "Upload exactly one file part.", nil))
		return
	}
	if extra := form.TakeExtensions(); form.Err() != nil || len(extra) != 0 {
		s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "Anthropic file uploads accept only one file part.", nil))
		return
	}
	form.Disarm()
	defer func() { _ = s.spool.Remove(file.Handle) }()
	p, err := s.kernel.NativeStatePin(r.Context(), x, route)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if e := s.kernel.UnboundedPinLimits(x.AdmissionAuthority(authority), p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e := s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	x.PinnedHeaders = filesHeaders()
	endpoint, urlErr := kernel.ResourceURL(p.Provider.Connector(), p.Model, "files", filesQuery())
	if urlErr != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."))
		return
	}
	resp, failure := s.kernel.UploadMultipart(ctx, x, p, endpoint, "", nil, file, false)
	if failure != nil {
		s.kernel.Refuse(x, w, kernel.UpstreamError(failure))
		return
	}
	defer resp.Body.Close()

	body, readErr := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
	if readErr != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."))
		return
	}
	upstreamID, valid := kernel.NativeResourceID(body, s.kernel.Config.MaxResponseBytes)
	if !valid {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."))
		return
	}
	metadata, state, expires := fileMetadata(body, p.Model)
	resp.Exchange.Complete(kernel.ClassSuccess)
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	defer stopCommit()
	res, putErr := s.store.Put(commitCtx, &resources.Resource{
		Kind: resources.KindAnthropicFile, APIKeyID: authority.ID, RouteSlug: route.Slug,
		ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID,
		TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID,
		UpstreamID: upstreamID, State: state, Metadata: metadata, ExpiresAt: expires,
	})
	if putErr != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be stored."))
		return
	}
	out, _ := kernel.RewriteID(body, "id", res.ID)
	s.kernel.WriteStateJSON(w, x, out, resp.Exchange)
}

func (s surface) fileCall(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, op func(context.Context, *kernel.Call, *resources.Resource, *kernel.Pin) *fault.Error) {
	x, authority, done := s.begin(w, r, wire.AnthropicFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	res, err := s.store.Get(r.Context(), resources.KindAnthropicFile, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No file with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."))
		return
	}
	p, route, e := s.kernel.ResolveResource(r.Context(), x, authority, res, "batch", use)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if !p.Provider.Connector().Supports("batch", "anthropic", "unary") || !p.Provider.Supports(p.Model, "batch", "anthropic", "unary") {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No file with this identifier exists for this key."))
		return
	}
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if e = s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e = op(ctx, x, res, p); e != nil {
		s.kernel.Refuse(x, w, e)
	}
}

func (s surface) getFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, res *resources.Resource, p *kernel.Pin) *fault.Error {
		x.PinnedHeaders = filesHeaders()
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID), filesQuery())
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.store.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		upstreamID, valid := kernel.NativeResourceID(body, s.kernel.Config.MaxResponseBytes)
		if !valid || upstreamID != res.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a mismatched file identity.")
		}
		metadata, state, expires := fileMetadata(body, p.Model)
		resp.Exchange.Complete(kernel.ClassSuccess)
		if err := s.store.Update(ctx, res.ID, state, metadata, expires); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The file refresh could not be committed.")
		}
		res.Metadata = metadata

		out, err := kernel.RewriteID(body, "id", res.ID)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
		}
		s.kernel.WriteStateJSON(w, x, out, resp.Exchange)
		return nil
	})
}

func (s surface) deleteFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, res *resources.Resource, p *kernel.Pin) *fault.Error {
		x.PinnedHeaders = filesHeaders()
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID), filesQuery())
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.store.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		_ = resp.Body.Close()
		resp.Exchange.Complete(kernel.ClassSuccess)
		if err := s.store.Tombstone(ctx, res.ID); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The file mapping could not be deleted.")
		}

		s.kernel.WriteStateJSON(w, x, kernel.MustJSON(map[string]any{"id": res.ID, "type": "file_deleted"}), resp.Exchange)
		return nil
	})
}

func (s surface) fileContent(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, res *resources.Resource, p *kernel.Pin) *fault.Error {
		x.PinnedHeaders = filesHeaders()
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID)+"/content", filesQuery())
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		x.PinnedBinary = true
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			return kernel.UpstreamError(failure)
		}
		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		artifact, err := resp.StageBody(ctx, s.spool, "file-content", contentType, s.kernel.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}
		resp.Exchange.Complete(kernel.ClassSuccess)
		defer func() { _ = s.spool.Remove(artifact.Handle) }()
		opened, err := s.spool.Open(artifact.Handle)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged provider file could not be read.")
		}
		defer opened.File.Close()
		kernel.SetFileDownloadHeaders(w.Header(), contentType, artifact.ContentLength)
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The results response could not be bounded.")
		}
		s.kernel.DeliverResourceContent(context.Background(), w, x, resp.Exchange, opened.File, artifact.ContentLength, "The results response ended before all bytes were delivered.")
		return nil
	})
}

func fileMetadata(body []byte, model string) (json.RawMessage, string, *time.Time) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	meta := map[string]json.RawMessage{}
	meta["upstream_model"], _ = json.Marshal(model)
	for _, name := range []string{"filename", "mime_type", "size_bytes", "created_at", "downloadable"} {
		if value, ok := fields[name]; ok {
			meta[name] = value
		}
	}
	encoded, _ := json.Marshal(meta)
	return encoded, "available", nil
}

func (s surface) listFiles(w http.ResponseWriter, r *http.Request) {
	s.listResources(w, r, resources.KindAnthropicFile, wire.AnthropicFile)
}

func validateFilesBeta(header http.Header) *fault.Error {
	param := "anthropic-beta"
	for _, line := range header.Values("Anthropic-Beta") {
		for _, beta := range strings.Split(line, ",") {
			if beta = strings.TrimSpace(beta); beta != "" && beta != "files-api-2025-04-14" {
				return fault.InvalidRequest("invalid_request", "Anthropic-Beta may contain only files-api-2025-04-14 for these endpoints.", &param)
			}
		}
	}
	return nil
}
