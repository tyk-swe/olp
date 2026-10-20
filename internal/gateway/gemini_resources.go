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
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) registerGeminiFilesState(mux *http.ServeMux) {
	mux.HandleFunc("POST /gemini/upload/v1beta/files", s.geminiUploadStart)
	mux.HandleFunc("POST /gemini/upload/{session}", s.geminiUploadCommand)
	mux.HandleFunc("GET /gemini/v1beta/files", s.geminiListFiles)
	mux.HandleFunc("GET /gemini/v1beta/files/{name}", s.geminiGetFile)
	mux.HandleFunc("DELETE /gemini/v1beta/files/{name}", s.geminiDeleteFile)
	mux.HandleFunc("GET /gemini/download/v1beta/files/{action}", s.geminiDownloadFile)
	mux.HandleFunc("GET /gemini/v1beta/batches", s.geminiListBatches)
	mux.HandleFunc("GET /gemini/v1beta/batches/{name}", s.geminiGetBatch)
	mux.HandleFunc("POST /gemini/v1beta/batches/{action}", s.geminiBatchCancel)
	mux.HandleFunc("DELETE /gemini/v1beta/batches/{name}", s.geminiDeleteBatch)
}

func (s *Server) geminiBegin(w http.ResponseWriter, r *http.Request, family wire.Format) (*call, access.Authority, bool) {
	return s.stateBegin(w, r, family)
}

func (s *Server) geminiRoute(x *call, authority access.Authority, r *http.Request, requested string) (*routing.Route, *Error) {
	slug := strings.TrimSpace(r.Header.Get(stateRouteHeader))
	if slug == "" {
		slug = requested
	}
	if slug == "" {
		return nil, invalidRequest("missing_required_parameter", "X-OLP-Route is required.", nil)
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
		return nil, policyUnavailable("target_capability", "Strict routes do not serve Gemini files.")
	}
	if err := x.guardrailSurfaceGate(&route); err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Server) geminiPin(ctx context.Context, x *call, route *routing.Route) (*pin, *Error) {
	return s.selectPinSurface(ctx, x, route, "batch", "gemini", "unary", func(p *routing.Provider, model string) bool {
		return p.Connector().Supports("batch", "gemini", "unary") && p.Supports(model, "batch", "gemini", "unary")
	})
}

func (s *Server) geminiResourceGates(ctx context.Context, x *call, authority access.Authority, p *pin, route *routing.Route, newWork bool) *Error {
	if newWork {
		if err := s.unboundedPinLimits(x.admissionAuthority(authority), p, route); err != nil {
			return err
		}
		if err := x.guardrailSurfaceGate(route); err != nil {
			return err
		}
	}
	if err := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
		return err
	}
	if p.hold == nil {
		gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
		if gate.verdict != gateAdmitted {
			return gateError(gate)
		}
		p.hold = gate.hold
	}
	return nil
}

func validateGeminiStartHeaders(protocol, command, rawLength string, maximum int64) (int64, *Error) {
	if protocol != "resumable" || command != "start" {
		return 0, invalidRequest("invalid_request", "Gemini files require a resumable start upload.", nil)
	}
	length, err := strconv.ParseInt(rawLength, 10, 64)
	if err != nil || length <= 0 || length > maximum {
		param := "X-Goog-Upload-Header-Content-Length"
		return 0, invalidRequest("invalid_value", "The upload length must be between 1 byte and the configured media limit.", &param)
	}
	return length, nil
}

func parseGeminiUploadStartBody(body []byte) (string, bool, *Error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", false, nil
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil || doc.Root().Kind() != oif.Object || len(doc.Root().Members()) != 1 || doc.Root().Members()[0].Name != "file" {
		return "", false, invalidRequest("invalid_request", "The start body must be empty or contain only file.displayName.", nil)
	}
	file, present := doc.Root().Lookup("file")
	if !present || file.Kind() != oif.Object || len(file.Members()) != 1 {
		return "", false, invalidRequest("invalid_request", "The start body must contain one file display name.", nil)
	}
	member := file.Members()[0]
	if member.Name != "displayName" && member.Name != "display_name" {
		return "", false, invalidRequest("invalid_request", "The start body must contain only file.displayName.", nil)
	}
	name, ok := member.Value.Text()
	if !ok || len(name) > 512 {
		return "", false, invalidRequest("invalid_value", "file.displayName must be a string of at most 512 bytes.", nil)
	}
	return name, true, nil
}

func geminiUploadOrigin(cfg connectors.Config, model string, egressPolicy interface {
	ValidateEndpoint(string) (*url.URL, error)
}) (string, error) {
	generate, err := cfg.URL(wire.Gemini, model, false)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(generate)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("Gemini GenerateContent endpoint is malformed")
	}
	suffix := "/models/" + url.PathEscape(cfg.Model(model)) + ":generateContent"
	if !strings.HasSuffix(parsed.EscapedPath(), suffix) {
		return "", errors.New("Gemini GenerateContent path is not recognized")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	endpoint := origin + "/upload/v1beta/files"
	if _, err := egressPolicy.ValidateEndpoint(endpoint); err != nil {
		return "", err
	}
	return endpoint, nil
}

func validateGeminiUploadURL(uploadURL, providerEndpoint string, egressPolicy interface {
	ValidateEndpoint(string) (*url.URL, error)
}) error {
	upload, err := url.Parse(uploadURL)
	if err != nil || upload.Scheme != "https" || upload.Host == "" || upload.User != nil || upload.Fragment != "" {
		return errors.New("Gemini resumable upload URL must be an absolute HTTPS URL")
	}
	provider, err := url.Parse(providerEndpoint)
	if err != nil || !strings.EqualFold(upload.Host, provider.Host) {
		return errors.New("Gemini resumable upload URL host differs from the provider")
	}
	if _, err := egressPolicy.ValidateEndpoint(uploadURL); err != nil {
		return errors.New("Gemini resumable upload URL is outside egress policy")
	}
	return nil
}

func (s *Server) geminiUploadStart(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.geminiBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.release(r.Context())
	route, err := s.geminiRoute(x, authority, r, "")
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	expected, err := validateGeminiStartHeaders(r.Header.Get("X-Goog-Upload-Protocol"), r.Header.Get("X-Goog-Upload-Command"),
		r.Header.Get("X-Goog-Upload-Header-Content-Length"), s.cfg.MaxMediaBodyBytes)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	body, readErr := s.readBody(r)
	if readErr != nil {
		s.stateFail(x, w, readErr, x.family)
		return
	}
	displayName, hasDisplayName, err := parseGeminiUploadStartBody(body)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	if !s.Resources.Encrypted() {
		s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Gemini upload sessions require encrypted resource storage."), x.family)
		return
	}
	p, err := s.geminiPin(r.Context(), x, route)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	endpoint, originErr := geminiUploadOrigin(p.provider.Connector(), p.model, s.egress)
	if originErr != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The Gemini upload address could not be resolved."), x.family)
		return
	}
	payload := []byte{}
	if hasDisplayName {
		payload, _ = json.Marshal(map[string]any{"file": map[string]string{"display_name": displayName}})
	}
	headers := http.Header{
		"X-Goog-Upload-Protocol":              {"resumable"},
		"X-Goog-Upload-Command":               {"start"},
		"X-Goog-Upload-Header-Content-Length": {strconv.FormatInt(expected, 10)},
	}
	if contentType := r.Header.Get("X-Goog-Upload-Header-Content-Type"); contentType != "" {
		headers.Set("X-Goog-Upload-Header-Content-Type", contentType)
	}
	resp, failure := s.geminiPinnedRequest(ctx, x, p, http.MethodPost, endpoint, bytes.NewReader(payload), int64(len(payload)), "application/json", "application/json", headers)
	if failure != nil {
		s.stateFail(x, w, upstreamError(failure), x.family)
		return
	}
	defer resp.Body.Close()
	uploadURLs := resp.Header.Values("X-Goog-Upload-URL")
	if len(uploadURLs) != 1 {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider did not return one resumable upload URL."), x.family)
		return
	}
	sessionURL := uploadURLs[0]
	generationURL, _ := p.provider.Connector().URL(wire.Gemini, p.model, false)
	if err := validateGeminiUploadURL(sessionURL, generationURL, s.egress); err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned an invalid resumable upload URL."), x.family)
		return
	}
	now := s.now()
	expires := now.Add(time.Hour)
	durableBound := now.Add(resources.DurableLifetime)
	if durableBound.Before(expires) {
		expires = durableBound
	}
	id := uuid.Must(uuid.NewV7())
	metadata := map[string]any{"upstream_model": p.model, "expected_length": expected, "offset": int64(0)}
	if hasDisplayName {
		metadata["display_name"] = displayName
	}
	if contentType := r.Header.Get("X-Goog-Upload-Header-Content-Type"); contentType != "" {
		metadata["content_type"] = contentType
	}
	commitCtx, stopCommit := resourceCommitContext(ctx)
	resource, storeErr := s.Resources.PutGeminiUploadSession(commitCtx, &resources.Resource{
		UUID: id, Kind: resources.KindGeminiUpload, APIKeyID: authority.ID, RouteSlug: route.Slug,
		ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: route.RevisionID,
		TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID,
		UpstreamID: id.String(), State: "active", Metadata: mustJSON(metadata), ExpiresAt: &expires,
	}, sessionURL)
	stopCommit()
	if storeErr != nil {
		s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The upload session could not be stored."), x.family)
		return
	}
	x.dispatched = true
	x.completePinned()
	x.responseHeaders(w.Header(), false)
	w.Header().Set("X-Goog-Upload-Status", "active")
	w.Header().Set("X-Goog-Upload-URL", s.gatewayOrigin(r)+"/gemini/upload/"+url.PathEscape(resource.ID))
	w.WriteHeader(http.StatusOK)
	s.finish(x, &outcome{committed: true}, http.StatusOK)
}

func (s *Server) uploadGeminiManagedFile(ctx context.Context, x *call, p *pin, part *media.Part) ([]byte, string, *attemptFailure) {
	endpoint, err := geminiUploadOrigin(p.provider.Connector(), p.model, s.egress)
	if err != nil {
		return nil, "", geminiManagedUploadFailure(x, classConnect, false)
	}
	startHeaders := http.Header{
		"X-Goog-Upload-Protocol":              {"resumable"},
		"X-Goog-Upload-Command":               {"start"},
		"X-Goog-Upload-Header-Content-Length": {strconv.FormatInt(part.Size, 10)},
		"X-Goog-Upload-Header-Content-Type":   {"application/jsonl"},
	}
	displayName, _ := json.Marshal(map[string]any{"file": map[string]string{"display_name": part.Filename}})
	start, failure := s.geminiPinnedRequest(ctx, x, p, http.MethodPost, endpoint, bytes.NewReader(displayName), int64(len(displayName)), "application/json", "application/json", startHeaders)
	if failure != nil {
		return nil, "", failure
	}
	uploadURL := start.Header.Get("X-Goog-Upload-URL")
	generationURL, _ := p.provider.Connector().URL(wire.Gemini, p.model, false)
	if len(start.Header.Values("X-Goog-Upload-URL")) != 1 ||
		validateGeminiUploadURL(uploadURL, generationURL, s.egress) != nil {
		_ = start.Body.Close()
		return nil, "", geminiManagedUploadFailure(x, classProtocol, true)
	}
	_ = start.Body.Close()
	opened, err := s.transport().Spool.Open(part.Handle)
	if err != nil {
		return nil, "", geminiManagedUploadFailure(x, classProtocol, true)
	}
	defer opened.File.Close()
	chunkHeaders := http.Header{
		"X-Goog-Upload-Command": {"upload, finalize"},
		"X-Goog-Upload-Offset":  {"0"},
	}
	final, failure := s.geminiPinnedRequest(ctx, x, p, http.MethodPost, uploadURL, opened.File, part.Size,
		"application/jsonl", "application/json", chunkHeaders)
	if failure != nil {
		return nil, "", failure
	}
	defer final.Body.Close()
	if final.Header.Get("X-Goog-Upload-Status") != "final" {
		return nil, "", geminiManagedUploadFailure(x, classProtocol, true)
	}
	body, err := readBounded(final.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		return nil, "", geminiManagedUploadFailure(x, classProtocol, true)
	}
	var result struct {
		File json.RawMessage `json:"file"`
	}
	var fileObject map[string]json.RawMessage
	if json.Unmarshal(body, &result) != nil || len(result.File) == 0 ||
		json.Unmarshal(result.File, &fileObject) != nil {
		return nil, "", geminiManagedUploadFailure(x, classProtocol, true)
	}
	var name string
	if json.Unmarshal(fileObject["name"], &name) != nil {
		return nil, "", geminiManagedUploadFailure(x, classProtocol, true)
	}
	if _, err := geminiFileID(name); err != nil {
		return nil, "", geminiManagedUploadFailure(x, classProtocol, true)
	}
	return body, name, nil
}

func geminiManagedUploadFailure(x *call, class string, dispatched bool) *attemptFailure {
	if x != nil {
		x.dispatched = x.dispatched || dispatched
	}
	return &attemptFailure{class: class, status: http.StatusBadGateway, dispatched: dispatched}
}

func (s *Server) geminiPinnedRequest(ctx context.Context, x *call, p *pin, method, endpoint string, body io.Reader, length int64, contentType, accept string, headers http.Header) (*http.Response, *attemptFailure) {
	exchange := s.beginPinned(ctx, x, p, method)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil {
		return nil, exchange.fail(s, x, classConnect, nil)
	}
	if _, err := s.egress.ValidateEndpoint(parsed.String()); err != nil {
		return nil, exchange.fail(s, x, classConnect, nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return nil, exchange.fail(s, x, classConnect, nil)
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if body != nil {
		req.ContentLength = length
	}
	if err := s.applySlotCredential(ctx, x, req, p.provider.Connector(), p.slot, nil, &p.provider, p.model); err != nil {
		if ctx.Err() != nil {
			return nil, exchange.fail(s, x, classCancelled, nil)
		}
		return nil, exchange.fail(s, x, classCredential, nil)
	}
	for name, values := range headers {
		req.Header.Del(name)
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	return s.doPinnedRequest(ctx, x, p, exchange, req, openai.ParseErrorBody)
}

type geminiCommand struct {
	query    bool
	finalize bool
}

func parseGeminiUploadCommand(value string) (geminiCommand, *Error) {
	switch strings.TrimSpace(value) {
	case "query":
		return geminiCommand{query: true}, nil
	case "upload":
		return geminiCommand{}, nil
	case "upload, finalize":
		return geminiCommand{finalize: true}, nil
	default:
		param := "X-Goog-Upload-Command"
		return geminiCommand{}, invalidRequest("invalid_value", "The upload command must be upload, upload, finalize, or query.", &param)
	}
}

func parseGeminiUploadOffset(value string) (int64, *Error) {
	offset, err := strconv.ParseInt(value, 10, 64)
	if err != nil || offset < 0 {
		param := "X-Goog-Upload-Offset"
		return 0, invalidRequest("invalid_value", "The upload offset must be a non-negative integer.", &param)
	}
	return offset, nil
}

func validateGeminiChunk(expected, stored, offset, length int64, finalize bool) *Error {
	if offset != stored {
		param := "X-Goog-Upload-Offset"
		return invalidRequest("invalid_value", "The upload offset does not match the stored session offset.", &param)
	}
	if expected <= 0 || stored < 0 || stored > expected || length < 0 || length > expected-stored {
		param := "Content-Length"
		return invalidRequest("invalid_value", "The upload chunk exceeds the remaining declared file length.", &param)
	}
	if finalize && stored+length != expected {
		param := "Content-Length"
		return invalidRequest("invalid_value", "The final upload chunk must complete the declared file length.", &param)
	}
	return nil
}

func geminiUploadState(resource *resources.Resource) (expected, offset int64, contentType string, err error) {
	var metadata struct {
		Expected int64  `json:"expected_length"`
		Offset   int64  `json:"offset"`
		Type     string `json:"content_type"`
	}
	if json.Unmarshal(resource.Metadata, &metadata) != nil || metadata.Expected <= 0 || metadata.Offset < 0 || metadata.Offset > metadata.Expected {
		return 0, 0, "", resources.ErrContract
	}
	return metadata.Expected, metadata.Offset, metadata.Type, nil
}

func geminiFileID(name string) (string, error) {
	prefix, id, ok := strings.Cut(name, "/")
	if !ok || prefix != "files" || id == "" || strings.Contains(id, "/") {
		return "", fmt.Errorf("invalid Gemini file name")
	}
	return id, nil
}

func geminiFileMetadata(body []byte, model string) (json.RawMessage, string, *time.Time, error) {
	var file map[string]json.RawMessage
	if json.Unmarshal(body, &file) != nil || file == nil {
		return nil, "", nil, errors.New("malformed Gemini file")
	}
	nameRaw := file["name"]
	var name string
	if json.Unmarshal(nameRaw, &name) != nil {
		return nil, "", nil, errors.New("Gemini file has no name")
	}
	if _, err := geminiFileID(name); err != nil {
		return nil, "", nil, err
	}
	metadata := map[string]json.RawMessage{"upstream_model": mustJSON(model)}
	for field, value := range file {
		metadata[field] = value
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

func projectGeminiFile(body []byte, localID, origin string) ([]byte, error) {
	var file map[string]json.RawMessage
	if json.Unmarshal(body, &file) != nil || file == nil {
		return nil, errors.New("malformed Gemini file projection")
	}
	delete(file, "upstream_model")
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

func writeGeminiUploadAck(w http.ResponseWriter, x *call, offset int64, state string) {
	x.completePinned()
	x.responseHeaders(w.Header(), false)
	w.Header().Set("X-Goog-Upload-Status", state)
	w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(offset, 10))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) markGeminiUploadFailed(ctx context.Context, resource *resources.Resource) {
	commitCtx, stop := resourceCommitContext(ctx)
	defer stop()
	if err := s.Resources.Update(commitCtx, resource.ID, "failed", nil, nil); err != nil {
		s.log.Warn("Gemini upload session failure could not be recorded", "resource_id", resource.ID, "error", err)
	}
}

func (s *Server) geminiUploadCommand(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.geminiBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.release(r.Context())
	session, err := s.Resources.Get(r.Context(), resources.KindGeminiUpload, authority.ID, r.PathValue("session"))
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No upload session with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The Gemini upload session could not be read."), x.family)
		return
	}
	if session.State == "failed" {
		s.stateFail(x, w, serverError(http.StatusConflict, "resource_transition", "The Gemini upload session has failed."), x.family)
		return
	}
	command, commandErr := parseGeminiUploadCommand(r.Header.Get("X-Goog-Upload-Command"))
	if commandErr != nil {
		s.stateFail(x, w, commandErr, x.family)
		return
	}
	p, route, pinErr := s.resolveResource(r.Context(), x, authority, session, "batch", retainedNewWork)
	if pinErr != nil {
		s.stateFail(x, w, pinErr, x.family)
		return
	}
	if x.strict() {
		s.stateFail(x, w, policyUnavailable("target_capability", "Strict routes do not serve Gemini files."), x.family)
		return
	}
	if !p.provider.Connector().Supports("batch", "gemini", "unary") || !p.provider.Supports(p.model, "batch", "gemini", "unary") {
		s.stateFail(x, w, notFoundError("not_found", "No upload session with this identifier exists for this key."), x.family)
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	expected, offset, contentType, err := geminiUploadState(session)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Gemini upload session metadata is invalid."), x.family)
		return
	}
	if command.query {
		if r.ContentLength != 0 {
			s.stateFail(x, w, invalidRequest("invalid_request", "A query upload command cannot contain a body.", nil), x.family)
			return
		}
		writeGeminiUploadAck(w, x, offset, "active")
		s.finish(x, &outcome{committed: true}, http.StatusOK)
		return
	}
	if r.ContentLength < 0 {
		param := "Content-Length"
		s.stateFail(x, w, invalidRequest("missing_required_parameter", "Content-Length is required for Gemini upload chunks.", &param), x.family)
		return
	}
	requestOffset, offsetErr := parseGeminiUploadOffset(r.Header.Get("X-Goog-Upload-Offset"))
	if offsetErr != nil {
		s.stateFail(x, w, offsetErr, x.family)
		return
	}
	if err := validateGeminiChunk(expected, offset, requestOffset, r.ContentLength, command.finalize); err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	remaining := expected - offset
	if r.ContentLength > s.cfg.MaxMediaBodyBytes {
		s.stateFail(x, w, serverError(http.StatusRequestEntityTooLarge, "request_too_large", "The Gemini upload chunk exceeds the configured media limit."), x.family)
		return
	}
	var chunk io.Reader = bytes.NewReader(nil)
	var chunkLength int64
	if r.ContentLength > 0 {
		artifact, err := s.transport().Spool.Put(ctx, media.Upload{
			Filename: "gemini-upload-chunk", ContentType: contentType, MaximumLength: min(remaining, s.cfg.MaxMediaBodyBytes), Body: r.Body,
		})
		if err != nil {
			s.stateFail(x, w, serverError(http.StatusBadRequest, "invalid_request", "The Gemini upload chunk could not be staged."), x.family)
			return
		}
		defer s.transport().Spool.Remove(artifact.Handle)
		opened, err := s.transport().Spool.Open(artifact.Handle)
		if err != nil {
			s.stateFail(x, w, serverError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini chunk could not be read."), x.family)
			return
		}
		defer opened.File.Close()
		chunk, chunkLength = opened.File, artifact.ContentLength
	}
	uploadURL, err := s.Resources.ReadGeminiUploadURL(ctx, session.UUID)
	generationURL, generationErr := p.provider.Connector().URL(wire.Gemini, p.model, false)
	if err != nil || generationErr != nil || validateGeminiUploadURL(uploadURL, generationURL, s.egress) != nil {
		s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini upload session URL is unavailable."), x.family)
		return
	}
	commandHeader := "upload"
	if command.finalize {
		commandHeader = "upload, finalize"
	}
	headers := http.Header{
		"X-Goog-Upload-Command": {commandHeader},
		"X-Goog-Upload-Offset":  {strconv.FormatInt(offset, 10)},
	}
	resp, failure := s.geminiPinnedRequest(ctx, x, p, http.MethodPost, uploadURL, chunk, chunkLength, contentType, "application/json", headers)
	if failure != nil {
		x.dispatched = failure.dispatched
		s.markGeminiUploadFailed(r.Context(), session)
		s.stateFail(x, w, upstreamError(failure), x.family)
		return
	}
	defer resp.Body.Close()
	x.dispatched = true
	newOffset := offset + chunkLength
	if received := resp.Header.Get("X-Goog-Upload-Size-Received"); received != "" {
		reported, parseErr := strconv.ParseInt(received, 10, 64)
		if parseErr != nil || reported != newOffset || reported > expected {
			s.markGeminiUploadFailed(r.Context(), session)
			s.stateFail(x, w, serverError(http.StatusBadGateway, "provider_protocol_error", "The Gemini upload offset response is invalid."), x.family)
			return
		}
		newOffset = reported
	}
	if command.finalize {
		body, readErr := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		if readErr != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.stateFail(x, w, serverError(http.StatusBadGateway, "provider_protocol_error", "The finalized Gemini file response could not be read."), x.family)
			return
		}
		var envelope struct {
			File json.RawMessage `json:"file"`
		}
		if json.Unmarshal(body, &envelope) != nil || len(envelope.File) == 0 {
			s.markGeminiUploadFailed(r.Context(), session)
			s.stateFail(x, w, serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file object."), x.family)
			return
		}
		fileMetadata, state, expires, metadataErr := geminiFileMetadata(envelope.File, p.model)
		var fileObject map[string]json.RawMessage
		if metadataErr != nil || json.Unmarshal(envelope.File, &fileObject) != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.stateFail(x, w, serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file object."), x.family)
			return
		}
		var upstreamName string
		if json.Unmarshal(fileObject["name"], &upstreamName) != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.stateFail(x, w, serverError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file name is invalid."), x.family)
			return
		}
		upstreamID, idErr := geminiFileID(upstreamName)
		if idErr != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.stateFail(x, w, serverError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file name is invalid."), x.family)
			return
		}
		file := &resources.Resource{
			Kind: resources.KindGeminiFile, APIKeyID: authority.ID, RouteSlug: route.Slug,
			ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: route.RevisionID,
			TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID,
			UpstreamID: upstreamID, State: state, Metadata: fileMetadata, ExpiresAt: expires,
		}
		commitCtx, stopCommit := resourceCommitContext(ctx)
		created, finalErr := s.Resources.FinalizeGeminiUpload(commitCtx, authority.ID, session.UUID, newOffset, file)
		stopCommit()
		if finalErr != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The finalized Gemini file could not be stored."), x.family)
			return
		}
		projected, projectionErr := projectGeminiFile(envelope.File, created.ID, s.gatewayOrigin(r))
		if projectionErr != nil {
			s.stateFail(x, w, serverError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file could not be projected."), x.family)
			return
		}
		x.completePinned()
		w.Header().Set("X-Goog-Upload-Status", "final")
		w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(newOffset, 10))
		s.writeStateJSON(w, x, mustJSON(map[string]json.RawMessage{"file": projected}))
		return
	}
	newMetadata, _ := json.Marshal(map[string]int64{"offset": newOffset})
	commitCtx, stopCommit := resourceCommitContext(ctx)
	updateErr := s.Resources.Update(commitCtx, session.ID, "active", newMetadata, nil)
	stopCommit()
	if updateErr != nil {
		s.markGeminiUploadFailed(r.Context(), session)
		s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini upload offset could not be stored."), x.family)
		return
	}
	writeGeminiUploadAck(w, x, newOffset, "active")
	s.finish(x, &outcome{committed: true}, http.StatusOK)
}

func (s *Server) geminiFileCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, access.Authority, *resources.Resource, *pin, *routing.Route) *Error) {
	x, authority, done := s.geminiBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.release(r.Context())
	localID := r.PathValue("name")
	if strings.HasPrefix(localID, "files/") {
		localID = strings.TrimPrefix(localID, "files/")
	}
	res, err := s.Resources.Get(r.Context(), resources.KindGeminiFile, authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No Gemini file with this name exists for this key."), "gemini")
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The Gemini file mapping could not be read."), "gemini")
		return
	}
	p, route, pinErr := s.resolveResource(r.Context(), x, authority, res, "batch", use)
	if pinErr != nil {
		s.stateFail(x, w, pinErr, "gemini")
		return
	}
	if !p.provider.Connector().Supports("batch", "gemini", "unary") || !p.provider.Supports(p.model, "batch", "gemini", "unary") {
		s.stateFail(x, w, notFoundError("not_found", "No Gemini file with this name exists for this key."), "gemini")
		return
	}
	if route.Fidelity.Strict() {
		s.stateFail(x, w, policyUnavailable("target_capability", "Strict routes do not serve Gemini files."), "gemini")
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, false); err != nil {
		s.stateFail(x, w, err, "gemini")
		return
	}
	if err := op(ctx, x, authority, res, p, route); err != nil {
		s.stateFail(x, w, err, "gemini")
	}
}

func (s *Server) geminiGetFile(w http.ResponseWriter, r *http.Request) {
	s.geminiFileCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, _ access.Authority, res *resources.Resource, p *pin, _ *routing.Route) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "files/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The Gemini file address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.Resources.Tombstone(ctx, res.ID)
				return notFoundError("not_found", "No Gemini file with this name exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The Gemini file response could not be read.")
		}
		metadata, state, expires, err := geminiFileMetadata(body, p.model)
		if err != nil {
			return serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file.")
		}
		var object map[string]json.RawMessage
		var upstreamName string
		if json.Unmarshal(body, &object) != nil || json.Unmarshal(object["name"], &upstreamName) != nil {
			return serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file name.")
		}
		upstreamID, idErr := geminiFileID(upstreamName)
		if idErr != nil || upstreamID != res.UpstreamID {
			return serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned a mismatched Gemini file name.")
		}
		if err := s.Resources.Update(ctx, res.ID, state, metadata, expires); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini file metadata could not be refreshed.")
		}
		projected, err := projectGeminiFile(body, res.ID, s.gatewayOrigin(r))
		if err != nil {
			return serverError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file could not be projected.")
		}
		x.completePinned()
		s.writeStateJSON(w, x, projected)
		return nil
	})
}

func (s *Server) geminiDeleteFile(w http.ResponseWriter, r *http.Request) {
	s.geminiFileCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, _ access.Authority, res *resources.Resource, p *pin, _ *routing.Route) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "files/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The Gemini file address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.Resources.Tombstone(ctx, res.ID)
				return notFoundError("not_found", "No Gemini file with this name exists for this key.")
			}
			return upstreamError(failure)
		}
		_ = resp.Body.Close()
		if err := s.Resources.Tombstone(ctx, res.ID); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini file could not be tombstoned.")
		}
		x.completePinned()
		s.writeStateJSON(w, x, []byte(`{}`))
		return nil
	})
}

func (s *Server) geminiListFiles(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.geminiBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.release(r.Context())
	pageSize, pageToken, cursorErr := geminiPageCursor(r.URL.Query())
	if cursorErr != nil {
		s.stateFail(x, w, cursorErr, "gemini")
		return
	}
	rows, err := s.Resources.ListKinds(r.Context(), []string{resources.KindGeminiFile}, authority.ID, pageSize+1, pageToken)
	if errors.Is(err, resources.ErrNotFound) {
		param := "pageToken"
		s.stateFail(x, w, invalidRequest("invalid_value", "pageToken must identify a Gemini file owned by this key.", &param), "gemini")
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The Gemini file list could not be read."), "gemini")
		return
	}
	hasMore := len(rows) > pageSize
	if hasMore {
		rows = rows[:pageSize]
	}
	files := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if authErr := s.authorizeListedResource(r.Context(), authority, row); authErr != nil {
			s.stateFail(x, w, authErr, "gemini")
			return
		}
		var metadata map[string]json.RawMessage
		if json.Unmarshal(row.Metadata, &metadata) != nil {
			s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "A Gemini file could not be projected."), "gemini")
			return
		}
		body, _ := json.Marshal(metadata)
		file, projectErr := projectGeminiFile(body, row.ID, s.gatewayOrigin(r))
		if projectErr != nil {
			s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "A Gemini file could not be projected."), "gemini")
			return
		}
		files = append(files, file)
	}
	response := map[string]any{"files": files}
	if hasMore && len(rows) > 0 {
		response["nextPageToken"] = rows[len(rows)-1].ID
	}
	s.writeStateJSON(w, x, mustJSON(response))
}

func (s *Server) geminiDownloadFile(w http.ResponseWriter, r *http.Request) {
	localID, action, ok := strings.Cut(r.PathValue("action"), ":")
	if !ok || action != "download" || localID == "" {
		x, _, done := s.geminiBegin(w, r, wire.GeminiFile)
		if !done {
			defer s.release(r.Context())
			s.stateFail(x, w, notFoundError("not_found", "Unknown Gemini file action."), "gemini")
		}
		return
	}
	r.SetPathValue("name", localID)
	s.geminiFileCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, _ access.Authority, res *resources.Resource, p *pin, _ *routing.Route) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "files/"+url.PathEscape(res.UpstreamID)+":download", url.Values{"alt": []string{"media"}})
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The Gemini download address could not be resolved.")
		}
		x.geminiFileBinary = true
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		artifact, err := s.transport().Spool.Put(ctx, media.Upload{
			Filename: "gemini-file", ContentType: contentType, MaximumLength: s.cfg.MaxResponseBytes, Body: resp.Body,
		})
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_response_too_large", "The Gemini download exceeded the configured response limit.")
		}
		defer s.transport().Spool.Remove(artifact.Handle)
		opened, err := s.transport().Spool.Open(artifact.Handle)
		if err != nil {
			return serverError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini download could not be read.")
		}
		defer opened.File.Close()
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.ContentLength, 10))
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
			return serverError(http.StatusInternalServerError, "internal_error", "The Gemini download response could not be bounded.")
		}
		x.responseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := io.Copy(w, opened.File)
		x.completePinned()
		if copyErr != nil || written != artifact.ContentLength {
			x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The Gemini download ended before all bytes were delivered.")
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

func geminiPageCursor(query url.Values) (int, string, *Error) {
	pageSize := 10
	if raw := query.Get("pageSize"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			param := "pageSize"
			return 0, "", invalidRequest("invalid_value", "pageSize must be an integer between 1 and 100.", &param)
		}
		pageSize = parsed
	}
	token := query.Get("pageToken")
	if token != "" && !validGeminiFileID(token) {
		param := "pageToken"
		return 0, "", invalidRequest("invalid_value", "pageToken must be a local Gemini file identifier.", &param)
	}
	return pageSize, token, nil
}

func validGeminiFileID(value string) bool {
	const prefix = resources.KindGeminiFile + "_"
	raw := strings.TrimPrefix(value, prefix)
	if raw == value || len(raw) != 32 {
		return false
	}
	id, err := uuid.Parse(raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:])
	return err == nil && resources.LocalID(resources.KindGeminiFile, id) == value
}
