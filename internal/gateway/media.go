package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/runtime"
)

// MediaDeps wires the bounded media substrate into the public surface. A nil
// MediaDeps leaves the media routes unregistered.
type MediaDeps struct {
	Jobs      *media.Service
	Admission *media.AdmissionState
}

// transport returns the media upstream transport.
func (s *Server) transport() *media.Transport { return s.Media.Jobs.Transport }

// multipartParseDeadline bounds one inbound multipart body.
const multipartParseDeadline = 60 * time.Second

// Multipart media uses the same admission estimates as the Rust gateway.
const mediaMultipartTokens = 2000

func (s *Server) registerMedia(mux *http.ServeMux) {
	if s.Media == nil {
		return
	}
	mux.HandleFunc("POST /v1/images/generations", s.imageGenerations)
	mux.HandleFunc("POST /v1/images/edits", s.mediaFormHandler(media.DefaultImageUploadLimit, 33, media.DecodeImageEdit))
	mux.HandleFunc("POST /v1/images/variations", s.mediaFormHandler(media.DefaultImageUploadLimit, 1, media.DecodeImageVariation))
	mux.HandleFunc("POST /v1/audio/speech", s.mediaJSONHandler(media.DecodeSpeech))
	mux.HandleFunc("POST /v1/audio/transcriptions", s.mediaFormHandler(media.DefaultAudioUploadLimit, 1, media.DecodeTranscription))
	mux.HandleFunc("POST /v1/videos", s.videoCreate)
	mux.HandleFunc("GET /v1/videos", s.videoList)
	mux.HandleFunc("GET /v1/videos/{video_id}", s.videoGet)
	mux.HandleFunc("GET /v1/videos/{video_id}/content", s.videoContent)
	mux.HandleFunc("DELETE /v1/videos/{video_id}", s.videoDelete)
}

// mediaJSONHandler serves a JSON-bodied media operation.
func (s *Server) mediaJSONHandler(decode func([]byte) (*media.Request, *media.Error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		x, authority, done := s.mediaBegin(w, r)
		if done {
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(requestBodyTimeout)); err != nil {
			s.release(r.Context())
			s.mediaFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The request could not be read."))
			return
		}
		body, e := s.readBody(r)
		if e != nil {
			s.release(r.Context())
			s.mediaFail(x, w, e)
			return
		}
		// Failed reads retain the deadline so HTTP/1 body draining is bounded.
		rc.SetReadDeadline(time.Time{})
		request, failure := decode(body)
		if failure != nil {
			s.release(r.Context())
			s.mediaFail(x, w, mediaError(failure))
			return
		}
		x.family = openai.Family(request.Op)
		x.estimate = (int64(len(body))+3)/4 + 1
		s.serveMedia(r.Context(), w, x, authority, request)
	}
}

// mediaFormHandler serves a multipart media operation. Staged uploads live in
// the spool until the decoded request takes ownership of them.
func (s *Server) mediaFormHandler(maxFileBytes int64, maxFiles int, decode func(*media.Form) (*media.Request, *media.Error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		x, authority, done := s.mediaBegin(w, r)
		if done {
			return
		}
		form, e := s.parseMediaForm(w, r, authority.ID, maxFileBytes, maxFiles)
		if e != nil {
			s.release(r.Context())
			s.mediaFail(x, w, e)
			return
		}
		request, failure := decode(form)
		if failure != nil {
			form.Cleanup()
			s.release(r.Context())
			s.mediaFail(x, w, mediaError(failure))
			return
		}
		form.Disarm()
		x.family = openai.Family(request.Op)
		s.serveMedia(r.Context(), w, x, authority, request)
	}
}

// parseMediaForm bounds both the raw request and the spool bytes promised to
// its parser. The aggregate body cap also bounds multi-image reservations.
func (s *Server) parseMediaForm(w http.ResponseWriter, r *http.Request, keyID string, maxFileBytes int64, maxFiles int) (*media.Form, *Error) {
	limit := s.cfg.MaxMediaBodyBytes
	if r.ContentLength > limit {
		return nil, bodyTooLarge()
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(multipartParseDeadline))
	defer rc.SetReadDeadline(time.Time{})
	admission := media.Admission{
		Lease: s.Media.Admission.TryAdmit(keyID, min(limit, maxFileBytes*int64(maxFiles))),
		Route: media.RouteAdmission{Kind: media.RouteUnrestricted},
	}
	form, err := media.ParseMultipart(r.Context(), r, s.transport().Spool, &admission, maxFileBytes, maxFiles)
	if err != nil {
		return nil, mediaError(mediaErr(err))
	}
	return form, nil
}

// mediaBegin runs the shared prelude: identity, admission, authentication,
// and the routing header every route-planned media operation shares.
func (s *Server) mediaBegin(w http.ResponseWriter, r *http.Request) (*execution, access.Authority, bool) {
	x := &execution{request: s.begin(w, r), family: openai.FamilyChat, actor: "api_key"}
	if !s.admit(r.Context()) {
		s.mediaFail(x, w, overloaded)
		return x, access.Authority{}, true
	}
	authority, e := s.authenticate(r, "inference")
	if e != nil {
		s.release(r.Context())
		s.mediaFail(x, w, e)
		return x, access.Authority{}, true
	}
	if x.preferences, e = routingPreferences(r); e != nil {
		s.release(r.Context())
		s.mediaFail(x, w, e)
		return x, access.Authority{}, true
	}
	x.keyID, x.affinity = authority.ID, []byte(authority.ID)
	x.budgetGroupID = authority.BudgetGroupID
	if x.attribution, e = s.parseAttribution(r, authority); e != nil {
		s.release(r.Context())
		s.mediaFail(x, w, e)
		return x, access.Authority{}, true
	}
	return x, authority, false
}

// mediaFail records and writes a pre-dispatch media failure.
func (s *Server) mediaFail(x *execution, w http.ResponseWriter, e *Error) {
	x.failure = e
	s.finish(x, nil, e.Status)
	writeSurfaceError(w, e, "openai")
}

// serveMedia plans and executes one media operation, then renders the result.
func (s *Server) serveMedia(ctx context.Context, w http.ResponseWriter, x *execution, authority access.Authority, request *media.Request) {
	defer s.release(ctx)
	x.media = request
	switch request.Op {
	case media.OpImageEdit, media.OpImageVariation:
		x.estimate = mediaMultipartTokens
	case media.OpTranscription:
		x.estimate = 1500
	}
	defer s.cleanupUploads(request)
	if e := s.prepareMedia(x, authority); e != nil {
		s.mediaFail(x, w, e)
		return
	}
	if e := s.enforceMediaInput(x, request); e != nil {
		s.mediaFail(x, w, e)
		return
	}
	overall := time.Duration(x.route.OverallTimeout) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	var e *Error
	if x.lease, e = s.Admission.reserveKey(ctx, authority, x.estimate, overall); e != nil {
		s.mediaFail(x, w, e)
		return
	}
	defer func() { settleKey(ctx, x.lease, x.dispatched, x.settledTokens(), s.log) }()

	out := s.executeMedia(ctx, w, x)
	if out.err != nil {
		status := (&streamWriter{w: w, family: x.family, committed: out.committed}).finish(
			&outcome{err: out.err, committed: out.committed, cancelled: out.cancelled})
		s.finishMedia(x, out, status)
		return
	}
	if out.result.Kind != media.ResponseSSE {
		s.writeMediaResult(w, x, out)
	}
	s.finishMedia(x, out, out.status)
}

func (s *Server) finishMedia(x *execution, out *mediaOutcome, status int) {
	s.finish(x, &outcome{err: out.err, committed: out.committed, cancelled: out.cancelled}, status)
}

// cleanupUploads removes the staged upload artifacts a media request owns.
func (s *Server) cleanupUploads(request *media.Request) {
	if request == nil {
		return
	}
	for _, handle := range request.Uploads() {
		if err := s.Media.Jobs.Transport.Spool.Remove(handle); err != nil {
			s.log.Warn("media upload cleanup failed", "error", err)
		}
	}
}

// prepareMedia resolves the route, the caller's route permission, and the
// eligible attempt set for one media operation.
func (s *Server) prepareMedia(x *execution, authority access.Authority) *Error {
	request := x.media
	x.mode = request.Mode()
	snapshot := x.request.release.Snapshot
	route, ok := snapshot.Routes[request.Route]
	if !ok {
		return modelNotFound(request.Route)
	}
	x.route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		return permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
	}
	if request.Op == media.OpVideoCreate && !videoLifecycleRoute(route.Operations) {
		return invalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow video lifecycle operations.", nil)
	}
	var semantic error
	// Candidate defaults are part of the actual provider request. Inspect each
	// encoded call for the metadata check so require_parameters cannot overlook
	// a default or an explicit native null.
	candidates := make(map[string][]string, len(route.Targets))
	plan, err := runtime.PlanRequest(snapshot, route.Slug, request.Op, "openai", x.mode, x.affinity, runtime.SelectionOptions{
		KeyID: x.keyID, Preferences: x.preferences, Parameters: mediaParameterNames(request), Inputs: s.routingInputs(), Now: s.now(),
		CheckSlots: true, CredentialRevoked: s.Runtime.Revoked,
		Accept: func(p runtime.Provider, t runtime.Target) error {
			if !connectorsSupports(p, request.Op, x.mode) {
				return errors.New("connector capability unavailable")
			}
			if request.Op == media.OpVideoCreate && !videoLifecycleProvider(&p, t.ProviderModel) {
				return errors.New("video lifecycle capabilities unavailable")
			}
			call, effective, e := media.EncodeConfigured(request, p.Connector(), t.ProviderModel)
			if e != nil {
				semantic = errors.New(e.Message)
				return semantic
			}
			if p.ProfileID != "" {
				parameters, err := mediaOutboundParameterNames(call, effective)
				if err != nil {
					semantic = err
					return err
				}
				candidates[t.ID] = parameters
			}
			return nil
		},
		Effective: func(p runtime.Provider, t runtime.Target) ([]string, *runtime.TokenDemand) {
			if p.ProfileID == "" {
				// Legacy codecs have their historical null/default wire behavior;
				// only explicit profiles define an exact effective native source.
				return mediaParameterNames(request), nil
			}
			parameters, ok := candidates[t.ID]
			if !ok {
				return nil, nil
			}
			return parameters, nil
		}})
	if err != nil {
		var se *runtime.SelectionError
		if errors.As(err, &se) && se.Code != runtime.NoEligibleTargets && se.Code != "attempt_budget_increase_forbidden" {
			return selectionError(err, route.Slug)
		}
		return requestError(err)
	}
	x.decisions = plan.Decisions
	x.policy = plan.Policy
	x.attempts = plan.Attempts
	x.budget = plan.Budget
	if len(plan.Attempts) == 0 {
		if semantic != nil {
			return requestError(semantic)
		}
		return selectionError(&runtime.SelectionError{Code: runtime.NoEligibleTargets}, route.Slug)
	}
	return nil
}

// mediaOutboundParameterNames inspects the same encoded body/field list sent
// upstream, including native null and absent-only defaults. Route identity and
// delivery controls are not model capability parameters.
func mediaOutboundParameterNames(call *media.UpstreamCall, request *media.Request) ([]string, error) {
	if call == nil {
		return mediaParameterNames(request), nil
	}
	if call.Native != "" {
		// Cloud image envelopes use qualified wrapper fields; the caller's
		// effective controls, not instances/parameters, are capabilities.
		return mediaParameterNames(request), nil
	}
	names := map[string]struct{}{}
	add := func(name string) {
		name = strings.TrimSuffix(name, "[]")
		if name == "" || name == "model" || name == "stream" || name == "stream_format" || strings.HasPrefix(name, "__olp_") {
			return
		}
		names[name] = struct{}{}
	}
	if call.JSON != nil {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(call.JSON, &fields); err != nil || fields == nil {
			return nil, errors.New("configured media body is not a JSON object")
		}
		for name := range fields {
			add(name)
		}
	} else if len(call.Fields) != 0 {
		for _, field := range call.Fields {
			if field.File != nil && field.Name != "mask" && field.Name != "input_reference" {
				continue
			}
			add(field.Name)
		}
	} else {
		return mediaParameterNames(request), nil
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

// connectorsSupports mirrors the connector capability check against the
// matrix in connectors.
func connectorsSupports(p runtime.Provider, op, mode string) bool {
	return p.Connector().Supports(op, "openai", mode)
}

func videoLifecycleRoute(operations []string) bool {
	for _, op := range []string{media.OpVideoGet, media.OpVideoContent, media.OpVideoDelete} {
		if !slices.Contains(operations, op) {
			return false
		}
	}
	return true
}

func videoLifecycleProvider(p *runtime.Provider, model string) bool {
	for _, op := range []string{media.OpVideoGet, media.OpVideoContent, media.OpVideoDelete} {
		if !p.Supports(model, op, "openai", "unary") {
			return false
		}
	}
	return true
}

// mediaOutcome is the terminal result of a media attempt loop. localJob is
// the durable identity a video create hands back instead of the upstream id.
type mediaOutcome struct {
	result    *media.Result
	err       *Error
	committed bool
	cancelled bool
	status    int
	localJob  string
}

// executeMedia adapts media to shared attempt execution. Media transport
// retains its absolute attempt deadline and side-effect ambiguity rules.
func (s *Server) executeMedia(ctx context.Context, w http.ResponseWriter, x *execution) *mediaOutcome {
	attempted := runAttempts(ctx, s, x, attemptAdapter[*media.Result]{
		estimate: func(*runtime.Provider) int64 { return x.estimate },
		dispatch: func(ctx context.Context, attempt runtime.Attempt, provider *runtime.Provider, slot runtime.Slot, ordinal int) (AttemptFact, *media.Result, *attemptFailure) {
			return s.mediaAttempt(ctx, w, x, attempt, provider, slot, ordinal)
		},
	})
	out := &mediaOutcome{
		result:    attempted.result,
		err:       attempted.err,
		committed: attempted.committed,
		cancelled: attempted.cancelled,
	}
	if out.err == nil {
		out.committed = true
		out.status = http.StatusOK
	}
	return out
}

// mediaParameterNames reports the canonical control names a media request
// actually supplies, plus its semantic extensions, for strict
// require_parameters filtering. Route and delivery fields — the model slug,
// stream flags, upload payloads, job identities — and internal cleanup
// markers never become routing requirements.
func mediaParameterNames(r *media.Request) []string {
	supplied := map[string]struct{}{}
	add := func(name string, present bool) {
		if present {
			supplied[name] = struct{}{}
		}
	}
	add("prompt", r.Prompt != "" || r.TextPrompt != nil)
	add("input", r.Input != "")
	add("voice", r.Voice != "")
	add("n", r.Count != nil)
	add("size", r.Size != nil)
	add("response_format", r.Format != nil)
	add("quality", r.Quality != nil)
	add("style", r.Style != nil)
	add("user", r.User != nil)
	add("background", r.Background != nil)
	add("moderation", r.Moderation != nil)
	add("input_fidelity", r.InputFidelity != nil)
	add("output_compression", r.OutputCompression != nil)
	add("output_format", r.OutputFormat != nil)
	add("partial_images", r.PartialImages != nil)
	add("language", r.Language != nil)
	add("temperature", r.Temperature != nil)
	add("speed", r.Speed != nil)
	add("instructions", r.Instructions != nil)
	add("include", len(r.Include) > 0)
	add("timestamp_granularities", len(r.TimestampGranularities) > 0)
	add("chunking_strategy", len(r.ChunkingStrategy) > 0)
	add("known_speaker_names", len(r.KnownSpeakerNames) > 0)
	add("known_speaker_references", len(r.KnownSpeakerReferences) > 0)
	add("seconds", r.Seconds != nil)
	add("mask", r.Mask != nil)
	add("input_reference", r.InputRef != nil)
	for name := range r.Extra {
		if strings.HasPrefix(name, "__olp_") {
			continue
		}
		supplied[strings.TrimSuffix(name, "[]")] = struct{}{}
	}
	names := make([]string, 0, len(supplied))
	for name := range supplied {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// mediaAttempt performs one upstream media call with one credential.
func (s *Server) mediaAttempt(ctx context.Context, w http.ResponseWriter, x *execution, a runtime.Attempt, provider *runtime.Provider, slot runtime.Slot, ordinal int) (AttemptFact, *media.Result, *attemptFailure) {
	fact := s.newFact(x, a, slot, ordinal)
	attemptCtx, atr := x.request.trace.Attempt(ctx, provider.Kind, a.ProviderRevisionID, a.UpstreamModel)
	finishTrace := func() {
		if atr == nil {
			return
		}
		if u := fact.Usage; u != nil {
			atr.RecordUsage(&u.InputTokens, &u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
		}
		atr.Finish(fact.Class, fact.Status)
	}
	fail := func(class string, f *attemptFailure) (AttemptFact, *media.Result, *attemptFailure) {
		if f == nil {
			f = &attemptFailure{}
		}
		f.class = class
		fact.Class = class
		fact.Committed = f.committed
		fact.Duration = s.now().Sub(fact.StartedAt)
		if f.retryAfter > 0 {
			retry := f.retryAfter
			fact.RetryAfter = &retry
		}
		fact.recordEvidence(f.billingUncertain())
		finishTrace()
		return fact, nil, f
	}
	cfg := provider.Connector()
	call, effective, mErr := media.EncodeConfigured(x.media, cfg, a.UpstreamModel)
	if mErr != nil {
		return fail(classProtocol, nil)
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fail(classTimeout, &attemptFailure{overall: true})
	}
	timeout := min(a.Timeout, remaining)
	actx, cancel := context.WithTimeout(attemptCtx, timeout)
	defer cancel()

	var secret []byte
	if slot.CredentialID != nil {
		secret, _ = x.request.release.Credential(*slot.CredentialID)
	}
	if x.request.trace.PropagateUpstream() {
		call.Inject = http.Header{}
		atr.InjectUpstream(call.Inject, true)
	}
	networkSecret, err := s.providerNetworkSecret(actx, x.request.release, provider)
	if err != nil {
		return fail(classCredential, nil)
	}
	target := media.Target{Config: cfg, Model: cfg.Model(a.UpstreamModel), Secret: secret, NetworkSecret: networkSecret, ConnectionScope: providerConnectionScope(provider, slot)}
	result, failure := s.Media.Jobs.Transport.Do(actx, target, call, effective)
	if failure != nil {
		fact.Status = failure.Status
		return fail(mediaClass(failure), &attemptFailure{
			status:     failure.Status,
			retryAfter: failure.RetryAfter,
			upstream:   failure.Upstream,
			dispatched: failure.Dispatched,
		})
	}
	fact.Status = result.Status
	fb := result.FirstByte
	fact.FirstByte = &fb
	fact.Class = "success"
	fact.Usage = mediaUsage(effective, result)
	fact.Committed = true
	if result.Kind == media.ResponseSSE {
		var streamFailure *attemptFailure
		fact.Usage, fact.Committed, streamFailure = s.streamMediaEvents(actx, w, x, result)
		if streamFailure != nil {
			if call.Ambiguous && !streamFailure.committed && streamFailure.class == classTimeout {
				streamFailure.class = classAmbiguous
			}
			return fail(streamFailure.class, streamFailure)
		}
	}
	fact.Duration = s.now().Sub(fact.StartedAt)
	fact.recordEvidence(true)
	finishTrace()
	return fact, result, nil
}

// mediaClass maps a media transport failure onto the attempt taxonomy. An
// ambiguous side-effecting failure never falls over.
func mediaClass(f *media.Failure) string {
	if f.Ambiguous {
		return classAmbiguous
	}
	switch f.Class {
	case media.ClassTimeout:
		return classTimeout
	case media.ClassRateLimit:
		return classRateLimit
	case media.ClassUpstreamServer:
		return classUpstreamServer
	case media.ClassUpstreamClient:
		return classUpstreamClient
	case media.ClassCredential:
		return classCredential
	case media.ClassProtocol:
		return classProtocol
	case media.ClassCancelled:
		return classCancelled
	}
	return classConnect
}

// mediaUsage builds the accounting usage a media result carries.
func mediaUsage(request *media.Request, result *media.Result) *openai.Usage {
	switch result.Kind {
	case media.ResponseImages:
		if result.Images == nil {
			return nil
		}
		usage := &openai.Usage{}
		if result.Images.Usage != nil {
			usage.InputTokens = result.Images.Usage.InputTokens
			usage.OutputTokens = result.Images.Usage.OutputTokens
			usage.TotalTokens = result.Images.Usage.TotalTokens
		}
		units := strconv.Itoa(len(result.Images.Images))
		usage.MediaUnits = &units
		return usage
	case media.ResponseTranscription:
		if result.Transcription == nil || result.Transcription.DurationSeconds == nil {
			return nil
		}
		units := strconv.FormatFloat(*result.Transcription.DurationSeconds, 'f', -1, 64)
		return &openai.Usage{MediaUnits: &units}
	case media.ResponseVideoJob:
		if request == nil || request.Op != media.OpVideoCreate || result.Video == nil || result.Video.Seconds == nil {
			return nil
		}
		units := *result.Video.Seconds
		return &openai.Usage{MediaUnits: &units}
	}
	return nil
}

// writeMediaResult renders the successful media response.
func (s *Server) writeMediaResult(w http.ResponseWriter, x *execution, out *mediaOutcome) {
	result := out.result
	switch result.Kind {
	case media.ResponseImages:
		s.writeImageResult(w, x, out)
	case media.ResponseBinary, media.ResponseVideoContent:
		s.streamArtifact(w, x, out)
	case media.ResponseTranscription:
		out.committed = true
		if result.Text != nil {
			format := "text/plain; charset=utf-8"
			if x.media.Format != nil {
				switch *x.media.Format {
				case "srt":
					format = "application/x-subrip; charset=utf-8"
				case "vtt":
					format = "text/vtt; charset=utf-8"
				}
			}
			s.deliverMedia(w, x, out, format, func() error {
				_, err := w.Write(result.Text)
				return err
			})
			return
		}
		body, failure := media.EncodeTranscriptionJSON(result.Transcription)
		if failure != nil {
			out.err = serverError(http.StatusBadGateway, failure.Code, failure.Message)
			out.status, out.committed = out.err.Status, false
			writeSurfaceError(w, out.err, "openai")
			return
		}
		s.deliverMediaJSON(w, x, out, body)
	case media.ResponseVideoJob:
		out.committed = true
		// Video create responses render the local identity.
		body, failure := media.EncodeVideoObject(result.Video, outJobID(out), x.media.Route)
		if failure != nil {
			out.err = serverError(http.StatusBadGateway, failure.Code, failure.Message)
			out.status, out.committed = out.err.Status, false
			writeSurfaceError(w, out.err, "openai")
			return
		}
		s.deliverMediaJSON(w, x, out, body)
	}
}

// deliverMedia records success only after the bounded body write and flush.
// Upstream usage remains intact when the client cannot receive the response.
func (s *Server) deliverMedia(w http.ResponseWriter, x *execution, out *mediaOutcome, contentType string, write func() error) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
		out.err = serverError(http.StatusInternalServerError, "internal_error", "The response could not be written.")
		out.status, out.committed = out.err.Status, false
		w.Header().Del("Content-Length")
		writeSurfaceError(w, out.err, "openai")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(out.status)
	out.committed = true
	err := write()
	if err == nil {
		err = rc.Flush()
	}
	if err != nil {
		out.err = (&attemptFailure{class: classCancelled}).toError()
		out.cancelled, out.status = true, 0
		return
	}
	x.delivered(s.now())
}

func (s *Server) deliverMediaJSON(w http.ResponseWriter, x *execution, out *mediaOutcome, body []byte) {
	s.deliverMedia(w, x, out, "application/json", func() error {
		_, err := w.Write(body)
		return err
	})
}

func outJobID(out *mediaOutcome) string {
	if out.localJob != "" {
		return out.localJob
	}
	if out.result.Video != nil {
		return out.result.Video.ID
	}
	return ""
}

// writeImageResult streams the image JSON document: each staged payload is
// base64-streamed in place of its marker so the response never buffers media.
func (s *Server) writeImageResult(w http.ResponseWriter, x *execution, out *mediaOutcome) {
	defer func() {
		for _, image := range out.result.Images.Images {
			if image.Handle != nil {
				s.transport().Spool.Remove(*image.Handle)
			}
		}
	}()
	body, markers, failure := media.EncodeImageResponse(out.result.Images)
	if failure != nil {
		out.err = serverError(http.StatusBadGateway, failure.Code, failure.Message)
		out.status, out.committed = out.err.Status, false
		writeSurfaceError(w, out.err, "openai")
		return
	}
	handles := map[string]media.Handle{}
	index := 0
	for _, image := range out.result.Images.Images {
		if image.Handle != nil && index < len(markers) {
			handles[markers[index]] = *image.Handle
			index++
		}
	}
	s.deliverMedia(w, x, out, "application/json", func() error {
		rest := body
		for _, marker := range markers {
			cut := bytes.Index(rest, []byte(marker))
			if cut < 0 {
				return errors.New("staged media marker missing")
			}
			if _, err := w.Write(rest[:cut]); err != nil {
				return err
			}
			if err := s.streamBase64(w, handles[marker]); err != nil {
				return err
			}
			rest = rest[cut+len(marker):]
		}
		if _, err := w.Write(rest); err != nil {
			return err
		}
		return nil
	})
}

// streamBase64 writes one staged artifact base64-encoded into w.
func (s *Server) streamBase64(w io.Writer, handle media.Handle) error {
	opened, err := s.Media.Jobs.Transport.Spool.Open(handle)
	if err != nil {
		return err
	}
	defer opened.File.Close()
	encoder := base64.NewEncoder(base64.StdEncoding, w)
	_, err = io.Copy(encoder, opened.File)
	if cerr := encoder.Close(); err == nil {
		err = cerr
	}
	return err
}

// streamArtifact streams a staged binary artifact to the client and removes
// it once the body is exhausted or the write fails.
func (s *Server) streamArtifact(w http.ResponseWriter, x *execution, out *mediaOutcome) {
	artifact := out.result.Artifact
	opened, err := s.Media.Jobs.Transport.Spool.Open(artifact.Handle)
	if err != nil {
		s.Media.Jobs.Transport.Spool.Remove(artifact.Handle)
		out.err = serverError(http.StatusBadGateway, "media_content_unavailable", "The staged media could not be read.")
		out.status, out.committed = out.err.Status, false
		writeSurfaceError(w, out.err, "openai")
		return
	}
	defer s.Media.Jobs.Transport.Spool.Remove(artifact.Handle)
	defer opened.File.Close()
	if artifact.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.ContentLength, 10))
	}
	s.deliverMedia(w, x, out, out.result.ContentType, func() error {
		_, err := io.Copy(w, opened.File)
		return err
	})
}

// streamMediaEvents drains a stream inside its attempt's deadline and quota
// reservation. Only a terminal event proves that the response completed.
func (s *Server) streamMediaEvents(ctx context.Context, w http.ResponseWriter, x *execution, result *media.Result) (*openai.Usage, bool, *attemptFailure) {
	defer result.Body.Close()
	sw := &streamWriter{w: w, family: x.family}
	var usage *openai.Usage
	terminal := errors.New("media stream completed")
	decodeErr := sse.Decode(result.Body, int(s.cfg.MaxEventBytes), func(frame sse.Frame) error {
		var payload struct {
			Type  string          `json:"type"`
			Error json.RawMessage `json:"error"`
		}
		event := "done"
		if frame.Data != "[DONE]" {
			if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
				return err
			}
			event = payload.Type
			if frame.Event != nil && *frame.Event != "" {
				event = *frame.Event
			}
		}
		if event == "error" || (len(payload.Error) > 0 && string(payload.Error) != "null") {
			return errors.New("provider media stream failed")
		}
		var buf bytes.Buffer
		if frame.Event != nil && *frame.Event != "" {
			buf.WriteString("event: " + *frame.Event + "\n")
		}
		for line := range strings.SplitSeq(frame.Data, "\n") {
			buf.WriteString("data: " + line + "\n")
		}
		buf.WriteByte('\n')
		if input, output, cached := media.ObserveMediaUsage(frame.Data); input != nil || output != nil || cached != nil {
			if usage == nil {
				usage = &openai.Usage{}
			}
			if input != nil {
				usage.InputTokens = *input
			}
			if output != nil {
				usage.OutputTokens = *output
			}
			if input != nil && output != nil {
				usage.TotalTokens = *input + *output
			}
			if cached != nil {
				usage.CachedInputTokens = cached
			}
		}
		completed := media.IsMediaTerminal(event)
		if completed {
			if units := x.media.SeedMediaUnits(); units != nil {
				if usage == nil {
					usage = &openai.Usage{}
				}
				usage.MediaUnits = units
			}
		}
		if err := sw.emit(buf.Bytes()); err != nil {
			return err
		}
		x.delivered(s.now())
		if completed {
			return terminal
		}
		return nil
	})
	if errors.Is(decodeErr, terminal) {
		return usage, sw.committed, nil
	}
	class := classProtocol
	switch {
	case errors.Is(decodeErr, errClientWrite), errors.Is(ctx.Err(), context.Canceled):
		class = classCancelled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		class = classTimeout
	}
	return usage, sw.committed, &attemptFailure{class: class, committed: sw.committed, dispatched: true}
}

// mediaError maps a media-layer failure onto a gateway error.
func mediaError(failure *media.Error) *Error {
	if failure == nil {
		return nil
	}
	typeName := "invalid_request_error"
	if failure.Status >= 500 {
		typeName = "server_error"
	}
	return &Error{Status: failure.Status, Type: typeName, Code: failure.Code, Message: failure.Message}
}

func mediaErr(err error) *media.Error {
	if failure, ok := errors.AsType[*media.Error](err); ok {
		return failure
	}
	return media.Fail(http.StatusBadRequest, "invalid_request", err.Error())
}

// imageGenerations decodes the JSON image-generation body.
func (s *Server) imageGenerations(w http.ResponseWriter, r *http.Request) {
	s.mediaJSONHandler(media.DecodeImageGeneration)(w, r)
}
