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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) registerGeminiFilesState(mux *http.ServeMux) {
	mux.HandleFunc("OPTIONS /gemini/upload/", s.preflight)
	mux.HandleFunc("OPTIONS /gemini/download/", s.preflight)
	mux.HandleFunc("POST /gemini/upload/v1beta/files", s.geminiUploadStart)
	mux.HandleFunc("POST /gemini/upload/{session}", s.geminiUploadCommand)
	mux.HandleFunc("GET /gemini/v1beta/files", s.geminiListFiles)
	mux.HandleFunc("GET /gemini/v1beta/files/{name}", s.geminiGetFile)
	mux.HandleFunc("DELETE /gemini/v1beta/files/{name}", s.geminiDeleteFile)
	mux.HandleFunc("GET /gemini/download/v1beta/files/{action}", s.geminiDownloadFile)
	for _, version := range []string{"v1", "v1beta"} {
		prefix := "/gemini/" + version + "/batches"
		mux.HandleFunc("GET "+prefix, s.geminiListBatches)
		mux.HandleFunc("GET "+prefix+"/{name}", s.geminiGetBatch)
		mux.HandleFunc("POST "+prefix+"/{action}", s.geminiBatchCancel)
		mux.HandleFunc("DELETE "+prefix+"/{name}", s.geminiDeleteBatch)
	}
}

func (s *Server) geminiResourceGates(ctx context.Context, x *Call, authority keyauthority.Authority, p *Pin, route *routing.Route, newWork bool) *fault.Error {
	if newWork {
		if err := s.UnboundedPinLimits(x.AdmissionAuthority(authority), p, route); err != nil {
			return err
		}
		if err := x.GuardrailSurfaceGate(route); err != nil {
			return err
		}
	}
	return s.ReserveStateAndSlot(ctx, x, authority, p, route)
}

func validateGeminiStartHeaders(protocol, command, rawLength string, maximum int64) (int64, *fault.Error) {
	if protocol != "resumable" || command != "start" {
		return 0, fault.InvalidRequest("invalid_request", "Gemini files require a resumable start upload.", nil)
	}
	length, err := strconv.ParseInt(rawLength, 10, 64)
	if err != nil || length <= 0 || length > maximum {
		param := "X-Goog-Upload-Header-Content-Length"
		return 0, fault.InvalidRequest("invalid_value", "The upload length must be between 1 byte and the configured media limit.", &param)
	}
	return length, nil
}

func parseGeminiUploadStartBody(body []byte) (string, bool, *fault.Error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", false, nil
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil || doc.Root().Kind() != oif.Object {
		return "", false, fault.InvalidRequest("invalid_request", "The start body must be empty or contain only file.displayName.", nil)
	}
	if len(doc.Root().Members()) == 0 {
		return "", false, nil
	}
	if len(doc.Root().Members()) != 1 || doc.Root().Members()[0].Name != "file" {
		return "", false, fault.InvalidRequest("invalid_request", "The start body must be empty or contain only file.displayName.", nil)
	}
	file, present := doc.Root().Lookup("file")
	if !present || file.Kind() != oif.Object || len(file.Members()) > 1 {
		return "", false, fault.InvalidRequest("invalid_request", "The start body must contain one file display name.", nil)
	}
	if len(file.Members()) == 0 {
		return "", false, nil
	}
	member := file.Members()[0]
	if member.Name != "displayName" && member.Name != "display_name" {
		return "", false, fault.InvalidRequest("invalid_request", "The start body must contain only file.displayName.", nil)
	}
	name, ok := member.Value.Text()
	if !ok || len(name) > 512 {
		return "", false, fault.InvalidRequest("invalid_value", "file.displayName must be a string of at most 512 bytes.", nil)
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
	validationURL := *upload
	validationURL.RawQuery, validationURL.ForceQuery = "", false
	if _, err := egressPolicy.ValidateEndpoint(validationURL.String()); err != nil {
		return errors.New("Gemini resumable upload URL is outside egress policy")
	}
	return nil
}

func (s *Server) geminiUploadStart(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	route, err := s.NativeStateRoute(x, authority, r, "")
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	expected, err := validateGeminiStartHeaders(r.Header.Get("X-Goog-Upload-Protocol"), r.Header.Get("X-Goog-Upload-Command"),
		r.Header.Get("X-Goog-Upload-Header-Content-Length"), s.Config.MaxMediaBodyBytes)
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	body, readErr := s.ReadResourceBody(w, r)
	if readErr != nil {
		s.Refuse(x, w, readErr)
		return
	}
	displayName, hasDisplayName, err := parseGeminiUploadStartBody(body)
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	if !s.Deps.Resources.Encrypted() {
		s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Gemini upload sessions require encrypted resource storage."))
		return
	}
	p, err := s.NativeStatePin(r.Context(), x, route)
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.Refuse(x, w, err)
		return
	}
	endpoint, originErr := geminiUploadOrigin(p.Provider.Connector(), p.Model, s.egress)
	if originErr != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini upload address could not be resolved."))
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
		s.Refuse(x, w, UpstreamError(failure))
		return
	}
	defer resp.Body.Close()
	uploadURLs := resp.Header.Values("X-Goog-Upload-URL")
	if len(uploadURLs) != 1 {
		s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider did not return one resumable upload URL."))
		return
	}
	sessionURL := uploadURLs[0]
	generationURL, _ := p.Provider.Connector().URL(wire.Gemini, p.Model, false)
	if err := validateGeminiUploadURL(sessionURL, generationURL, s.egress); err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned an invalid resumable upload URL."))
		return
	}
	resp.Exchange.Complete(ClassSuccess)
	now := s.Now()
	expires := now.Add(time.Hour)
	durableBound := now.Add(resources.DurableLifetime)
	if durableBound.Before(expires) {
		expires = durableBound
	}
	id := uuid.Must(uuid.NewV7())
	metadata := map[string]any{"upstream_model": p.Model, "expected_length": expected, "offset": int64(0)}
	if hasDisplayName {
		metadata["display_name"] = displayName
	}
	if contentType := r.Header.Get("X-Goog-Upload-Header-Content-Type"); contentType != "" {
		metadata["content_type"] = contentType
	}
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	resource, storeErr := s.Deps.Resources.PutGeminiUploadSession(commitCtx, &resources.Resource{
		UUID: id, Kind: resources.KindGeminiUpload, APIKeyID: authority.ID, RouteSlug: route.Slug,
		ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID,
		TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID,
		UpstreamID: id.String(), State: "active", Metadata: MustJSON(metadata), ExpiresAt: &expires,
	}, sessionURL)
	stopCommit()
	if storeErr != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The upload session could not be stored."))
		return
	}

	x.ResponseHeaders(w.Header(), false)
	w.Header().Set("X-Goog-Upload-Status", "active")
	w.Header().Set("X-Goog-Upload-URL", s.GatewayOrigin(r)+"/gemini/upload/"+url.PathEscape(resource.ID))
	w.WriteHeader(http.StatusOK)
	s.finishGeminiUploadAck(w, x, resp.Exchange)
}

func (s *Server) uploadGeminiManagedFile(ctx context.Context, x *Call, p *Pin, part *staging.Part) ([]byte, string, *pinnedExchange, *attemptFailure) {
	endpoint, err := geminiUploadOrigin(p.Provider.Connector(), p.Model, s.egress)
	if err != nil {
		return nil, "", nil, geminiManagedUploadFailure(classConnect, false)
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
		return nil, "", nil, failure
	}
	uploadURL := start.Header.Get("X-Goog-Upload-URL")
	generationURL, _ := p.Provider.Connector().URL(wire.Gemini, p.Model, false)
	if len(start.Header.Values("X-Goog-Upload-URL")) != 1 ||
		validateGeminiUploadURL(uploadURL, generationURL, s.egress) != nil {
		_ = start.Body.Close()
		return nil, "", nil, geminiManagedUploadFailure(classProtocol, true)
	}
	_ = start.Body.Close()
	start.Exchange.Complete(ClassSuccess)
	opened, err := s.Deps.Staging.Open(part.Handle)
	if err != nil {
		return nil, "", nil, geminiManagedUploadFailure(classProtocol, true)
	}
	defer opened.File.Close()
	chunkHeaders := http.Header{
		"X-Goog-Upload-Command": {"upload, finalize"},
		"X-Goog-Upload-Offset":  {"0"},
	}
	final, failure := s.geminiPinnedRequest(ctx, x, p, http.MethodPost, uploadURL, pinnedUploadSource{opened.File}, part.Size,
		"application/jsonl", "application/json", chunkHeaders)
	if failure != nil {
		return nil, "", nil, failure
	}
	defer final.Body.Close()
	if final.Header.Get("X-Goog-Upload-Status") != "final" {
		return nil, "", nil, geminiManagedUploadFailure(classProtocol, true)
	}
	body, err := final.ReadBody(s.Config.MaxResponseBytes)
	if err != nil {
		return nil, "", nil, geminiManagedUploadFailure(classProtocol, true)
	}
	var result struct {
		File json.RawMessage `json:"file"`
	}
	var fileObject map[string]json.RawMessage
	if json.Unmarshal(body, &result) != nil || len(result.File) == 0 ||
		json.Unmarshal(result.File, &fileObject) != nil {
		return nil, "", nil, geminiManagedUploadFailure(classProtocol, true)
	}
	var name string
	if json.Unmarshal(fileObject["name"], &name) != nil {
		return nil, "", nil, geminiManagedUploadFailure(classProtocol, true)
	}
	if _, err := geminiFileID(name); err != nil {
		return nil, "", nil, geminiManagedUploadFailure(classProtocol, true)
	}
	return body, name, final.Exchange, nil
}

func geminiManagedUploadFailure(class string, dispatched bool) *attemptFailure {
	return &attemptFailure{class: class, Status: http.StatusBadGateway, dispatched: dispatched}
}

func (s *Server) geminiPinnedRequest(ctx context.Context, x *Call, p *Pin, method, endpoint string, body io.Reader, length int64, contentType, accept string, headers http.Header) (*pinnedResponse, *attemptFailure) {
	exchange := s.beginPinned(ctx, x, p, method)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil {
		return nil, exchange.fail(classConnect, nil)
	}
	validationURL := *parsed
	validationURL.RawQuery, validationURL.ForceQuery = "", false
	if _, err := s.egress.ValidateEndpoint(validationURL.String()); err != nil {
		return nil, exchange.fail(classConnect, nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return nil, exchange.fail(classConnect, nil)
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
	if err := s.applySlotCredential(ctx, x, req, p.Provider.Connector(), p.Slot, nil, &p.Provider, p.Model); err != nil {
		if ctx.Err() != nil {
			return nil, exchange.fail(classCancelled, nil)
		}
		return nil, exchange.fail(classCredential, nil)
	}
	for name, values := range headers {
		req.Header.Del(name)
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	return s.doPinnedRequest(ctx, p, exchange, req, openai.ParseErrorBody)
}

type geminiCommand struct {
	query    bool
	finalize bool
}

func parseGeminiUploadCommand(value string) (geminiCommand, *fault.Error) {
	switch strings.TrimSpace(value) {
	case "query":
		return geminiCommand{query: true}, nil
	case "upload":
		return geminiCommand{}, nil
	case "upload, finalize":
		return geminiCommand{finalize: true}, nil
	default:
		param := "X-Goog-Upload-Command"
		return geminiCommand{}, fault.InvalidRequest("invalid_value", "The upload command must be upload, upload, finalize, or query.", &param)
	}
}

func parseGeminiUploadOffset(value string) (int64, *fault.Error) {
	offset, err := strconv.ParseInt(value, 10, 64)
	if err != nil || offset < 0 {
		param := "X-Goog-Upload-Offset"
		return 0, fault.InvalidRequest("invalid_value", "The upload offset must be a non-negative integer.", &param)
	}
	return offset, nil
}

func validateGeminiChunk(expected, stored, offset, length int64, finalize bool) *fault.Error {
	if offset != stored {
		param := "X-Goog-Upload-Offset"
		return fault.InvalidRequest("invalid_value", "The upload offset does not match the stored session offset.", &param)
	}
	if expected <= 0 || stored < 0 || stored > expected || length < 0 || length > expected-stored {
		param := "Content-Length"
		return fault.InvalidRequest("invalid_value", "The upload chunk exceeds the remaining declared file length.", &param)
	}
	if finalize && stored+length != expected {
		param := "Content-Length"
		return fault.InvalidRequest("invalid_value", "The final upload chunk must complete the declared file length.", &param)
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
	metadata := map[string]json.RawMessage{"upstream_model": MustJSON(model)}
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

func projectGeminiFile(body []byte, localID, origin string) ([]byte, error) {
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

func writeGeminiUploadAck(w http.ResponseWriter, x *Call, offset int64, state string) {
	x.ResponseHeaders(w.Header(), false)
	w.Header().Set("X-Goog-Upload-Status", state)
	w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(offset, 10))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) markGeminiUploadFailed(ctx context.Context, resource *resources.Resource) {
	commitCtx, stop := ResourceCommitContext(ctx)
	defer stop()
	if err := s.Deps.Resources.Update(commitCtx, resource.ID, "failed", nil, nil); err != nil {
		s.log.Warn("Gemini upload session failure could not be recorded", "resource_id", resource.ID, "error", err)
	}
}

func (s *Server) geminiUploadCommand(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	session, err := s.Deps.Resources.Get(r.Context(), resources.KindGeminiUpload, authority.ID, r.PathValue("session"))
	if errors.Is(err, resources.ErrNotFound) {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No upload session with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini upload session could not be read."))
		return
	}
	if session.State == "failed" {
		s.Refuse(x, w, fault.ServerError(http.StatusConflict, "resource_transition", "The Gemini upload session has failed."))
		return
	}
	command, commandErr := parseGeminiUploadCommand(r.Header.Get("X-Goog-Upload-Command"))
	if commandErr != nil {
		s.Refuse(x, w, commandErr)
		return
	}
	p, route, pinErr := s.ResolveResource(r.Context(), x, authority, session, "batch", RetainedNewWork)
	if pinErr != nil {
		s.Refuse(x, w, pinErr)
		return
	}
	if x.strict() {
		s.Refuse(x, w, policyUnavailable("target_capability", "Strict routes do not serve Gemini files."))
		return
	}
	if !p.Provider.Connector().Supports("batch", "gemini", "unary") || !p.Provider.Supports(p.Model, "batch", "gemini", "unary") {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No upload session with this identifier exists for this key."))
		return
	}
	x.Route = route
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.Refuse(x, w, err)
		return
	}
	expected, offset, contentType, err := geminiUploadState(session)
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Gemini upload session metadata is invalid."))
		return
	}
	if command.query {
		if r.ContentLength != 0 {
			s.Refuse(x, w, fault.InvalidRequest("invalid_request", "A query upload command cannot contain a body.", nil))
			return
		}
		writeGeminiUploadAck(w, x, offset, "active")
		s.Finish(x, &Outcome{Committed: true}, http.StatusOK)
		return
	}
	if r.ContentLength < 0 {
		param := "Content-Length"
		s.Refuse(x, w, fault.InvalidRequest("missing_required_parameter", "Content-Length is required for Gemini upload chunks.", &param))
		return
	}
	requestOffset, offsetErr := parseGeminiUploadOffset(r.Header.Get("X-Goog-Upload-Offset"))
	if offsetErr != nil {
		s.Refuse(x, w, offsetErr)
		return
	}
	if err := validateGeminiChunk(expected, offset, requestOffset, r.ContentLength, command.finalize); err != nil {
		s.Refuse(x, w, err)
		return
	}
	remaining := expected - offset
	if r.ContentLength > s.Config.MaxMediaBodyBytes {
		s.Refuse(x, w, fault.ServerError(http.StatusRequestEntityTooLarge, "request_too_large", "The Gemini upload chunk exceeds the configured media limit."))
		return
	}
	var batchInput *batchManifest
	var chunk io.Reader = bytes.NewReader(nil)
	var chunkLength int64
	if r.ContentLength > 0 {
		deadline := time.Now().Add(requestBodyTimeout)
		if routeDeadline, ok := ctx.Deadline(); ok && routeDeadline.Before(deadline) {
			deadline = routeDeadline
		}
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(deadline); err != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini chunk read could not be bounded."))
			return
		}
		artifact, err := s.Deps.Staging.Put(ctx, staging.Upload{
			Filename: "gemini-upload-chunk", ContentType: contentType, MaximumLength: min(remaining, s.Config.MaxMediaBodyBytes), Body: r.Body,
		})
		if err != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusBadRequest, "invalid_request", "The Gemini upload chunk could not be staged."))
			return
		}
		// Keep failed reads bounded while HTTP/1 drains their remaining body.
		_ = controller.SetReadDeadline(time.Time{})
		defer s.Deps.Staging.Remove(artifact.Handle)
		opened, err := s.Deps.Staging.Open(artifact.Handle)
		if err != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini chunk could not be read."))
			return
		}
		defer opened.File.Close()
		if offset == 0 && command.finalize && artifact.ContentLength == expected {
			if manifest, qualifyErr := qualifyGeminiBatchJSONL(opened.File, route.Slug, s.batchLineDigest); qualifyErr == nil {
				batchInput = &manifest
			}
			if _, seekErr := opened.File.Seek(0, io.SeekStart); seekErr != nil {
				s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini file could not be read."))
				return
			}
		}
		chunk, chunkLength = pinnedUploadSource{opened.File}, artifact.ContentLength
	}
	uploadURL, err := s.Deps.Resources.ReadGeminiUploadURL(ctx, session.UUID)
	generationURL, generationErr := p.Provider.Connector().URL(wire.Gemini, p.Model, false)
	if err != nil || generationErr != nil || validateGeminiUploadURL(uploadURL, generationURL, s.egress) != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini upload session URL is unavailable."))
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
		if failure.acceptance.Unresolved() || failure.Status == http.StatusNotFound || failure.Status == http.StatusGone {
			s.markGeminiUploadFailed(r.Context(), session)
		}
		s.Refuse(x, w, UpstreamError(failure))
		return
	}
	defer resp.Body.Close()

	newOffset := offset + chunkLength
	if received := resp.Header.Get("X-Goog-Upload-Size-Received"); received != "" {
		reported, parseErr := strconv.ParseInt(received, 10, 64)
		if parseErr != nil || reported != newOffset || reported > expected {
			s.markGeminiUploadFailed(r.Context(), session)
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini upload offset response is invalid."))
			return
		}
		newOffset = reported
	}
	if command.finalize {
		body, readErr := resp.ReadBody(s.Config.MaxResponseBytes)
		if readErr != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The finalized Gemini file response could not be read."))
			return
		}
		var envelope struct {
			File json.RawMessage `json:"file"`
		}
		if json.Unmarshal(body, &envelope) != nil || len(envelope.File) == 0 {
			s.markGeminiUploadFailed(r.Context(), session)
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file object."))
			return
		}
		fileMetadata, state, expires, metadataErr := geminiFileMetadata(envelope.File, p.Model)
		var fileObject map[string]json.RawMessage
		if metadataErr != nil || json.Unmarshal(envelope.File, &fileObject) != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file object."))
			return
		}
		var upstreamName string
		if json.Unmarshal(fileObject["name"], &upstreamName) != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file name is invalid."))
			return
		}
		upstreamID, idErr := geminiFileID(upstreamName)
		if idErr != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file name is invalid."))
			return
		}
		if batchInput != nil {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(fileMetadata, &fields)
			fields["batch_manifest"] = MustJSON(batchInput)
			fileMetadata = MustJSON(fields)
		}
		resp.Exchange.Complete(ClassSuccess)
		file := &resources.Resource{
			Kind: resources.KindGeminiFile, APIKeyID: authority.ID, RouteSlug: route.Slug,
			ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID,
			TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID,
			UpstreamID: upstreamID, State: state, Metadata: fileMetadata, ExpiresAt: expires,
		}
		commitCtx, stopCommit := ResourceCommitContext(ctx)
		created, finalErr := s.Deps.Resources.FinalizeGeminiUpload(commitCtx, authority.ID, session.UUID, newOffset, file)
		stopCommit()
		if finalErr != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The finalized Gemini file could not be stored."))
			return
		}
		projected, projectionErr := projectGeminiFile(envelope.File, created.ID, s.GatewayOrigin(r))
		if projectionErr != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file could not be projected."))
			return
		}
		w.Header().Set("X-Goog-Upload-Status", "final")
		w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(newOffset, 10))
		s.WriteStateJSON(w, x, MustJSON(map[string]json.RawMessage{"file": projected}), resp.Exchange)
		return
	}
	resp.Exchange.Complete(ClassSuccess)
	newMetadata, _ := json.Marshal(map[string]int64{"offset": newOffset})
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	updateErr := s.Deps.Resources.Update(commitCtx, session.ID, "active", newMetadata, nil)
	stopCommit()
	if updateErr != nil {
		s.markGeminiUploadFailed(r.Context(), session)
		s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini upload offset could not be stored."))
		return
	}
	writeGeminiUploadAck(w, x, newOffset, "active")
	s.finishGeminiUploadAck(w, x, resp.Exchange)
}

func (s *Server) finishGeminiUploadAck(w http.ResponseWriter, x *Call, exchange *pinnedExchange) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	if err := controller.Flush(); err != nil {
		exchange.Delivery(usage.ClientUnobserved)
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The upload acknowledgement could not be delivered.")
		s.Finish(x, &Outcome{Err: x.Failure, Committed: true, Cancelled: true}, 0)
		return
	}
	exchange.Delivery(usage.ClientTerminal)
	s.Finish(x, &Outcome{Committed: true}, http.StatusOK)
}

func (s *Server) geminiFileCall(w http.ResponseWriter, r *http.Request, use RetainedUse, op func(context.Context, *Call, keyauthority.Authority, *resources.Resource, *Pin, *routing.Route) *fault.Error) {
	x, authority, done := s.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	localID := strings.TrimPrefix(r.PathValue("name"), "files/")
	res, err := s.Deps.Resources.Get(r.Context(), resources.KindGeminiFile, authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No Gemini file with this name exists for this key."))
		return
	}
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini file mapping could not be read."))
		return
	}
	p, route, pinErr := s.ResolveResource(r.Context(), x, authority, res, "batch", use)
	if pinErr != nil {
		s.Refuse(x, w, pinErr)
		return
	}
	if !p.Provider.Connector().Supports("batch", "gemini", "unary") || !p.Provider.Supports(p.Model, "batch", "gemini", "unary") {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No Gemini file with this name exists for this key."))
		return
	}
	if route.Fidelity.Strict() {
		s.Refuse(x, w, policyUnavailable("target_capability", "Strict routes do not serve Gemini files."))
		return
	}
	x.Route = route
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, false); err != nil {
		s.Refuse(x, w, err)
		return
	}
	if err := op(ctx, x, authority, res, p, route); err != nil {
		s.Refuse(x, w, err)
	}
}

func (s *Server) geminiGetFile(w http.ResponseWriter, r *http.Request) {
	s.geminiFileCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, _ keyauthority.Authority, res *resources.Resource, p *Pin, _ *routing.Route) *fault.Error {
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini file address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.Deps.Resources.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No Gemini file with this name exists for this key.")
			}
			return UpstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := resp.ReadBody(s.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini file response could not be read.")
		}
		metadata, state, expires, err := geminiFileMetadata(body, p.Model)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file.")
		}
		var object map[string]json.RawMessage
		var upstreamName string
		if json.Unmarshal(body, &object) != nil || json.Unmarshal(object["name"], &upstreamName) != nil {
			return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file name.")
		}
		upstreamID, idErr := geminiFileID(upstreamName)
		if idErr != nil || upstreamID != res.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned a mismatched Gemini file name.")
		}
		resp.Exchange.Complete(ClassSuccess)
		var previous, fields map[string]json.RawMessage
		_ = json.Unmarshal(res.Metadata, &previous)
		_ = json.Unmarshal(metadata, &fields)
		if manifest, ok := previous["batch_manifest"]; ok {
			fields["batch_manifest"] = manifest
			metadata = MustJSON(fields)
		}
		if err := s.Deps.Resources.Update(ctx, res.ID, state, metadata, expires); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini file metadata could not be refreshed.")
		}
		projected, err := projectGeminiFile(body, res.ID, s.GatewayOrigin(r))
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file could not be projected.")
		}
		s.WriteStateJSON(w, x, projected, resp.Exchange)
		return nil
	})
}

func (s *Server) geminiDeleteFile(w http.ResponseWriter, r *http.Request) {
	s.geminiFileCall(w, r, RetainedHousekeeping, func(ctx context.Context, x *Call, _ keyauthority.Authority, res *resources.Resource, p *Pin, _ *routing.Route) *fault.Error {
		if res.ParentID != nil {
			if err := s.RequireBatchEvidenceSettled(ctx, *res.ParentID); err != nil {
				return err
			}
		}
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini file address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.Deps.Resources.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No Gemini file with this name exists for this key.")
			}
			return UpstreamError(failure)
		}
		resp.Exchange.Complete(ClassSuccess)
		_ = resp.Body.Close()
		if err := s.Deps.Resources.Tombstone(ctx, res.ID); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini file could not be tombstoned.")
		}
		s.WriteStateJSON(w, x, []byte(`{}`), resp.Exchange)
		return nil
	})
}

func (s *Server) geminiListFiles(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	pageSize, pageToken, cursorErr := geminiPageCursor(r.URL.Query())
	if cursorErr != nil {
		s.Refuse(x, w, cursorErr)
		return
	}
	rows, continuation, listErr := s.ListedResourcePage(authority.ID, "pageToken", pageSize, pageToken, func(cursor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
		batch, err := s.Deps.Resources.ListKindsPage(r.Context(), []string{resources.KindGeminiFile}, authority.ID, count, cursor, false)
		if errors.Is(err, resources.ErrNotFound) {
			param := "pageToken"
			return nil, fault.InvalidRequest("invalid_value", "pageToken must identify a Gemini file owned by this key.", &param)
		}
		if err != nil {
			return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini file list could not be read.")
		}
		return batch, nil
	}, func(row *resources.Resource) *fault.Error {
		return s.AuthorizeListedResource(r.Context(), authority, row)
	})
	if listErr != nil {
		s.Refuse(x, w, listErr)
		return
	}
	files := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		var metadata map[string]json.RawMessage
		if json.Unmarshal(row.Metadata, &metadata) != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "A Gemini file could not be projected."))
			return
		}
		body, _ := json.Marshal(metadata)
		file, projectErr := projectGeminiFile(body, row.ID, s.GatewayOrigin(r))
		if projectErr != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "A Gemini file could not be projected."))
			return
		}
		files = append(files, file)
	}
	response := map[string]any{"files": files}
	if continuation != "" {
		response["nextPageToken"] = continuation
	}
	s.WriteStateJSON(w, x, MustJSON(response))
}

func geminiDownloadURL(cfg connectors.Config, model, fileID string) (string, error) {
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

func (s *Server) geminiDownloadFile(w http.ResponseWriter, r *http.Request) {
	localID, action, ok := strings.Cut(r.PathValue("action"), ":")
	if !ok || action != "download" || localID == "" {
		x, _, done := s.StateBegin(w, r, wire.GeminiFile)
		if !done {
			defer s.Gate.Release(r.Context())
			s.Refuse(x, w, fault.NotFoundError("not_found", "Unknown Gemini file action."))
		}
		return
	}
	r.SetPathValue("name", localID)
	s.geminiFileCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, _ keyauthority.Authority, res *resources.Resource, p *Pin, _ *routing.Route) *fault.Error {
		endpoint, err := geminiDownloadURL(p.Provider.Connector(), p.Model, res.UpstreamID)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini download address could not be resolved.")
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
		artifact, err := resp.StageBody(ctx, s.Deps.Staging, "gemini-file", contentType, s.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Gemini download exceeded the configured response limit.")
		}
		resp.Exchange.Complete(ClassSuccess)
		defer s.Deps.Staging.Remove(artifact.Handle)
		opened, err := s.Deps.Staging.Open(artifact.Handle)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini download could not be read.")
		}
		defer opened.File.Close()
		SetFileDownloadHeaders(w.Header(), contentType, artifact.ContentLength)
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini download response could not be bounded.")
		}
		x.ResponseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := CopyResourceContent(w, opened.File, artifact.ContentLength)
		if copyErr != nil || written != artifact.ContentLength {
			resp.Exchange.Delivery(DeliveryState(written))
			x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The Gemini download ended before all bytes were delivered.")
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

func geminiPageCursor(query url.Values) (int, string, *fault.Error) {
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
	if token != "" && !strings.HasPrefix(token, RetainedListCursorPrefix) && !validGeminiFileID(token) {
		param := "pageToken"
		return 0, "", fault.InvalidRequest("invalid_value", "pageToken must be a local Gemini file identifier.", &param)
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
