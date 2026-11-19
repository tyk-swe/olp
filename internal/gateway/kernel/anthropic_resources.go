package kernel

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
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

const anthropicFilesBeta = "files-api-2025-04-14"

func anthropicFilesHeaders() http.Header {
	return http.Header{"Anthropic-Beta": {anthropicFilesBeta}}
}

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

func (s *Server) anthropicBegin(w http.ResponseWriter, r *http.Request, family wire.Format) (*Call, keyauthority.Authority, bool) {
	x, authority, done := s.StateBegin(w, r, family)
	if done {
		return x, authority, true
	}
	if err := validateAnthropicFilesBeta(r.Header); err != nil {
		s.Gate.Release(r.Context())
		s.Refuse(x, w, err)
		return x, authority, true
	}
	return x, authority, false
}

func (s *Server) anthropicUploadFile(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.anthropicBegin(w, r, wire.AnthropicFile)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	route, err := s.NativeStateRoute(x, authority, r, "")
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	form, err := s.ParseMediaForm(w, r, authority.ID, s.Config.MaxMediaBodyBytes, 1)
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	defer form.Cleanup()
	file := form.TakeSingleFile("file")
	if form.Err() != nil || file == nil {
		s.Refuse(x, w, fault.InvalidRequest("invalid_request", "Upload exactly one file part.", nil))
		return
	}
	if extra := form.TakeExtensions(); form.Err() != nil || len(extra) != 0 {
		s.Refuse(x, w, fault.InvalidRequest("invalid_request", "Anthropic file uploads accept only one file part.", nil))
		return
	}
	form.Disarm()
	defer s.Deps.Staging.Remove(file.Handle)
	p, err := s.NativeStatePin(r.Context(), x, route)
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if e := s.UnboundedPinLimits(x.AdmissionAuthority(authority), p, route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	if e := s.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	x.PinnedHeaders = anthropicFilesHeaders()
	endpoint, urlErr := ResourceURL(p.Provider.Connector(), p.Model, "files", anthropicFilesQuery())
	if urlErr != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."))
		return
	}
	resp, failure := s.UploadMultipart(ctx, x, p, endpoint, "", nil, file, false)
	if failure != nil {
		s.Refuse(x, w, UpstreamError(failure))
		return
	}
	defer resp.Body.Close()

	body, readErr := resp.ReadBody(s.Config.MaxResponseBytes)
	if readErr != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."))
		return
	}
	upstreamID, valid := NativeResourceID(body, s.Config.MaxResponseBytes)
	if !valid {
		s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."))
		return
	}
	metadata, state, expires := anthropicFileMetadata(body, p.Model)
	resp.Exchange.Complete(ClassSuccess)
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	res, putErr := s.Deps.Resources.Put(commitCtx, &resources.Resource{
		Kind: resources.KindAnthropicFile, APIKeyID: authority.ID, RouteSlug: route.Slug,
		ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID,
		TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID,
		UpstreamID: upstreamID, State: state, Metadata: metadata, ExpiresAt: expires,
	})
	if putErr != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be stored."))
		return
	}
	out, _ := RewriteID(body, "id", res.ID)
	s.WriteStateJSON(w, x, out, resp.Exchange)
}

func (s *Server) anthropicFileCall(w http.ResponseWriter, r *http.Request, use RetainedUse, op func(context.Context, *Call, *resources.Resource, *Pin) *fault.Error) {
	x, authority, done := s.anthropicBegin(w, r, wire.AnthropicFile)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	res, err := s.Deps.Resources.Get(r.Context(), resources.KindAnthropicFile, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No file with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."))
		return
	}
	p, route, e := s.ResolveResource(r.Context(), x, authority, res, "batch", use)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	if !p.Provider.Connector().Supports("batch", "anthropic", "unary") || !p.Provider.Supports(p.Model, "batch", "anthropic", "unary") {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No file with this identifier exists for this key."))
		return
	}
	x.Route = route
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if e = s.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	if e = op(ctx, x, res, p); e != nil {
		s.Refuse(x, w, e)
	}
}

func (s *Server) anthropicGetFile(w http.ResponseWriter, r *http.Request) {
	s.anthropicFileCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, res *resources.Resource, p *Pin) *fault.Error {
		x.PinnedHeaders = anthropicFilesHeaders()
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID), anthropicFilesQuery())
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.Deps.Resources.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := resp.ReadBody(s.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		upstreamID, valid := NativeResourceID(body, s.Config.MaxResponseBytes)
		if !valid || upstreamID != res.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a mismatched file identity.")
		}
		metadata, state, expires := anthropicFileMetadata(body, p.Model)
		resp.Exchange.Complete(ClassSuccess)
		if err := s.Deps.Resources.Update(ctx, res.ID, state, metadata, expires); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The file refresh could not be committed.")
		}
		res.Metadata = metadata

		out, err := RewriteID(body, "id", res.ID)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
		}
		s.WriteStateJSON(w, x, out, resp.Exchange)
		return nil
	})
}

func (s *Server) anthropicDeleteFile(w http.ResponseWriter, r *http.Request) {
	s.anthropicFileCall(w, r, RetainedHousekeeping, func(ctx context.Context, x *Call, res *resources.Resource, p *Pin) *fault.Error {
		x.PinnedHeaders = anthropicFilesHeaders()
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID), anthropicFilesQuery())
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.Deps.Resources.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		_ = resp.Body.Close()
		resp.Exchange.Complete(ClassSuccess)
		if err := s.Deps.Resources.Tombstone(ctx, res.ID); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The file mapping could not be deleted.")
		}

		s.WriteStateJSON(w, x, MustJSON(map[string]any{"id": res.ID, "type": "file_deleted"}), resp.Exchange)
		return nil
	})
}

func (s *Server) anthropicFileContent(w http.ResponseWriter, r *http.Request) {
	s.anthropicFileCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, res *resources.Resource, p *Pin) *fault.Error {
		x.PinnedHeaders = anthropicFilesHeaders()
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID)+"/content", anthropicFilesQuery())
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		x.PinnedBinary = true
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			return UpstreamError(failure)
		}
		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		artifact, err := resp.StageBody(ctx, s.Deps.Staging, "file-content", contentType, s.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}
		resp.Exchange.Complete(ClassSuccess)
		defer s.Deps.Staging.Remove(artifact.Handle)
		opened, err := s.Deps.Staging.Open(artifact.Handle)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged provider file could not be read.")
		}
		defer opened.File.Close()
		SetFileDownloadHeaders(w.Header(), contentType, artifact.ContentLength)
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The results response could not be bounded.")
		}
		x.ResponseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := CopyResourceContent(w, opened.File, artifact.ContentLength)

		if copyErr != nil || written != artifact.ContentLength {
			resp.Exchange.Delivery(DeliveryState(written))
			x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The results response ended before all bytes were delivered.")
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
	defer s.Gate.Release(r.Context())
	limit, before, cursor, err := anthropicListCursor(r.URL.Query(), kind)
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	ctx := r.Context()
	if kind == resources.KindAnthropicBatch {
		scanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ctx = scanCtx
	}
	backwards := before != ""
	param := "after_id"
	if backwards {
		param = "before_id"
	}
	fetch := func(anchor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
		batch, err := s.Deps.Resources.ListKindsPage(ctx, []string{kind}, authority.ID, count, anchor, backwards)
		if errors.Is(err, resources.ErrNotFound) {
			return nil, fault.InvalidRequest("invalid_value", "The cursor must identify a resource owned by this key.", &param)
		}
		if err != nil {
			return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The resource list could not be read.")
		}
		return batch, nil
	}
	authorize := func(row *resources.Resource) *fault.Error {
		return s.AuthorizeListedResource(ctx, authority, row)
	}
	var rows []*resources.Resource
	var continuation string
	var listErr *fault.Error
	for token := cursor; ; token = continuation {
		rows, continuation, listErr = s.ListedResourcePage(authority.ID, param, limit, token, fetch, authorize)
		if kind != resources.KindAnthropicBatch || len(rows) != 0 || continuation == "" || listErr != nil || ctx.Err() != nil {
			break
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.Refuse(x, w, fault.ServerError(http.StatusGatewayTimeout, "gateway_timeout", "The resource list could not be read within the deadline."))
		return
	}
	if listErr != nil {
		s.Refuse(x, w, listErr)
		return
	}
	if backwards {
		slices.Reverse(rows)
	}
	items, ids := make([]json.RawMessage, 0, len(rows)), make([]string, 0, len(rows))
	for _, row := range rows {
		object, err := s.anthropicResourceObject(row, kind, r)
		if err != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A resource could not be projected."))
			return
		}
		items, ids = append(items, object), append(ids, row.ID)
	}
	s.WriteStateJSON(w, x, anthropicListBody(items, ids, continuation, backwards))
}

func anthropicListBody(items []json.RawMessage, ids []string, continuation string, backwards bool) []byte {
	body := map[string]any{"data": items, "has_more": continuation != ""}
	if len(ids) > 0 {
		body["first_id"], body["last_id"] = ids[0], ids[len(ids)-1]
	}
	if continuation != "" {
		if backwards {
			body["first_id"] = continuation
		} else {
			body["last_id"] = continuation
		}
	}
	return MustJSON(body)
}

func (s *Server) anthropicResourceObject(res *resources.Resource, kind string, r *http.Request) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(res.Metadata, &fields); err != nil || fields == nil {
		return nil, errors.New("stored Anthropic resource metadata is not an object")
	}
	delete(fields, "upstream_model")
	fields["id"] = MustJSON(res.ID)
	if kind == resources.KindAnthropicBatch {
		if raw, ok := fields["results_url"]; ok && string(raw) != "null" {
			fields["results_url"] = MustJSON(s.GatewayOrigin(r) + "/anthropic/v1/messages/batches/" + url.PathEscape(res.ID) + "/results")
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

func validateAnthropicFilesBeta(header http.Header) *fault.Error {
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

func anthropicListCursor(query url.Values, kind string) (int, string, string, *fault.Error) {
	before, after := query.Get("before_id"), query.Get("after_id")
	if before != "" && after != "" {
		return 0, "", "", fault.InvalidRequest("invalid_request", "before_id and after_id cannot be used together.", nil)
	}
	limit := 20
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			param := "limit"
			return 0, "", "", fault.InvalidRequest("invalid_value", "limit must be an integer between 1 and 1000.", &param)
		}
		limit = parsed
	}
	cursor := before
	if cursor == "" {
		cursor = after
	}
	if cursor != "" && !strings.HasPrefix(cursor, RetainedListCursorPrefix) && !validAnthropicCursor(kind, cursor) {
		param := "after_id"
		if before != "" {
			param = "before_id"
		}
		return 0, "", "", fault.InvalidRequest("invalid_value", param+" must be a local resource identifier.", &param)
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
