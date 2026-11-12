package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
)

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

func (s *Server) anthropicBegin(w http.ResponseWriter, r *http.Request, family wire.Format) (*call, keyauthority.Authority, bool) {
	x, authority, done := s.stateBegin(w, r, family)
	if done {
		return x, authority, true
	}
	if err := validateAnthropicFilesBeta(r.Header); err != nil {
		s.release(r.Context())
		s.refuse(x, w, err)
		return x, authority, true
	}
	return x, authority, false
}

func (s *Server) anthropicRoute(x *call, authority keyauthority.Authority, r *http.Request, requested string) (*routing.Route, *Error) {
	slug := strings.TrimSpace(r.Header.Get(stateRouteHeader))
	if slug == "" {
		slug = requested
	}
	if slug == "" {
		return nil, invalidRequest("missing_required_parameter", "X-OLP-Route or params.model is required.", nil)
	}
	route, ok := x.snapshot().FindRoute(slug)
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
		return nil, policyUnavailable("target_capability", "Strict routes do not serve Anthropic files and batches.")
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
		s.refuse(x, w, err)
		return
	}
	form, err := s.parseMediaForm(w, r, authority.ID, s.cfg.MaxMediaBodyBytes, 1)
	if err != nil {
		s.refuse(x, w, err)
		return
	}
	defer form.Cleanup()
	file := form.TakeSingleFile("file")
	if form.Err() != nil || file == nil {
		s.refuse(x, w, invalidRequest("invalid_request", "Upload exactly one file part.", nil))
		return
	}
	if extra := form.TakeExtensions(); form.Err() != nil || len(extra) != 0 {
		s.refuse(x, w, invalidRequest("invalid_request", "Anthropic file uploads accept only one file part.", nil))
		return
	}
	form.Disarm()
	defer s.deps.Staging.Remove(file.Handle)
	p, err := s.anthropicPin(r.Context(), x, route)
	if err != nil {
		s.refuse(x, w, err)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.unboundedPinLimits(x.admissionAuthority(authority), p, route); e != nil {
		s.refuse(x, w, e)
		return
	}
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.refuse(x, w, e)
		return
	}
	if p.hold == nil {
		gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
		if gate.verdict != gateAdmitted {
			s.refuse(x, w, gateError(gate))
			return
		}
		p.hold = gate.hold
	}
	x.anthropicFilesBeta = true
	endpoint, urlErr := resourceURL(p.provider.Connector(), p.model, "files", anthropicFilesQuery())
	if urlErr != nil {
		s.refuse(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."))
		return
	}
	resp, failure := s.uploadMultipart(ctx, x, p, endpoint, "", nil, file, false)
	if failure != nil {
		s.refuse(x, w, upstreamError(failure))
		return
	}
	defer resp.Body.Close()
	x.dispatched = true
	body, readErr := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if readErr != nil {
		s.refuse(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."))
		return
	}
	upstreamID, valid := nativeResourceID(body, s.cfg.MaxResponseBytes)
	if !valid {
		s.refuse(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."))
		return
	}
	metadata, state, expires := anthropicFileMetadata(body, p.model)
	commitCtx, stopCommit := resourceCommitContext(ctx)
	defer stopCommit()
	res, putErr := s.deps.Resources.Put(commitCtx, &resources.Resource{
		Kind: resources.KindAnthropicFile, APIKeyID: authority.ID, RouteSlug: route.Slug,
		ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: route.RevisionID,
		TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID,
		UpstreamID: upstreamID, State: state, Metadata: metadata, ExpiresAt: expires,
	})
	if putErr != nil {
		s.refuse(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be stored."))
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
	res, err := s.deps.Resources.Get(r.Context(), resources.KindAnthropicFile, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.refuse(x, w, notFoundError("not_found", "No file with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.refuse(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."))
		return
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, res, "batch", use)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	if !p.provider.Connector().Supports("batch", "anthropic", "unary") || !p.provider.Supports(p.model, "batch", "anthropic", "unary") {
		s.refuse(x, w, notFoundError("not_found", "No file with this identifier exists for this key."))
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e = s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.refuse(x, w, e)
		return
	}
	if p.hold == nil {
		gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
		if gate.verdict != gateAdmitted {
			s.refuse(x, w, gateError(gate))
			return
		}
		p.hold = gate.hold
	}
	if e = op(ctx, x, res, p); e != nil {
		s.refuse(x, w, e)
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
				_ = s.deps.Resources.Tombstone(ctx, res.ID)
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
		if err := s.deps.Resources.Update(ctx, res.ID, state, metadata, expires); err != nil {
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
				_ = s.deps.Resources.Tombstone(ctx, res.ID)
				return notFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		_ = resp.Body.Close()
		if err := s.deps.Resources.Tombstone(ctx, res.ID); err != nil {
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
		artifact, err := s.deps.Staging.Put(ctx, staging.Upload{Filename: "file-content", ContentType: contentType, MaximumLength: s.cfg.MaxResponseBytes, Body: resp.Body})
		if err != nil {
			return serverError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}
		defer s.deps.Staging.Remove(artifact.Handle)
		opened, err := s.deps.Staging.Open(artifact.Handle)
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
		written, copyErr := copyResourceContent(w, opened.File, artifact.ContentLength)
		x.completePinned()
		if copyErr != nil || written != artifact.ContentLength {
			if len(x.facts) > 0 {
				x.facts[len(x.facts)-1].Class = classCancelled
			}
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

func (s *Server) anthropicListFiles(w http.ResponseWriter, r *http.Request) {
	s.anthropicListResources(w, r, resources.KindAnthropicFile, wire.AnthropicFile)
}

func (s *Server) anthropicListResources(w http.ResponseWriter, r *http.Request, kind string, family wire.Format) {
	x, authority, done := s.anthropicBegin(w, r, family)
	if done {
		return
	}
	defer s.release(r.Context())
	limit, before, cursor, err := anthropicListCursor(r.URL.Query(), kind)
	if err != nil {
		s.refuse(x, w, err)
		return
	}
	var rows []*resources.Resource
	var listErr error
	if before != "" {
		rows, listErr = s.deps.Resources.ListKindsBefore(r.Context(), []string{kind}, authority.ID, limit+1, before)
	} else {
		rows, listErr = s.deps.Resources.ListKinds(r.Context(), []string{kind}, authority.ID, limit+1, cursor)
	}
	if listErr != nil {
		if errors.Is(listErr, resources.ErrNotFound) {
			param := "after_id"
			if before != "" {
				param = "before_id"
			}
			s.refuse(x, w, invalidRequest("invalid_value", "The cursor must identify a resource owned by this key.", &param))
			return
		}
		s.refuse(x, w, serverError(http.StatusInternalServerError, "internal_error", "The resource list could not be read."))
		return
	}
	rows, hasMore := anthropicListPage(rows, limit, before != "")
	items, ids := make([]json.RawMessage, 0, len(rows)), make([]string, 0, len(rows))
	for _, row := range rows {
		if authErr := s.authorizeListedResource(r.Context(), authority, row); authErr != nil {
			s.refuse(x, w, authErr)
			return
		}
		object, err := s.anthropicResourceObject(row, kind, r)
		if err != nil {
			s.refuse(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A resource could not be projected."))
			return
		}
		items, ids = append(items, object), append(ids, row.ID)
	}
	s.writeStateJSON(w, x, anthropicListBody(items, ids, hasMore))
}

func anthropicListPage(rows []*resources.Resource, limit int, before bool) ([]*resources.Resource, bool) {
	if len(rows) <= limit {
		return rows, false
	}
	if before {
		return rows[len(rows)-limit:], true
	}
	return rows[:limit], true
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
	if err := json.Unmarshal(res.Metadata, &fields); err != nil || fields == nil {
		return nil, errors.New("stored Anthropic resource metadata is not an object")
	}
	delete(fields, "upstream_model")
	fields["id"] = mustJSON(res.ID)
	if kind == resources.KindAnthropicBatch {
		if raw, ok := fields["results_url"]; ok && string(raw) != "null" {
			fields["results_url"] = mustJSON(s.gatewayOrigin(r) + "/anthropic/v1/messages/batches/" + url.PathEscape(res.ID) + "/results")
		}
		return json.Marshal(fields)
	}
	if _, ok := fields["type"]; !ok {
		fields["type"] = json.RawMessage(`"file"`)
	}
	return json.Marshal(fields)
}

func decodeAnthropicObject(body []byte) (map[string]any, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid Anthropic request")
	}
	return value, nil
}

// Only protocol-defined content blocks own file sources. Tool inputs are
// application JSON and must never be traversed as Anthropic resource metadata.
func visitAnthropicFileSources(value map[string]any, visit func(map[string]any) error) error {
	var content func(any) error
	content = func(raw any) error {
		blocks, _ := raw.([]any)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			switch block["type"] {
			case "container_upload":
				return fmt.Errorf("container-upload file references are unsupported in batch requests")
			case "image", "document":
				source, _ := block["source"].(map[string]any)
				if source["type"] == "file" {
					id, ok := source["file_id"].(string)
					if !ok || id == "" || len(source) != 2 {
						return fmt.Errorf("invalid Anthropic file source")
					}
					if err := visit(source); err != nil {
						return err
					}
				}
			case "tool_result":
				if err := content(block["content"]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := content(value["system"]); err != nil {
		return err
	}
	messages, _ := value["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if err := content(message["content"]); err != nil {
			return err
		}
	}
	return nil
}

func anthropicFileReferences(body []byte) ([]string, error) {
	value, err := decodeAnthropicObject(body)
	if err != nil {
		return nil, err
	}
	refs := []string{}
	err = visitAnthropicFileSources(value, func(source map[string]any) error {
		refs = append(refs, source["file_id"].(string))
		return nil
	})
	return refs, err
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
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
		if file.ProviderID != first.ProviderID || file.ProviderRevisionID != first.ProviderRevisionID || file.TargetID != first.TargetID || file.SlotID != first.SlotID ||
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
	value, err := decodeAnthropicObject(body)
	if err != nil {
		return nil, err
	}
	replace := func(source map[string]any) error {
		id := source["file_id"].(string)
		bound, ok := upstream[id]
		if !ok {
			return fmt.Errorf("unknown file")
		}
		source["file_id"] = bound
		return nil
	}
	if requests, ok := value["requests"].([]any); ok {
		for _, raw := range requests {
			request, _ := raw.(map[string]any)
			params, _ := request["params"].(map[string]any)
			if err := visitAnthropicFileSources(params, replace); err != nil {
				return nil, err
			}
		}
	} else if err := visitAnthropicFileSources(value, replace); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
