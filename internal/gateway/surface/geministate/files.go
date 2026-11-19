package geministate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s surface) geminiResourceGates(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, p *kernel.Pin, route *routing.Route, newWork bool) *fault.Error {
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

func (s surface) geminiUploadStart(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	route, err := s.kernel.NativeStateRoute(x, authority, r, "")
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	expected, err := validateGeminiStartHeaders(r.Header.Get("X-Goog-Upload-Protocol"), r.Header.Get("X-Goog-Upload-Command"),
		r.Header.Get("X-Goog-Upload-Header-Content-Length"), s.kernel.Config.MaxMediaBodyBytes)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	body, readErr := s.kernel.ReadResourceBody(w, r)
	if readErr != nil {
		s.kernel.Refuse(x, w, readErr)
		return
	}
	displayName, hasDisplayName, err := parseGeminiUploadStartBody(body)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	if !s.kernel.Deps.Resources.Encrypted() {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Gemini upload sessions require encrypted resource storage."))
		return
	}
	p, err := s.kernel.NativeStatePin(r.Context(), x, route)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	endpoint, originErr := kernel.GeminiUploadOrigin(p.Provider.Connector(), p.Model, s.kernel.Egress)
	if originErr != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini upload address could not be resolved."))
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
	resp, failure := s.kernel.GeminiPinnedRequest(ctx, x, p, http.MethodPost, endpoint, bytes.NewReader(payload), int64(len(payload)), "application/json", "application/json", headers)
	if failure != nil {
		s.kernel.Refuse(x, w, kernel.UpstreamError(failure))
		return
	}
	defer resp.Body.Close()
	uploadURLs := resp.Header.Values("X-Goog-Upload-URL")
	if len(uploadURLs) != 1 {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider did not return one resumable upload URL."))
		return
	}
	sessionURL := uploadURLs[0]
	generationURL, _ := p.Provider.Connector().URL(wire.Gemini, p.Model, false)
	if err := kernel.ValidateGeminiUploadURL(sessionURL, generationURL, s.kernel.Egress); err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned an invalid resumable upload URL."))
		return
	}
	resp.Exchange.Complete(kernel.ClassSuccess)
	now := s.kernel.Now()
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
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	resource, storeErr := s.kernel.Deps.Resources.PutGeminiUploadSession(commitCtx, &resources.Resource{
		UUID: id, Kind: resources.KindGeminiUpload, APIKeyID: authority.ID, RouteSlug: route.Slug,
		ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID,
		TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID,
		UpstreamID: id.String(), State: "active", Metadata: kernel.MustJSON(metadata), ExpiresAt: &expires,
	}, sessionURL)
	stopCommit()
	if storeErr != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The upload session could not be stored."))
		return
	}

	x.ResponseHeaders(w.Header(), false)
	w.Header().Set("X-Goog-Upload-Status", "active")
	w.Header().Set("X-Goog-Upload-URL", s.kernel.GatewayOrigin(r)+"/gemini/upload/"+url.PathEscape(resource.ID))
	w.WriteHeader(http.StatusOK)
	s.finishGeminiUploadAck(w, x, resp.Exchange)
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

func writeGeminiUploadAck(w http.ResponseWriter, x *kernel.Call, offset int64, state string) {
	x.ResponseHeaders(w.Header(), false)
	w.Header().Set("X-Goog-Upload-Status", state)
	w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(offset, 10))
	w.WriteHeader(http.StatusOK)
}

func (s surface) markGeminiUploadFailed(ctx context.Context, resource *resources.Resource) {
	commitCtx, stop := kernel.ResourceCommitContext(ctx)
	defer stop()
	if err := s.kernel.Deps.Resources.Update(commitCtx, resource.ID, "failed", nil, nil); err != nil {
		s.kernel.Log.Warn("Gemini upload session failure could not be recorded", "resource_id", resource.ID, "error", err)
	}
}

func (s surface) geminiUploadCommand(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	session, err := s.kernel.Deps.Resources.Get(r.Context(), resources.KindGeminiUpload, authority.ID, r.PathValue("session"))
	if errors.Is(err, resources.ErrNotFound) {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No upload session with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini upload session could not be read."))
		return
	}
	if session.State == "failed" {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusConflict, "resource_transition", "The Gemini upload session has failed."))
		return
	}
	command, commandErr := parseGeminiUploadCommand(r.Header.Get("X-Goog-Upload-Command"))
	if commandErr != nil {
		s.kernel.Refuse(x, w, commandErr)
		return
	}
	p, route, pinErr := s.kernel.ResolveResource(r.Context(), x, authority, session, "batch", kernel.RetainedNewWork)
	if pinErr != nil {
		s.kernel.Refuse(x, w, pinErr)
		return
	}
	if x.Strict() {
		s.kernel.Refuse(x, w, kernel.PolicyUnavailable("target_capability", "Strict routes do not serve Gemini files."))
		return
	}
	if !p.Provider.Connector().Supports("batch", "gemini", "unary") || !p.Provider.Supports(p.Model, "batch", "gemini", "unary") {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No upload session with this identifier exists for this key."))
		return
	}
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	expected, offset, contentType, err := geminiUploadState(session)
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Gemini upload session metadata is invalid."))
		return
	}
	if command.query {
		if r.ContentLength != 0 {
			s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "A query upload command cannot contain a body.", nil))
			return
		}
		writeGeminiUploadAck(w, x, offset, "active")
		s.kernel.Finish(x, &kernel.Outcome{Committed: true}, http.StatusOK)
		return
	}
	if r.ContentLength < 0 {
		param := "Content-Length"
		s.kernel.Refuse(x, w, fault.InvalidRequest("missing_required_parameter", "Content-Length is required for Gemini upload chunks.", &param))
		return
	}
	requestOffset, offsetErr := parseGeminiUploadOffset(r.Header.Get("X-Goog-Upload-Offset"))
	if offsetErr != nil {
		s.kernel.Refuse(x, w, offsetErr)
		return
	}
	if err := validateGeminiChunk(expected, offset, requestOffset, r.ContentLength, command.finalize); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	remaining := expected - offset
	if r.ContentLength > s.kernel.Config.MaxMediaBodyBytes {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusRequestEntityTooLarge, "request_too_large", "The Gemini upload chunk exceeds the configured media limit."))
		return
	}
	var batchInput *kernel.BatchManifest
	var chunk io.Reader = bytes.NewReader(nil)
	var chunkLength int64
	if r.ContentLength > 0 {
		deadline := time.Now().Add(kernel.RequestBodyTimeout)
		if routeDeadline, ok := ctx.Deadline(); ok && routeDeadline.Before(deadline) {
			deadline = routeDeadline
		}
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(deadline); err != nil {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini chunk read could not be bounded."))
			return
		}
		artifact, err := s.kernel.Deps.Staging.Put(ctx, staging.Upload{
			Filename: "gemini-upload-chunk", ContentType: contentType, MaximumLength: min(remaining, s.kernel.Config.MaxMediaBodyBytes), Body: r.Body,
		})
		if err != nil {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadRequest, "invalid_request", "The Gemini upload chunk could not be staged."))
			return
		}
		// Keep failed reads bounded while HTTP/1 drains their remaining body.
		_ = controller.SetReadDeadline(time.Time{})
		defer s.kernel.Deps.Staging.Remove(artifact.Handle)
		opened, err := s.kernel.Deps.Staging.Open(artifact.Handle)
		if err != nil {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini chunk could not be read."))
			return
		}
		defer opened.File.Close()
		if offset == 0 && command.finalize && artifact.ContentLength == expected {
			if manifest, qualifyErr := qualifyGeminiBatchJSONL(opened.File, route.Slug, s.kernel.BatchLineDigest); qualifyErr == nil {
				batchInput = &manifest
			}
			if _, seekErr := opened.File.Seek(0, io.SeekStart); seekErr != nil {
				s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini file could not be read."))
				return
			}
		}
		chunk, chunkLength = kernel.PinnedUploadSource{Reader: opened.File}, artifact.ContentLength
	}
	uploadURL, err := s.kernel.Deps.Resources.ReadGeminiUploadURL(ctx, session.UUID)
	generationURL, generationErr := p.Provider.Connector().URL(wire.Gemini, p.Model, false)
	if err != nil || generationErr != nil || kernel.ValidateGeminiUploadURL(uploadURL, generationURL, s.kernel.Egress) != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini upload session URL is unavailable."))
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
	resp, failure := s.kernel.GeminiPinnedRequest(ctx, x, p, http.MethodPost, uploadURL, chunk, chunkLength, contentType, "application/json", headers)
	if failure != nil {
		if failure.Acceptance.Unresolved() || failure.Status == http.StatusNotFound || failure.Status == http.StatusGone {
			s.markGeminiUploadFailed(r.Context(), session)
		}
		s.kernel.Refuse(x, w, kernel.UpstreamError(failure))
		return
	}
	defer resp.Body.Close()

	newOffset := offset + chunkLength
	if received := resp.Header.Get("X-Goog-Upload-Size-Received"); received != "" {
		reported, parseErr := strconv.ParseInt(received, 10, 64)
		if parseErr != nil || reported != newOffset || reported > expected {
			s.markGeminiUploadFailed(r.Context(), session)
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini upload offset response is invalid."))
			return
		}
		newOffset = reported
	}
	if command.finalize {
		body, readErr := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
		if readErr != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The finalized Gemini file response could not be read."))
			return
		}
		var envelope struct {
			File json.RawMessage `json:"file"`
		}
		if json.Unmarshal(body, &envelope) != nil || len(envelope.File) == 0 {
			s.markGeminiUploadFailed(r.Context(), session)
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file object."))
			return
		}
		fileMetadata, state, expires, metadataErr := geminiFileMetadata(envelope.File, p.Model)
		var fileObject map[string]json.RawMessage
		if metadataErr != nil || json.Unmarshal(envelope.File, &fileObject) != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file object."))
			return
		}
		var upstreamName string
		if json.Unmarshal(fileObject["name"], &upstreamName) != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file name is invalid."))
			return
		}
		upstreamID, idErr := kernel.GeminiFileID(upstreamName)
		if idErr != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file name is invalid."))
			return
		}
		if batchInput != nil {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(fileMetadata, &fields)
			fields["batch_manifest"] = kernel.MustJSON(batchInput)
			fileMetadata = kernel.MustJSON(fields)
		}
		resp.Exchange.Complete(kernel.ClassSuccess)
		file := &resources.Resource{
			Kind: resources.KindGeminiFile, APIKeyID: authority.ID, RouteSlug: route.Slug,
			ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID,
			TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID,
			UpstreamID: upstreamID, State: state, Metadata: fileMetadata, ExpiresAt: expires,
		}
		commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
		created, finalErr := s.kernel.Deps.Resources.FinalizeGeminiUpload(commitCtx, authority.ID, session.UUID, newOffset, file)
		stopCommit()
		if finalErr != nil {
			s.markGeminiUploadFailed(r.Context(), session)
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The finalized Gemini file could not be stored."))
			return
		}
		projected, projectionErr := projectGeminiFile(envelope.File, created.ID, s.kernel.GatewayOrigin(r))
		if projectionErr != nil {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file could not be projected."))
			return
		}
		w.Header().Set("X-Goog-Upload-Status", "final")
		w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(newOffset, 10))
		s.kernel.WriteStateJSON(w, x, kernel.MustJSON(map[string]json.RawMessage{"file": projected}), resp.Exchange)
		return
	}
	resp.Exchange.Complete(kernel.ClassSuccess)
	newMetadata, _ := json.Marshal(map[string]int64{"offset": newOffset})
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	updateErr := s.kernel.Deps.Resources.Update(commitCtx, session.ID, "active", newMetadata, nil)
	stopCommit()
	if updateErr != nil {
		s.markGeminiUploadFailed(r.Context(), session)
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini upload offset could not be stored."))
		return
	}
	writeGeminiUploadAck(w, x, newOffset, "active")
	s.finishGeminiUploadAck(w, x, resp.Exchange)
}

func (s surface) finishGeminiUploadAck(w http.ResponseWriter, x *kernel.Call, exchange *kernel.PinnedExchange) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	if err := controller.Flush(); err != nil {
		exchange.Delivery(usage.ClientUnobserved)
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The upload acknowledgement could not be delivered.")
		s.kernel.Finish(x, &kernel.Outcome{Err: x.Failure, Committed: true, Cancelled: true}, 0)
		return
	}
	exchange.Delivery(usage.ClientTerminal)
	s.kernel.Finish(x, &kernel.Outcome{Committed: true}, http.StatusOK)
}

func (s surface) geminiFileCall(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, op func(context.Context, *kernel.Call, keyauthority.Authority, *resources.Resource, *kernel.Pin, *routing.Route) *fault.Error) {
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
	if err := s.geminiResourceGates(ctx, x, authority, p, route, false); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	if err := op(ctx, x, authority, res, p, route); err != nil {
		s.kernel.Refuse(x, w, err)
	}
}

func (s surface) geminiGetFile(w http.ResponseWriter, r *http.Request) {
	s.geminiFileCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, res *resources.Resource, p *kernel.Pin, _ *routing.Route) *fault.Error {
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
		metadata, state, expires, err := geminiFileMetadata(body, p.Model)
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
		projected, err := projectGeminiFile(body, res.ID, s.kernel.GatewayOrigin(r))
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file could not be projected.")
		}
		s.kernel.WriteStateJSON(w, x, projected, resp.Exchange)
		return nil
	})
}

func (s surface) geminiDeleteFile(w http.ResponseWriter, r *http.Request) {
	s.geminiFileCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, res *resources.Resource, p *kernel.Pin, _ *routing.Route) *fault.Error {
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

func (s surface) geminiListFiles(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	pageSize, pageToken, cursorErr := geminiPageCursor(r.URL.Query())
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
		file, projectErr := projectGeminiFile(body, row.ID, s.kernel.GatewayOrigin(r))
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

func (s surface) geminiDownloadFile(w http.ResponseWriter, r *http.Request) {
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
	s.geminiFileCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, res *resources.Resource, p *kernel.Pin, _ *routing.Route) *fault.Error {
		endpoint, err := geminiDownloadURL(p.Provider.Connector(), p.Model, res.UpstreamID)
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
		x.ResponseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := kernel.CopyResourceContent(w, opened.File, artifact.ContentLength)
		if copyErr != nil || written != artifact.ContentLength {
			resp.Exchange.Delivery(kernel.DeliveryState(written))
			x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The Gemini download ended before all bytes were delivered.")
			s.kernel.Finish(x, &kernel.Outcome{Err: x.Failure, Committed: true, Cancelled: true}, x.Failure.Status)
			return nil
		}
		if written > 0 {
			x.Delivered(s.kernel.Now())
		}
		resp.Exchange.Delivery(usage.ClientTerminal)
		s.kernel.Finish(x, &kernel.Outcome{Committed: true}, http.StatusOK)
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
	if token != "" && !strings.HasPrefix(token, kernel.RetainedListCursorPrefix) && !kernel.ValidGeminiFileID(token) {
		param := "pageToken"
		return 0, "", fault.InvalidRequest("invalid_value", "pageToken must be a local Gemini file identifier.", &param)
	}
	return pageSize, token, nil
}
