package geministate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) gates(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, p *kernel.Pin, route *routing.Route, newWork bool) *fault.Error {
	if newWork {
		if err := s.kernel.UnboundedPinLimits(x.AdmissionAuthority(authority), p, route); err != nil {
			return err
		}
		if err := x.GuardrailSurfaceGate(route); err != nil {
			return err
		}
	}
	return s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route)
}

func fileMetadata(body []byte, model string) (json.RawMessage, string, *time.Time, error) {
	var file map[string]json.RawMessage
	if json.Unmarshal(body, &file) != nil || file == nil {
		return nil, "", nil, errors.New("malformed Gemini file")
	}
	nameRaw := file["name"]
	var name string
	if json.Unmarshal(nameRaw, &name) != nil {
		return nil, "", nil, errors.New("Gemini file has no name")
	}
	if _, err := kernel.GeminiFileID(name); err != nil {
		return nil, "", nil, err
	}
	metadata := map[string]json.RawMessage{"upstream_model": kernel.MustJSON(model)}
	for field, value := range file {
		if field != "upstream_model" && field != "batch_manifest" {
			metadata[field] = value
		}
	}
	state := "ACTIVE"
	if raw := file["state"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &state)
	}
	var expires *time.Time
	if raw := file["expirationTime"]; len(raw) > 0 && string(raw) != "null" {
		var timestamp string
		if json.Unmarshal(raw, &timestamp) == nil {
			parsed, err := time.Parse(time.RFC3339Nano, timestamp)
			if err != nil {
				return nil, "", nil, errors.New("Gemini file expiration is malformed")
			}
			expires = &parsed
		}
	}
	encoded, err := json.Marshal(metadata)
	return encoded, state, expires, err
}

func projectFile(body []byte, localID, origin string) ([]byte, error) {
	var file map[string]json.RawMessage
	if json.Unmarshal(body, &file) != nil || file == nil {
		return nil, errors.New("malformed Gemini file projection")
	}
	delete(file, "upstream_model")
	delete(file, "batch_manifest")
	localName := "files/" + localID
	file["name"], _ = json.Marshal(localName)
	if raw, exists := file["uri"]; exists && string(raw) != "null" {
		file["uri"], _ = json.Marshal(origin + "/gemini/v1beta/files/" + url.PathEscape(localID))
	}
	if raw, exists := file["downloadUri"]; exists && string(raw) != "null" {
		file["downloadUri"], _ = json.Marshal(origin + "/gemini/download/v1beta/files/" + url.PathEscape(localID) + ":download?alt=media")
	}
	return json.Marshal(file)
}

func (s surface) fileCall(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, op func(context.Context, *kernel.Call, keyauthority.Authority, *resources.Resource, *kernel.Pin, *routing.Route) *fault.Error) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	localID := strings.TrimPrefix(r.PathValue("name"), "files/")
	res, err := s.kernel.Deps.Resources.Get(r.Context(), resources.KindGeminiFile, authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No Gemini file with this name exists for this key."))
		return
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini file mapping could not be read."))
		return
	}
	p, route, pinErr := s.kernel.ResolveResource(r.Context(), x, authority, res, "batch", use)
	if pinErr != nil {
		s.kernel.Refuse(x, w, pinErr)
		return
	}
	if !p.Provider.Connector().Supports("batch", "gemini", "unary") || !p.Provider.Supports(p.Model, "batch", "gemini", "unary") {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No Gemini file with this name exists for this key."))
		return
	}
	if route.Fidelity.Strict() {
		s.kernel.Refuse(x, w, kernel.PolicyUnavailable("target_capability", "Strict routes do not serve Gemini files."))
		return
	}
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.gates(ctx, x, authority, p, route, false); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	if err := op(ctx, x, authority, res, p, route); err != nil {
		s.kernel.Refuse(x, w, err)
	}
}

func (s surface) getFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, res *resources.Resource, p *kernel.Pin, _ *routing.Route) *fault.Error {
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini file address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.kernel.Deps.Resources.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No Gemini file with this name exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini file response could not be read.")
		}
		metadata, state, expires, err := fileMetadata(body, p.Model)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file.")
		}
		var object map[string]json.RawMessage
		var upstreamName string
		if json.Unmarshal(body, &object) != nil || json.Unmarshal(object["name"], &upstreamName) != nil {
			return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file name.")
		}
		upstreamID, idErr := kernel.GeminiFileID(upstreamName)
		if idErr != nil || upstreamID != res.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned a mismatched Gemini file name.")
		}
		resp.Exchange.Complete(kernel.ClassSuccess)
		var previous, fields map[string]json.RawMessage
		_ = json.Unmarshal(res.Metadata, &previous)
		_ = json.Unmarshal(metadata, &fields)
		if manifest, ok := previous["batch_manifest"]; ok {
			fields["batch_manifest"] = manifest
			metadata = kernel.MustJSON(fields)
		}
		if err := s.kernel.Deps.Resources.Update(ctx, res.ID, state, metadata, expires); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini file metadata could not be refreshed.")
		}
		projected, err := projectFile(body, res.ID, s.kernel.GatewayOrigin(r))
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file could not be projected.")
		}
		s.kernel.WriteStateJSON(w, x, projected, resp.Exchange)
		return nil
	})
}

func (s surface) deleteFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, res *resources.Resource, p *kernel.Pin, _ *routing.Route) *fault.Error {
		if res.ParentID != nil {
			if err := s.kernel.RequireBatchEvidenceSettled(ctx, *res.ParentID); err != nil {
				return err
			}
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini file address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.kernel.Deps.Resources.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No Gemini file with this name exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		resp.Exchange.Complete(kernel.ClassSuccess)
		_ = resp.Body.Close()
		if err := s.kernel.Deps.Resources.Tombstone(ctx, res.ID); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini file could not be tombstoned.")
		}
		s.kernel.WriteStateJSON(w, x, []byte(`{}`), resp.Exchange)
		return nil
	})
}

func (s surface) listFiles(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	pageSize, pageToken, cursorErr := filePageCursor(r.URL.Query())
	if cursorErr != nil {
		s.kernel.Refuse(x, w, cursorErr)
		return
	}
	rows, continuation, listErr := s.kernel.ListedResourcePage(authority.ID, "pageToken", pageSize, pageToken, func(cursor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
		batch, err := s.kernel.Deps.Resources.ListKindsPage(r.Context(), []string{resources.KindGeminiFile}, authority.ID, count, cursor, false)
		if errors.Is(err, resources.ErrNotFound) {
			param := "pageToken"
			return nil, fault.InvalidRequest("invalid_value", "pageToken must identify a Gemini file owned by this key.", &param)
		}
		if err != nil {
			return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini file list could not be read.")
		}
		return batch, nil
	}, func(row *resources.Resource) *fault.Error {
		return s.kernel.AuthorizeListedResource(r.Context(), authority, row)
	})
	if listErr != nil {
		s.kernel.Refuse(x, w, listErr)
		return
	}
	files := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		var metadata map[string]json.RawMessage
		if json.Unmarshal(row.Metadata, &metadata) != nil {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "A Gemini file could not be projected."))
			return
		}
		body, _ := json.Marshal(metadata)
		file, projectErr := projectFile(body, row.ID, s.kernel.GatewayOrigin(r))
		if projectErr != nil {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "A Gemini file could not be projected."))
			return
		}
		files = append(files, file)
	}
	response := map[string]any{"files": files}
	if continuation != "" {
		response["nextPageToken"] = continuation
	}
	s.kernel.WriteStateJSON(w, x, kernel.MustJSON(response))
}

func downloadURL(cfg connectors.Config, model, fileID string) (string, error) {
	generation, err := cfg.URL(wire.Gemini, model, false)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(generation)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("Gemini generation URL is malformed")
	}
	return parsed.Scheme + "://" + parsed.Host + "/download/v1beta/files/" + url.PathEscape(fileID) + ":download?alt=media", nil
}

func (s surface) downloadFile(w http.ResponseWriter, r *http.Request) {
	localID, action, ok := strings.Cut(r.PathValue("action"), ":")
	if !ok || action != "download" || localID == "" {
		x, _, done := s.kernel.StateBegin(w, r, wire.GeminiFile)
		if !done {
			defer s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "Unknown Gemini file action."))
		}
		return
	}
	r.SetPathValue("name", localID)
	s.fileCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, res *resources.Resource, p *kernel.Pin, _ *routing.Route) *fault.Error {
		endpoint, err := downloadURL(p.Provider.Connector(), p.Model, res.UpstreamID)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini download address could not be resolved.")
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
		artifact, err := resp.StageBody(ctx, s.kernel.Deps.Staging, "gemini-file", contentType, s.kernel.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Gemini download exceeded the configured response limit.")
		}
		resp.Exchange.Complete(kernel.ClassSuccess)
		defer s.kernel.Deps.Staging.Remove(artifact.Handle)
		opened, err := s.kernel.Deps.Staging.Open(artifact.Handle)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini download could not be read.")
		}
		defer opened.File.Close()
		kernel.SetFileDownloadHeaders(w.Header(), contentType, artifact.ContentLength)
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini download response could not be bounded.")
		}
		s.kernel.DeliverResourceContent(context.Background(), w, x, resp.Exchange, opened.File, artifact.ContentLength, "The Gemini download ended before all bytes were delivered.")
		return nil
	})
}

func filePageCursor(query url.Values) (int, string, *fault.Error) {
	pageSize := 10
	if raw := query.Get("pageSize"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			param := "pageSize"
			return 0, "", fault.InvalidRequest("invalid_value", "pageSize must be an integer between 1 and 100.", &param)
		}
		pageSize = parsed
	}
	token := query.Get("pageToken")
	if token != "" && !strings.HasPrefix(token, kernel.RetainedListCursorPrefix) && !kernel.ValidGeminiFileID(token) {
		param := "pageToken"
		return 0, "", fault.InvalidRequest("invalid_value", "pageToken must be a local Gemini file identifier.", &param)
	}
	return pageSize, token, nil
}
