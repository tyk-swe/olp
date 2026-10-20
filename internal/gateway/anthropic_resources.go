package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

var anthropicCustomID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

const anthropicFilesBeta = "files-api-2025-04-14"

func anthropicFilesQuery() url.Values {
	return url.Values{"beta": []string{"true"}}
}

func (s *Server) registerAnthropicState(mux *http.ServeMux) {
	mux.HandleFunc("POST /anthropic/v1/files", s.anthropicUploadFile)
	mux.HandleFunc("GET /anthropic/v1/files", s.anthropicListFiles)
	mux.HandleFunc("GET /anthropic/v1/files/{id}", s.anthropicGetFile)
	mux.HandleFunc("DELETE /anthropic/v1/files/{id}", s.anthropicDeleteFile)
	mux.HandleFunc("GET /anthropic/v1/files/{id}/content", s.anthropicFileContent)
	mux.HandleFunc("POST /anthropic/v1/messages/batches", s.anthropicCreateBatch)
	mux.HandleFunc("GET /anthropic/v1/messages/batches", s.anthropicListBatches)
	mux.HandleFunc("GET /anthropic/v1/messages/batches/{id}", s.anthropicGetBatch)
	mux.HandleFunc("DELETE /anthropic/v1/messages/batches/{id}", s.anthropicDeleteBatch)
	mux.HandleFunc("POST /anthropic/v1/messages/batches/{id}/cancel", s.anthropicCancelBatch)
	mux.HandleFunc("GET /anthropic/v1/messages/batches/{id}/results", s.anthropicBatchResults)
}

func (s *Server) anthropicBegin(w http.ResponseWriter, r *http.Request, family wire.Format) (*call, access.Authority, bool) {
	x, authority, done := s.stateBegin(w, r, family)
	if done {
		return x, authority, true
	}
	if err := validateAnthropicFilesBeta(r.Header); err != nil {
		s.stateFail(x, w, err, family)
		return x, authority, true
	}
	return x, authority, false
}

func (s *Server) anthropicOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else {
		remote, _, err := net.SplitHostPort(r.RemoteAddr)
		addr, parseErr := netip.ParseAddr(remote)
		trusted := false
		if err == nil && parseErr == nil {
			for _, prefix := range s.cfg.TrustedProxies {
				if prefix.Contains(addr.Unmap()) {
					trusted = true
					break
				}
			}
		}
		if trusted && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
	}
	return scheme + "://" + r.Host
}

func (s *Server) anthropicRoute(x *call, authority access.Authority, r *http.Request, requested string) (*routing.Route, *Error) {
	slug := strings.TrimSpace(r.Header.Get(stateRouteHeader))
	if slug == "" {
		slug = requested
	}
	if slug == "" {
		return nil, invalidRequest("missing_required_parameter", "X-OLP-Route or params.model is required.", nil)
	}
	route, ok := x.snapshot().Routes[slug]
	if !ok {
		return nil, modelNotFound(slug)
	}
	x.route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		return nil, permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
	}
	if !slices.Contains(route.Operations, "batch") {
		return nil, invalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow batch operations.", nil)
	}
	if x.strict() {
		return nil, serverError(http.StatusUnprocessableEntity, "target_capability", "Strict routes do not serve Anthropic files and batches.")
	}
	if e := x.guardrailSurfaceGate(&route); e != nil {
		return nil, e
	}
	return &route, nil
}

func (s *Server) anthropicPin(ctx context.Context, x *call, route *routing.Route) (*pin, *Error) {
	return s.selectPinSurface(ctx, x, route, "batch", "anthropic", "unary", func(p *routing.Provider, model string) bool {
		return p.Connector().Supports("batch", "anthropic", "unary") && p.Supports(model, "batch", "anthropic", "unary")
	})
}

func (s *Server) anthropicUploadFile(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.anthropicBegin(w, r, wire.AnthropicFile)
	if done {
		return
	}
	defer s.release(r.Context())
	route, err := s.anthropicRoute(x, authority, r, "")
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	form, err := s.parseMediaForm(w, r, authority.ID, s.cfg.MaxMediaBodyBytes, 1)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	defer form.Cleanup()
	file, mediaErr := form.TakeSingleFile("file")
	if mediaErr != nil || file == nil {
		s.stateFail(x, w, invalidRequest("invalid_request", "Upload exactly one file part.", nil), x.family)
		return
	}
	extra, mediaErr := form.TakeExtensions()
	if mediaErr != nil || len(extra) != 0 {
		s.stateFail(x, w, invalidRequest("invalid_request", "Anthropic file uploads accept only one file part.", nil), x.family)
		return
	}
	form.Disarm()
	defer s.transport().Spool.Remove(file.Handle)
	p, err := s.anthropicPin(r.Context(), x, route)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.unboundedPinLimits(x.admissionAuthority(authority), p, route); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if p.hold == nil {
		gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
		if gate.verdict != gateAdmitted {
			s.stateFail(x, w, gateError(gate), x.family)
			return
		}
		p.hold = gate.hold
	}
	x.anthropicFilesBeta = true
	endpoint, urlErr := resourceURL(p.provider.Connector(), p.model, "files", anthropicFilesQuery())
	if urlErr != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."), x.family)
		return
	}
	resp, failure := s.uploadMultipart(ctx, x, p, endpoint, "", nil, file, false)
	if failure != nil {
		s.stateFail(x, w, upstreamError(failure), x.family)
		return
	}
	defer resp.Body.Close()
	x.dispatched = true
	body, readErr := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if readErr != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."), x.family)
		return
	}
	upstreamID, valid := nativeResourceID(body, s.cfg.MaxResponseBytes)
	if !valid {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."), x.family)
		return
	}
	metadata, state, expires := anthropicFileMetadata(body, p.model)
	commitCtx, stopCommit := resourceCommitContext(ctx)
	defer stopCommit()
	res, putErr := s.Resources.Put(commitCtx, &resources.Resource{
		Kind: resources.KindAnthropicFile, APIKeyID: authority.ID, RouteSlug: route.Slug,
		ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: route.RevisionID,
		TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID,
		UpstreamID: upstreamID, State: state, Metadata: metadata, ExpiresAt: expires,
	})
	if putErr != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be stored."), x.family)
		return
	}
	x.completePinned()
	out, _ := rewriteID(body, "id", res.ID)
	s.writeStateJSON(w, x, out)
}

func (s *Server) anthropicFileCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, *resources.Resource, *pin) *Error) {
	x, authority, done := s.anthropicBegin(w, r, wire.AnthropicFile)
	if done {
		return
	}
	defer s.release(r.Context())
	res, err := s.Resources.Get(r.Context(), resources.KindAnthropicFile, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No file with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."), x.family)
		return
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, res, "batch", use)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if !p.provider.Connector().Supports("batch", "anthropic", "unary") || !p.provider.Supports(p.model, "batch", "anthropic", "unary") {
		s.stateFail(x, w, notFoundError("not_found", "No file with this identifier exists for this key."), x.family)
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e = s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if p.hold == nil {
		gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
		if gate.verdict != gateAdmitted {
			s.stateFail(x, w, gateError(gate), x.family)
			return
		}
		p.hold = gate.hold
	}
	if e = op(ctx, x, res, p); e != nil {
		s.stateFail(x, w, e, x.family)
	}
}

func (s *Server) anthropicGetFile(w http.ResponseWriter, r *http.Request) {
	s.anthropicFileCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, res *resources.Resource, p *pin) *Error {
		x.anthropicFilesBeta = true
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "files/"+url.PathEscape(res.UpstreamID), anthropicFilesQuery())
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.Resources.Tombstone(ctx, res.ID)
				return notFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		upstreamID, valid := nativeResourceID(body, s.cfg.MaxResponseBytes)
		if !valid || upstreamID != res.UpstreamID {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a mismatched file identity.")
		}
		metadata, state, expires := anthropicFileMetadata(body, p.model)
		if err := s.Resources.Update(ctx, res.ID, state, metadata, expires); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The file refresh could not be committed.")
		}
		res.Metadata = metadata
		x.dispatched = true
		x.completePinned()
		out, err := rewriteID(body, "id", res.ID)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
		}
		s.writeStateJSON(w, x, out)
		return nil
	})
}

func (s *Server) anthropicDeleteFile(w http.ResponseWriter, r *http.Request) {
	s.anthropicFileCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, res *resources.Resource, p *pin) *Error {
		x.anthropicFilesBeta = true
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "files/"+url.PathEscape(res.UpstreamID), anthropicFilesQuery())
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.Resources.Tombstone(ctx, res.ID)
				return notFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		_ = resp.Body.Close()
		if err := s.Resources.Tombstone(ctx, res.ID); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The file mapping could not be deleted.")
		}
		x.dispatched = true
		x.completePinned()
		s.writeStateJSON(w, x, mustJSON(map[string]any{"id": res.ID, "type": "file_deleted"}))
		return nil
	})
}

func (s *Server) anthropicFileContent(w http.ResponseWriter, r *http.Request) {
	s.anthropicFileCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, res *resources.Resource, p *pin) *Error {
		x.anthropicFilesBeta = true
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "files/"+url.PathEscape(res.UpstreamID)+"/content", anthropicFilesQuery())
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		x.anthropicFileBinary = true
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		artifact, err := s.transport().Spool.Put(ctx, media.Upload{Filename: "file-content", ContentType: contentType, MaximumLength: s.cfg.MaxResponseBytes, Body: resp.Body})
		if err != nil {
			return serverError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}
		defer s.transport().Spool.Remove(artifact.Handle)
		opened, err := s.transport().Spool.Open(artifact.Handle)
		if err != nil {
			return serverError(http.StatusBadGateway, "media_content_unavailable", "The staged provider file could not be read.")
		}
		defer opened.File.Close()
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", fmt.Sprint(artifact.ContentLength))
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
			return serverError(http.StatusInternalServerError, "internal_error", "The results response could not be bounded.")
		}
		x.responseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := io.Copy(w, opened.File)
		x.completePinned()
		if copyErr != nil || written != artifact.ContentLength {
			x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The results response ended before all bytes were delivered.")
			s.finish(x, &outcome{err: x.failure, committed: true, cancelled: true}, x.failure.Status)
			return nil
		}
		if written > 0 {
			x.delivered(s.now())
		}
		s.finish(x, &outcome{committed: true}, http.StatusOK)
		return nil
	})
}

func anthropicFileMetadata(body []byte, model string) (json.RawMessage, string, *time.Time) {
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

func (s *Server) anthropicListBatches(w http.ResponseWriter, r *http.Request) {
	s.anthropicListResources(w, r, resources.KindAnthropicBatch, wire.AnthropicBatch)
}

func (s *Server) anthropicCreateBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.anthropicBegin(w, r, wire.AnthropicBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	body, err := s.readBody(r)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	requestedRoute := strings.TrimSpace(r.Header.Get(stateRouteHeader))
	if requestedRoute == "" {
		requestedRoute, _ = anthropicCommonModel(body)
	}
	route, err := s.anthropicRoute(x, authority, r, requestedRoute)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	if _, _, _, validationErr := validateAnthropicBatchRequest(body, route.Slug, ""); validationErr != nil {
		s.stateFail(x, w, validationErr, x.family)
		return
	}
	p, _, e := s.anthropicSelectPinWithFiles(r.Context(), x, authority, route, body)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if x.route != nil {
		route = x.route
	}
	translated, fileIDs, customIDs, e := validateAnthropicBatchRequest(body, route.Slug, p.model)
	if e != nil {
		s.releaseHold(r.Context(), p.hold)
		s.stateFail(x, w, e, x.family)
		return
	}
	if len(fileIDs) > 0 {
		rewrites := map[string]string{}
		for _, id := range fileIDs {
			file, getErr := s.Resources.Get(r.Context(), resources.KindAnthropicFile, authority.ID, id)
			if getErr != nil || file.RouteSlug != route.Slug {
				s.releaseHold(r.Context(), p.hold)
				s.stateFail(x, w, notFoundError("not_found", "No file with this identifier exists for this key."), x.family)
				return
			}
			rewrites[id] = file.UpstreamID
		}
		var rewriteErr error
		translated, rewriteErr = replaceAnthropicFileReferences(translated, rewrites)
		if rewriteErr != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("invalid_request", "A referenced file could not be bound.", nil), x.family)
			return
		}
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e = s.unboundedPinLimits(x.admissionAuthority(authority), p, route); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if e = x.guardrailSurfaceGate(route); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if e = s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if p.hold == nil {
		gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
		if gate.verdict != gateAdmitted {
			s.stateFail(x, w, gateError(gate), x.family)
			return
		}
		p.hold = gate.hold
	}
	if len(fileIDs) > 0 {
		x.anthropicFilesBeta = true
	}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if parseErr != nil {
		s.stateFail(x, w, invalidRequest("invalid_request", "The request body is invalid.", nil), x.family)
		return
	}
	endpoint, urlErr := resourceURL(p.provider.Connector(), p.model, "messages/batches", nil)
	if urlErr != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."), x.family)
		return
	}
	cursor := json.RawMessage(`{"submission":{"anthropic":true}}`)
	expected := int64(len(customIDs))
	submission, e := s.prepareBatchSubmission(ctx, x, authority.ID, p, source, source.Bytes(), translated, oif.ServingIdentity{},
		"", "", resources.KindAnthropicBatch, "anthropic", "anthropic", "generation", 24*time.Hour, &expected, cursor)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	resp, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, translated, "application/json")
	if failure != nil {
		x.dispatched = failure.dispatched
		if failure.status == 0 && failure.dispatched {
			commit, stop := resourceCommitContext(r.Context())
			defer stop()
			_ = s.Batches.MarkUnknown(commit, submission.resource.UUID, "submission_unknown", s.now())
			s.stateFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The provider may have accepted this batch, but its outcome is unknown."), x.family)
			return
		}
		commit, stop := resourceCommitContext(r.Context())
		defer stop()
		_ = s.Batches.DeleteSubmission(commit, submission.resource.UUID)
		s.stateFail(x, w, upstreamError(failure), x.family)
		return
	}
	defer resp.Body.Close()
	x.dispatched = true
	result, readErr := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if readErr != nil {
		commit, stop := resourceCommitContext(r.Context())
		defer stop()
		_ = s.Batches.DeleteSubmission(commit, submission.resource.UUID)
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."), x.family)
		return
	}
	localID := resources.LocalID(resources.KindAnthropicBatch, submission.resource.UUID)
	projected, projectionErr := projectAnthropicBatch(result, localID, s.anthropicOrigin(r))
	if projectionErr != nil {
		commit, stop := resourceCommitContext(r.Context())
		defer stop()
		_ = s.Batches.DeleteSubmission(commit, submission.resource.UUID)
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object."), x.family)
		return
	}
	upstreamID, valid := nativeResourceID(result, s.cfg.MaxResponseBytes)
	if !valid {
		commit, stop := resourceCommitContext(r.Context())
		defer stop()
		_ = s.Batches.DeleteSubmission(commit, submission.resource.UUID)
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch identity."), x.family)
		return
	}
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(projected, &metadata)
	delete(metadata, "id")
	metadata["upstream_model"], _ = json.Marshal(p.model)
	resource := submission.resource
	resource.UpstreamID = upstreamID
	resource.State = anthropicBatchState(result)
	resource.Metadata = mustJSON(metadata)
	commit, stop := resourceCommitContext(ctx)
	defer stop()
	tx, beginErr := s.Resources.Begin(commit)
	if beginErr != nil {
		_ = s.Batches.MarkUnknown(commit, resource.UUID, "submission_commit_unavailable", s.now())
		s.stateFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The accepted batch could not be stored."), x.family)
		return
	}
	defer tx.Rollback(commit)
	_, commitErr := s.Resources.PutTx(commit, tx, resource)
	if commitErr == nil {
		commitErr = s.Batches.ActivateTx(commit, tx, resource.UUID, upstreamID, &expected)
	}
	if commitErr == nil {
		commitErr = tx.Commit(commit)
	}
	if commitErr != nil {
		_ = s.Batches.MarkUnknown(commit, resource.UUID, "submission_commit_unavailable", s.now())
		s.stateFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The accepted batch could not be confirmed locally."), x.family)
		return
	}
	x.completePinned()
	s.writeStateJSON(w, x, projected)
}

func anthropicCommonModel(body []byte) (string, bool) {
	var object struct {
		Requests []struct {
			Params struct {
				Model string `json:"model"`
			} `json:"params"`
		} `json:"requests"`
	}
	if json.Unmarshal(body, &object) != nil || len(object.Requests) == 0 {
		return "", false
	}
	model := object.Requests[0].Params.Model
	for _, request := range object.Requests[1:] {
		if request.Params.Model != model {
			return "", false
		}
	}
	return model, model != ""
}

func (s *Server) anthropicSelectPinWithFiles(ctx context.Context, x *call, authority access.Authority, route *routing.Route, body []byte) (*pin, []*resources.Resource, *Error) {
	refs, refErr := anthropicBatchFileReferences(body)
	if refErr != nil {
		return nil, nil, invalidRequest("invalid_request", "The request contains an invalid file source.", nil)
	}
	ids := uniqueStrings(refs)
	files := make([]*resources.Resource, 0, len(ids))
	for _, id := range ids {
		file, err := s.Resources.Get(ctx, resources.KindAnthropicFile, authority.ID, id)
		if err != nil || file.RouteSlug != route.Slug {
			return nil, nil, notFoundError("not_found", "No file with this identifier exists for this key.")
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		p, err := s.anthropicPin(ctx, x, route)
		return p, files, err
	}
	if !sameAnthropicAffinity(files) {
		return nil, nil, invalidRequest("invalid_request", "Referenced files must share the same provider affinity.", nil)
	}
	p, pinnedRoute, err := s.resolveResource(ctx, x, authority, files[0], "batch", retainedNewWork)
	if err != nil || pinnedRoute.Slug != route.Slug {
		return nil, nil, notFoundError("not_found", "No file with this identifier exists for this key.")
	}
	for _, file := range files[1:] {
		if file.ProviderID != files[0].ProviderID || file.TargetID != files[0].TargetID || file.SlotID != files[0].SlotID || !equalCredentialID(file.CredentialID, files[0].CredentialID) {
			return nil, nil, invalidRequest("invalid_request", "Referenced files must share the same provider affinity.", nil)
		}
	}
	x.route = pinnedRoute
	return p, files, nil
}

func anthropicBatchFileReferences(body []byte) ([]string, error) {
	var request struct {
		Requests []anthropicBatchRequest `json:"requests"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	refs := []string{}
	for _, item := range request.Requests {
		itemRefs, err := anthropicFileReferences(item.Params)
		if err != nil {
			return nil, err
		}
		refs = append(refs, itemRefs...)
	}
	return uniqueStrings(refs), nil
}

func (s *Server) anthropicListFiles(w http.ResponseWriter, r *http.Request) {
	s.anthropicListResources(w, r, resources.KindAnthropicFile, wire.AnthropicFile)
}

func mustJSON(value any) []byte {
	out, _ := json.Marshal(value)
	return out
}

func (s *Server) anthropicListResources(w http.ResponseWriter, r *http.Request, kind string, family wire.Format) {
	x, authority, done := s.anthropicBegin(w, r, family)
	if done {
		return
	}
	defer s.release(r.Context())
	limit, before, cursor, err := anthropicListCursor(r.URL.Query(), kind)
	if err != nil {
		s.stateFail(x, w, err, family)
		return
	}
	var rows []*resources.Resource
	var listErr error
	if before != "" {
		rows, listErr = s.Resources.ListKindsBefore(r.Context(), []string{kind}, authority.ID, limit+1, before)
	} else {
		rows, listErr = s.Resources.ListKinds(r.Context(), []string{kind}, authority.ID, limit+1, cursor)
	}
	if listErr != nil {
		if errors.Is(listErr, resources.ErrNotFound) {
			param := "after_id"
			if before != "" {
				param = "before_id"
			}
			s.stateFail(x, w, invalidRequest("invalid_value", "The cursor must identify a resource owned by this key.", &param), family)
			return
		}
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The resource list could not be read."), family)
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items, ids := make([]json.RawMessage, 0, len(rows)), make([]string, 0, len(rows))
	for _, row := range rows {
		object, err := s.anthropicResourceObject(row, kind, r)
		if err != nil {
			s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A resource could not be projected."), family)
			return
		}
		items, ids = append(items, object), append(ids, row.ID)
	}
	s.writeStateJSON(w, x, anthropicListBody(items, ids, hasMore))
}

func anthropicListBody(items []json.RawMessage, ids []string, hasMore bool) []byte {
	body := map[string]any{"data": items, "has_more": hasMore}
	if len(ids) > 0 {
		body["first_id"], body["last_id"] = ids[0], ids[len(ids)-1]
	}
	return mustJSON(body)
}

func (s *Server) anthropicResourceObject(res *resources.Resource, kind string, r *http.Request) ([]byte, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(res.Metadata, &fields) != nil || fields == nil {
		fields = map[string]json.RawMessage{}
	}
	delete(fields, "upstream_model")
	if kind == resources.KindAnthropicBatch {
		fields["id"], _ = json.Marshal(res.ID)
		if raw, ok := fields["results_url"]; ok && string(raw) != "null" {
			fields["results_url"], _ = json.Marshal(s.anthropicOrigin(r) + "/anthropic/v1/messages/batches/" + url.PathEscape(res.ID) + "/results")
		}
		return json.Marshal(fields)
	}
	fields["id"], _ = json.Marshal(res.ID)
	if _, ok := fields["type"]; !ok {
		fields["type"] = json.RawMessage(`"file"`)
	}
	return json.Marshal(fields)
}

func (s *Server) anthropicBatchCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, access.Authority, *resources.Resource, *pin) *Error) {
	x, authority, done := s.anthropicBegin(w, r, wire.AnthropicBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	res, err := s.Resources.Get(r.Context(), resources.KindAnthropicBatch, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No message batch with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The batch mapping could not be read."), x.family)
		return
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, res, "batch", use)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if !p.provider.Connector().Supports("batch", "anthropic", "unary") || !p.provider.Supports(p.model, "batch", "anthropic", "unary") {
		s.stateFail(x, w, notFoundError("not_found", "No message batch with this identifier exists for this key."), x.family)
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e = s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
	if gate.verdict != gateAdmitted {
		s.stateFail(x, w, gateError(gate), x.family)
		return
	}
	p.hold = gate.hold
	if e = op(ctx, x, authority, res, p); e != nil {
		s.stateFail(x, w, e, x.family)
	}
}

func (s *Server) anthropicBatchRequest(w http.ResponseWriter, r *http.Request, use retainedUse, method string, cancel bool) {
	s.anthropicBatchCall(w, r, use, func(ctx context.Context, x *call, _ access.Authority, res *resources.Resource, p *pin) *Error {
		path := "messages/batches/" + url.PathEscape(res.UpstreamID)
		if cancel {
			path += "/cancel"
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, path, nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, method, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				return notFoundError("not_found", "No message batch with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		upstreamID, valid := nativeResourceID(body, s.cfg.MaxResponseBytes)
		if !valid || upstreamID != res.UpstreamID {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a mismatched batch identity.")
		}
		projected, err := projectAnthropicBatch(body, res.ID, s.anthropicOrigin(r))
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
		}
		var stored map[string]json.RawMessage
		_ = json.Unmarshal(projected, &stored)
		delete(stored, "id")
		stored["upstream_model"], _ = json.Marshal(resourceModel(res))
		metadata := mustJSON(stored)
		if err := s.Resources.Update(ctx, res.ID, anthropicBatchState(body), metadata, nil); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The batch refresh could not be committed.")
		}
		res.Metadata = metadata
		x.dispatched = true
		x.completePinned()
		s.writeStateJSON(w, x, projected)
		return nil
	})
}

func anthropicBatchState(body []byte) string {
	var object struct {
		ProcessingStatus string `json:"processing_status"`
	}
	_ = json.Unmarshal(body, &object)
	return object.ProcessingStatus
}

func (s *Server) anthropicGetBatch(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchRequest(w, r, retainedRetrieval, http.MethodGet, false)
}

func (s *Server) anthropicCancelBatch(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchRequest(w, r, retainedHousekeeping, http.MethodPost, true)
}

func (s *Server) anthropicDeleteBatch(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, _ access.Authority, res *resources.Resource, p *pin) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "messages/batches/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.Resources.Tombstone(ctx, res.ID)
				return notFoundError("not_found", "No message batch with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		body, readErr := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		if err := s.Resources.Tombstone(ctx, res.ID); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The batch mapping could not be deleted.")
		}
		x.dispatched = true
		x.completePinned()
		projected, projectErr := projectAnthropicBatch(body, res.ID, s.anthropicOrigin(r))
		if projectErr != nil {
			projected = mustJSON(map[string]string{"id": res.ID, "type": "message_batch_deleted"})
		}
		s.writeStateJSON(w, x, projected)
		return nil
	})
}

func (s *Server) anthropicBatchResults(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, _ access.Authority, res *resources.Resource, p *pin) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "messages/batches/"+url.PathEscape(res.UpstreamID)+"/results", nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/jsonl"
		}
		artifact, err := s.transport().Spool.Put(ctx, media.Upload{Filename: "batch-results", ContentType: contentType, MaximumLength: s.cfg.MaxResponseBytes, Body: resp.Body})
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_response_too_large", "The results exceeded the configured response bound.")
		}
		defer s.transport().Spool.Remove(artifact.Handle)
		opened, err := s.transport().Spool.Open(artifact.Handle)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The staged results could not be read.")
		}
		defer opened.File.Close()
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", fmt.Sprint(artifact.ContentLength))
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
			return serverError(http.StatusInternalServerError, "internal_error", "The results response could not be bounded.")
		}
		x.completePinned()
		x.responseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := io.Copy(w, opened.File)
		if copyErr != nil || written != artifact.ContentLength {
			x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The results response ended before all bytes were delivered.")
			s.finish(x, &outcome{err: x.failure, committed: true, cancelled: true}, x.failure.Status)
			return nil
		}
		if written > 0 {
			x.delivered(s.now())
		}
		s.finish(x, &outcome{committed: true}, http.StatusOK)
		return nil
	})
}

type anthropicBatchRequest struct {
	CustomID string          `json:"custom_id"`
	Params   json.RawMessage `json:"params"`
}

func validateAnthropicBatchRequest(body []byte, routeSlug, targetModel string) ([]byte, []string, []string, *Error) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil || source.Root().Kind() != oif.Object || len(source.Root().Members()) != 1 || source.Root().Members()[0].Name != "requests" {
		return nil, nil, nil, invalidRequest("invalid_request", "The request body must contain only requests.", nil)
	}
	requestsValue, present := source.Root().Lookup("requests")
	elements := requestsValue.Elements()
	if !present || requestsValue.Kind() != oif.Array || len(elements) < 1 || len(elements) > 100000 {
		return nil, nil, nil, invalidRequest("invalid_request", "requests must contain between 1 and 100000 entries.", nil)
	}
	var requests []anthropicBatchRequest
	if json.Unmarshal(body, &struct {
		Requests *[]anthropicBatchRequest `json:"requests"`
	}{Requests: &requests}) != nil || len(requests) != len(elements) {
		return nil, nil, nil, invalidRequest("invalid_request", "Every request must contain custom_id and params.", nil)
	}
	seen := make(map[string]struct{}, len(requests))
	fileIDs := []string{}
	model := routeSlug
	for i, request := range requests {
		members := elements[i].Members()
		if elements[i].Kind() != oif.Object || len(members) != 2 {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d] must contain only custom_id and params.", i), nil)
		}
		memberNames := map[string]bool{}
		for _, member := range members {
			memberNames[member.Name] = true
		}
		if !memberNames["custom_id"] || !memberNames["params"] {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d] must contain custom_id and params.", i), nil)
		}
		if !anthropicCustomID.MatchString(request.CustomID) {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].custom_id is invalid.", i), nil)
		}
		if _, exists := seen[request.CustomID]; exists {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].custom_id is duplicated.", i), nil)
		}
		seen[request.CustomID] = struct{}{}
		params, parseErr := oif.ParseJSON(request.Params, oif.Limits{MaxBytes: len(request.Params)})
		if parseErr != nil || params.Root().Kind() != oif.Object {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params must be an object.", i), nil)
		}
		if stream, ok := params.Root().Lookup("stream"); ok && stream.Raw() == "true" {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params.stream is unsupported.", i), nil)
		}
		modelValue, hasModel := params.Root().Lookup("model")
		requestModel, _ := modelValue.Text()
		parseModel := routeSlug
		if parseModel == "" {
			parseModel = requestModel
		}
		if parsed, parseErr := protocols.Parse(wire.Anthropic, request.Params, parseModel); parseErr != nil || parsed == nil {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params is invalid.", i), nil)
		}
		if routeSlug == "" {
			if !hasModel || requestModel == "" || i > 0 && model != requestModel {
				return nil, nil, nil, invalidRequest("invalid_request", "A single common params.model or X-OLP-Route is required.", nil)
			}
			model = requestModel
		}
		if requestModel == "" || targetModel != "" && requestModel != routeSlug && requestModel != targetModel {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params.model does not match the route.", i), nil)
		}
		refs, refErr := anthropicFileReferences(request.Params)
		if refErr != nil {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params contains an invalid file source.", i), nil)
		}
		fileIDs = append(fileIDs, refs...)
	}
	var object map[string]json.RawMessage
	_ = json.Unmarshal(body, &object)
	for i := range requests {
		var params map[string]json.RawMessage
		_ = json.Unmarshal(requests[i].Params, &params)
		if targetModel != "" {
			params["model"], _ = json.Marshal(targetModel)
		}
		requests[i].Params, _ = json.Marshal(params)
	}
	encodedRequests, _ := json.Marshal(requests)
	object["requests"] = encodedRequests
	encoded, _ := json.Marshal(object)
	return encoded, uniqueStrings(fileIDs), requestsCustomIDs(requests), nil
}

func anthropicFileReferences(body []byte) ([]string, error) {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil || value == nil {
		return nil, fmt.Errorf("invalid Anthropic request")
	}
	refs := []string{}
	var visit func(any) error
	visit = func(node any) error {
		switch item := node.(type) {
		case map[string]any:
			source, ok := item["source"].(map[string]any)
			if ok && source["type"] == "file" {
				id, ok := source["file_id"].(string)
				if !ok || id == "" || len(source) != 2 {
					return fmt.Errorf("invalid Anthropic file source")
				}
			}
			if ok && source["type"] == "file" {
				id := source["file_id"].(string)
				refs = append(refs, id)
			}
			for key, child := range item {
				if key == "source" {
					continue
				}
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range item {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, field := range []string{"system", "messages"} {
		if node, present := value[field]; present {
			if err := visit(node); err != nil {
				return nil, err
			}
		}
	}
	return refs, nil
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func requestsCustomIDs(requests []anthropicBatchRequest) []string {
	out := make([]string, len(requests))
	for i := range requests {
		out[i] = requests[i].CustomID
	}
	return out
}

func projectAnthropicBatch(body []byte, localID, origin string) ([]byte, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return nil, fmt.Errorf("malformed Anthropic batch")
	}
	object["id"], _ = json.Marshal(localID)
	if raw, ok := object["results_url"]; ok && string(raw) != "null" {
		var path string
		if json.Unmarshal(raw, &path) != nil {
			return nil, fmt.Errorf("malformed Anthropic results URL")
		}
		_, err := url.Parse(path)
		if err != nil {
			return nil, fmt.Errorf("malformed Anthropic results URL")
		}
		object["results_url"], _ = json.Marshal(origin + "/anthropic/v1/messages/batches/" + url.PathEscape(localID) + "/results")
	}
	return json.Marshal(object)
}

func validateAnthropicFilesBeta(header http.Header) *Error {
	param := "anthropic-beta"
	for _, line := range header.Values("Anthropic-Beta") {
		for _, beta := range strings.Split(line, ",") {
			if beta = strings.TrimSpace(beta); beta != "" && beta != "files-api-2025-04-14" {
				return invalidRequest("invalid_request", "Anthropic-Beta may contain only files-api-2025-04-14 for these endpoints.", &param)
			}
		}
	}
	return nil
}

func anthropicListCursor(query url.Values, kind string) (int, string, string, *Error) {
	before, after := query.Get("before_id"), query.Get("after_id")
	if before != "" && after != "" {
		return 0, "", "", invalidRequest("invalid_request", "before_id and after_id cannot be used together.", nil)
	}
	limit := 20
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			param := "limit"
			return 0, "", "", invalidRequest("invalid_value", "limit must be an integer between 1 and 1000.", &param)
		}
		limit = parsed
	}
	cursor := before
	if cursor == "" {
		cursor = after
	}
	if cursor != "" && !validAnthropicCursor(kind, cursor) {
		param := "after_id"
		if before != "" {
			param = "before_id"
		}
		return 0, "", "", invalidRequest("invalid_value", param+" must be a local resource identifier.", &param)
	}
	return limit, before, cursor, nil
}

func validAnthropicCursor(kind, value string) bool {
	prefix := kind + "_"
	raw := strings.TrimPrefix(value, prefix)
	if raw == value || len(raw) != 32 {
		return false
	}
	id, err := uuid.Parse(raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:])
	return err == nil && resources.LocalID(kind, id) == value
}

func sameAnthropicAffinity(resources []*resources.Resource) bool {
	if len(resources) < 2 {
		return true
	}
	first := resources[0]
	for _, file := range resources[1:] {
		if file.ProviderID != first.ProviderID || file.TargetID != first.TargetID || file.SlotID != first.SlotID ||
			!equalCredentialID(file.CredentialID, first.CredentialID) {
			return false
		}
	}
	return true
}

func equalCredentialID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func replaceAnthropicFileReferences(body []byte, upstream map[string]string) ([]byte, error) {
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("invalid Anthropic request")
	}
	var visit func(any) error
	visit = func(node any) error {
		switch item := node.(type) {
		case map[string]any:
			source, ok := item["source"].(map[string]any)
			if ok && source["type"] == "file" {
				id, _ := source["file_id"].(string)
				up, found := upstream[id]
				if !found {
					return fmt.Errorf("unknown file")
				}
				source["file_id"] = up
			}
			for key, child := range item {
				if key == "source" {
					continue
				}
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range item {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if requests, ok := value["requests"].([]any); ok {
		for _, item := range requests {
			request, _ := item.(map[string]any)
			params, _ := request["params"].(map[string]any)
			for _, field := range []string{"system", "messages"} {
				if node, present := params[field]; present {
					if err := visit(node); err != nil {
						return nil, err
					}
				}
			}
		}
	} else {
		for _, field := range []string{"system", "messages"} {
			if node, present := value[field]; present {
				if err := visit(node); err != nil {
					return nil, err
				}
			}
		}
	}
	return json.Marshal(value)
}
