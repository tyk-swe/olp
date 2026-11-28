package geministate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type uploadCommand struct {
	query    bool
	finalize bool
}

func parseCommand(value string) (uploadCommand, *fault.Error) {
	switch strings.TrimSpace(value) {
	case "query":
		return uploadCommand{query: true}, nil
	case "upload":
		return uploadCommand{}, nil
	case "upload, finalize":
		return uploadCommand{finalize: true}, nil
	default:
		param := "X-Goog-Upload-Command"
		return uploadCommand{}, fault.InvalidRequest("invalid_value", "The upload command must be upload, upload, finalize, or query.", &param)
	}
}

func parseOffset(value string) (int64, *fault.Error) {
	offset, err := strconv.ParseInt(value, 10, 64)
	if err != nil || offset < 0 {
		param := "X-Goog-Upload-Offset"
		return 0, fault.InvalidRequest("invalid_value", "The upload offset must be a non-negative integer.", &param)
	}
	return offset, nil
}

func validateChunk(expected, stored, offset, length int64, finalize bool) *fault.Error {
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

func acknowledge(w http.ResponseWriter, x *kernel.Call, offset int64, state string) {
	x.ResponseHeaders(w.Header(), false)
	w.Header().Set("X-Goog-Upload-Status", state)
	w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(offset, 10))
	w.WriteHeader(http.StatusOK)
}

func (s surface) markFailed(ctx context.Context, resource *resources.Resource) {
	commitCtx, stop := kernel.ResourceCommitContext(ctx)
	defer stop()
	if err := s.store.Update(commitCtx, resource.ID, "failed", nil, nil); err != nil {
		s.kernel.Log.Warn("Gemini upload session failure could not be recorded", "resource_id", resource.ID, "error", err)
	}
}

func (s surface) finishAck(w http.ResponseWriter, x *kernel.Call, exchange *kernel.PinnedExchange) {
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

type sessionState struct {
	expected, offset int64
	contentType      string
}

func readState(resource *resources.Resource) (sessionState, error) {
	var metadata struct {
		Expected int64  `json:"expected_length"`
		Offset   int64  `json:"offset"`
		Type     string `json:"content_type"`
	}
	if json.Unmarshal(resource.Metadata, &metadata) != nil || metadata.Expected <= 0 || metadata.Offset < 0 || metadata.Offset > metadata.Expected {
		return sessionState{}, resources.ErrContract
	}
	return sessionState{expected: metadata.Expected, offset: metadata.Offset, contentType: metadata.Type}, nil
}

func (c uploadCommand) header() string {
	if c.finalize {
		return "upload, finalize"
	}
	return "upload"
}

func acceptsChunk(length int64, rawOffset string, state sessionState, finalize bool, maximum int64) *fault.Error {
	if length < 0 {
		param := "Content-Length"
		return fault.InvalidRequest("missing_required_parameter", "Content-Length is required for Gemini upload chunks.", &param)
	}
	offset, err := parseOffset(rawOffset)
	if err != nil {
		return err
	}
	if err := validateChunk(state.expected, state.offset, offset, length, finalize); err != nil {
		return err
	}
	if length > maximum {
		return fault.ServerError(http.StatusRequestEntityTooLarge, "request_too_large", "The Gemini upload chunk exceeds the configured media limit.")
	}
	return nil
}

func receivedOffset(header http.Header, state sessionState, length int64) (int64, bool) {
	offset := state.offset + length
	received := header.Get("X-Goog-Upload-Size-Received")
	if received == "" {
		return offset, true
	}
	reported, err := strconv.ParseInt(received, 10, 64)
	return offset, err == nil && reported == offset && reported <= state.expected
}

func (s surface) continueUpload(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	session, command, err := s.session(r, authority)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	p, route, err := s.sessionPin(r.Context(), x, authority, session)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.gates(ctx, x, authority, p, route, true); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	state, stateErr := readState(session)
	if stateErr != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Gemini upload session metadata is invalid."))
		return
	}
	u := upload{surface: s, w: w, r: r, x: x, authority: authority, session: session, route: route, pin: p, state: state, command: command}
	if command.query {
		u.report()
		return
	}
	if err := acceptsChunk(r.ContentLength, r.Header.Get("X-Goog-Upload-Offset"), state, command.finalize, s.kernel.Config.MaxMediaBodyBytes); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	staged, err := u.stage(ctx)
	defer s.unstage(staged)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	resp, err := u.send(ctx, staged)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	defer resp.Body.Close()
	offset, consistent := receivedOffset(resp.Header, state, staged.length)
	if !consistent {
		u.fail()
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini upload offset response is invalid."))
		return
	}
	if command.finalize {
		u.finalize(ctx, resp, offset, staged.manifest)
		return
	}
	u.advance(ctx, resp, offset)
}

func (s surface) session(r *http.Request, authority keyauthority.Authority) (*resources.Resource, uploadCommand, *fault.Error) {
	session, err := s.store.Get(r.Context(), resources.KindGeminiUpload, authority.ID, r.PathValue("session"))
	if errors.Is(err, resources.ErrNotFound) {
		return nil, uploadCommand{}, fault.NotFoundError("not_found", "No upload session with this identifier exists for this key.")
	}
	if err != nil {
		return nil, uploadCommand{}, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini upload session could not be read.")
	}
	if session.State == "failed" {
		return nil, uploadCommand{}, fault.ServerError(http.StatusConflict, "resource_transition", "The Gemini upload session has failed.")
	}
	command, commandErr := parseCommand(r.Header.Get("X-Goog-Upload-Command"))
	return session, command, commandErr
}

func (s surface) sessionPin(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, session *resources.Resource) (*kernel.Pin, *routing.Route, *fault.Error) {
	p, route, err := s.kernel.ResolveResource(ctx, x, authority, session, "batch", kernel.RetainedNewWork)
	if err != nil {
		return nil, nil, err
	}
	if x.Strict() {
		return nil, nil, kernel.PolicyUnavailable("target_capability", "Strict routes do not serve Gemini files.")
	}
	if !p.Provider.Connector().Supports("batch", "gemini", "unary") || !p.Provider.Supports(p.Model, "batch", "gemini", "unary") {
		return nil, nil, fault.NotFoundError("not_found", "No upload session with this identifier exists for this key.")
	}
	return p, route, nil
}

type upload struct {
	surface
	w         http.ResponseWriter
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	session   *resources.Resource
	route     *routing.Route
	pin       *kernel.Pin
	state     sessionState
	command   uploadCommand
}

func (u upload) fail() {
	u.markFailed(u.r.Context(), u.session)
}

func (u upload) report() {
	if u.r.ContentLength != 0 {
		u.kernel.Refuse(u.x, u.w, fault.InvalidRequest("invalid_request", "A query upload command cannot contain a body.", nil))
		return
	}
	acknowledge(u.w, u.x, u.state.offset, "active")
	u.kernel.Finish(u.x, &kernel.Outcome{Committed: true}, http.StatusOK)
}

type chunk struct {
	body     io.Reader
	length   int64
	manifest *managedbatch.Manifest
	handle   staging.Handle
	file     *os.File
}

func chunkDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(kernel.RequestBodyTimeout)
	if bound, ok := ctx.Deadline(); ok && bound.Before(deadline) {
		return bound
	}
	return deadline
}

func (u upload) stage(ctx context.Context) (chunk, *fault.Error) {
	if u.r.ContentLength == 0 {
		return chunk{body: bytes.NewReader(nil)}, nil
	}
	controller := http.NewResponseController(u.w)
	if err := controller.SetReadDeadline(chunkDeadline(ctx)); err != nil {
		return chunk{}, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini chunk read could not be bounded.")
	}
	artifact, err := u.spool.Put(ctx, staging.Upload{
		Filename: "gemini-upload-chunk", ContentType: u.state.contentType, Body: u.r.Body,
		MaximumLength: min(u.state.expected-u.state.offset, u.kernel.Config.MaxMediaBodyBytes),
	})
	if err != nil {
		return chunk{}, fault.ServerError(http.StatusBadRequest, "invalid_request", "The Gemini upload chunk could not be staged.")
	}
	// Keep failed reads bounded while HTTP/1 drains their remaining body.
	_ = controller.SetReadDeadline(time.Time{})
	opened, err := u.spool.Open(artifact.Handle)
	if err != nil {
		return chunk{handle: artifact.Handle}, fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini chunk could not be read.")
	}
	staged := chunk{body: kernel.PinnedUploadSource{Reader: opened.File}, length: artifact.ContentLength, handle: artifact.Handle, file: opened.File}
	if u.state.offset != 0 || !u.command.finalize || artifact.ContentLength != u.state.expected {
		return staged, nil
	}
	manifest, qualifyErr := qualifyJSONL(opened.File, u.route.Slug, u.lineDigest)
	if _, err := opened.File.Seek(0, io.SeekStart); err != nil {
		return staged, fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged Gemini file could not be read.")
	}
	if qualifyErr != nil {
		return staged, nil
	}
	return chunk{body: staged.body, length: staged.length, manifest: &manifest, handle: staged.handle, file: staged.file}, nil
}

func (s surface) unstage(staged chunk) {
	if staged.file != nil {
		staged.file.Close()
	}
	if staged.handle != "" {
		_ = s.spool.Remove(staged.handle)
	}
}

func (u upload) send(ctx context.Context, staged chunk) (*kernel.PinnedResponse, *fault.Error) {
	uploadURL, err := u.store.ReadGeminiUploadURL(ctx, u.session.UUID)
	generationURL, generationErr := u.pin.Provider.Connector().URL(wire.Gemini, u.pin.Model, false)
	if err != nil || generationErr != nil || kernel.ValidateGeminiUploadURL(uploadURL, generationURL, u.kernel.Egress) != nil {
		return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini upload session URL is unavailable.")
	}
	headers := http.Header{
		"X-Goog-Upload-Command": {u.command.header()},
		"X-Goog-Upload-Offset":  {strconv.FormatInt(u.state.offset, 10)},
	}
	resp, failure := u.kernel.GeminiPinnedRequest(ctx, u.x, u.pin, http.MethodPost, uploadURL, staged.body, staged.length, u.state.contentType, "application/json", headers)
	if failure == nil {
		return resp, nil
	}
	if failure.Acceptance.Unresolved() || failure.Status == http.StatusNotFound || failure.Status == http.StatusGone {
		u.fail()
	}
	return nil, kernel.UpstreamError(failure)
}

func (u upload) advance(ctx context.Context, resp *kernel.PinnedResponse, offset int64) {
	resp.Exchange.Complete(kernel.ClassSuccess)
	metadata, _ := json.Marshal(map[string]int64{"offset": offset})
	commitCtx, stop := kernel.ResourceCommitContext(ctx)
	err := u.store.Update(commitCtx, u.session.ID, "active", metadata, nil)
	stop()
	if err != nil {
		u.fail()
		u.kernel.Refuse(u.x, u.w, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini upload offset could not be stored."))
		return
	}
	acknowledge(u.w, u.x, offset, "active")
	u.finishAck(u.w, u.x, resp.Exchange)
}

func (u upload) finalize(ctx context.Context, resp *kernel.PinnedResponse, offset int64, manifest *managedbatch.Manifest) {
	reply, err := u.bind(ctx, resp, offset, manifest)
	if err != nil {
		u.kernel.Refuse(u.x, u.w, err)
		return
	}
	u.w.Header().Set("X-Goog-Upload-Status", "final")
	u.w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(offset, 10))
	u.kernel.WriteStateJSON(u.w, u.x, reply, resp.Exchange)
}

func (u upload) bind(ctx context.Context, resp *kernel.PinnedResponse, offset int64, manifest *managedbatch.Manifest) ([]byte, *fault.Error) {
	body, readErr := resp.ReadBody(u.kernel.Config.MaxResponseBytes)
	if readErr != nil {
		u.fail()
		return nil, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The finalized Gemini file response could not be read.")
	}
	file, err := readFinalFile(body, u.pin.Model)
	if err != nil {
		u.fail()
		return nil, err
	}
	resp.Exchange.Complete(kernel.ClassSuccess)
	commitCtx, stop := kernel.ResourceCommitContext(ctx)
	created, storeErr := u.store.FinalizeGeminiUpload(commitCtx, u.authority.ID, u.session.UUID, offset, &resources.Resource{
		Kind: resources.KindGeminiFile, APIKeyID: u.authority.ID, RouteSlug: u.route.Slug,
		ProviderID: u.pin.Provider.ID, ProviderRevisionID: u.pin.Provider.RevisionID, RouteRevisionID: u.route.RevisionID,
		TargetID: u.pin.Target.ID, SlotID: u.pin.Slot.ID, CredentialID: u.pin.Slot.CredentialID,
		UpstreamID: file.upstreamID, State: file.state, Metadata: withManifest(file.metadata, manifest), ExpiresAt: file.expires,
	})
	stop()
	if storeErr != nil {
		u.fail()
		return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The finalized Gemini file could not be stored.")
	}
	projected, projectionErr := projectFile(file.object, created.ID, u.kernel.GatewayOrigin(u.r))
	if projectionErr != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file could not be projected.")
	}
	return kernel.MustJSON(map[string]json.RawMessage{"file": projected}), nil
}

type finalFile struct {
	object     json.RawMessage
	upstreamID string
	state      string
	expires    *time.Time
	metadata   json.RawMessage
}

func readFinalFile(body []byte, model string) (finalFile, *fault.Error) {
	invalid := fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Gemini file object.")
	var envelope struct {
		File json.RawMessage `json:"file"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.File) == 0 {
		return finalFile{}, invalid
	}
	metadata, state, expires, err := fileMetadata(envelope.File, model)
	var object map[string]json.RawMessage
	if err != nil || json.Unmarshal(envelope.File, &object) != nil {
		return finalFile{}, invalid
	}
	unnamed := fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Gemini file name is invalid.")
	var name string
	if json.Unmarshal(object["name"], &name) != nil {
		return finalFile{}, unnamed
	}
	upstreamID, err := kernel.GeminiFileID(name)
	if err != nil {
		return finalFile{}, unnamed
	}
	return finalFile{object: envelope.File, upstreamID: upstreamID, state: state, expires: expires, metadata: metadata}, nil
}

func withManifest(metadata json.RawMessage, manifest *managedbatch.Manifest) json.RawMessage {
	if manifest == nil {
		return metadata
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(metadata, &fields)
	fields["batch_manifest"] = kernel.MustJSON(manifest)
	return kernel.MustJSON(fields)
}
