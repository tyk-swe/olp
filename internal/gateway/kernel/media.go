package kernel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/mediacontract"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

type MediaDeps struct {
	Transport *transport.Transport
	Jobs      *jobs.Service
	Store     *jobs.Store
}

const multipartParseDeadline = 60 * time.Second

const mediaMultipartTokens = 2000

func (s *Server) registerMedia(mux *http.ServeMux) {
	if s.Deps.Media == nil {
		return
	}
	mux.HandleFunc("POST /v1/images/generations", s.mediaJSONHandler(media.DecodeImageGeneration))
	mux.HandleFunc("POST /v1/images/edits", s.mediaFormHandler(media.DefaultImageUploadLimit, 33, media.DecodeImageEdit))
	mux.HandleFunc("POST /v1/images/variations", s.mediaFormHandler(media.DefaultImageUploadLimit, 1, media.DecodeImageVariation))
	mux.HandleFunc("POST /v1/audio/speech", s.mediaJSONHandler(media.DecodeSpeech))
	mux.HandleFunc("POST /v1/audio/transcriptions", s.mediaFormHandler(media.DefaultAudioUploadLimit, 1, media.DecodeTranscription))
	mux.HandleFunc("POST /v1/audio/translations", s.mediaFormHandler(media.DefaultAudioUploadLimit, 1, media.DecodeTranslation))
	mux.HandleFunc("POST /v1/videos", s.videoCreate)
	mux.HandleFunc("GET /v1/videos", s.videoList)
	mux.HandleFunc("GET /v1/videos/{video_id}", s.videoGet)
	mux.HandleFunc("GET /v1/videos/{video_id}/content", s.videoContent)
	mux.HandleFunc("DELETE /v1/videos/{video_id}", s.videoDelete)
}

func (s *Server) mediaJSONHandler(decode func([]byte) (*media.Request, *media.Error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		x, authority, done := s.mediaBegin(w, r)
		if done {
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(requestBodyTimeout)); err != nil {
			s.Gate.Release(r.Context())
			s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The request could not be read."))
			return
		}
		body, e := s.Gate.ReadBody(r)
		if e != nil {
			s.Gate.Release(r.Context())
			s.Refuse(x, w, e)
			return
		}
		rc.SetReadDeadline(time.Time{})
		request, failure := decode(body)
		if failure != nil {
			s.Gate.Release(r.Context())
			s.Refuse(x, w, mediaError(failure))
			return
		}
		x.family = wire.Format(request.Op())
		sized := estimate.HeuristicBytesTokens(len(body))
		x.estimate, x.sizedInput = sized+1, &sized
		s.serveMedia(r.Context(), w, x, authority, request)
	}
}

func (s *Server) mediaFormHandler(maxFileBytes int64, maxFiles int, decode func(*staging.Form) (*media.Request, *media.Error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		x, authority, done := s.mediaBegin(w, r)
		if done {
			return
		}
		form, e := s.ParseMediaForm(w, r, authority.ID, maxFileBytes, maxFiles)
		if e != nil {
			s.Gate.Release(r.Context())
			s.Refuse(x, w, e)
			return
		}
		request, failure := decode(form)
		if failure != nil {
			form.Cleanup()
			s.Gate.Release(r.Context())
			s.Refuse(x, w, mediaError(failure))
			return
		}
		form.Disarm()
		x.family = wire.Format(request.Op())
		s.serveMedia(r.Context(), w, x, authority, request)
	}
}

func (s *Server) ParseMediaForm(w http.ResponseWriter, r *http.Request, keyID string, maxFileBytes int64, maxFiles int) (*staging.Form, *fault.Error) {
	limit := s.Config.MaxMediaBodyBytes
	if r.ContentLength > limit {
		return nil, ingress.BodyTooLarge()
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(multipartParseDeadline)); err != nil {
		return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The request could not be read.")
	}
	defer rc.SetReadDeadline(time.Time{})
	lease := s.Deps.Uploads.TryAdmit(keyID, min(limit, maxFileBytes*int64(maxFiles)))
	form, failure := staging.ParseMultipart(r.Context(), r, s.Deps.Staging, lease, maxFileBytes, maxFiles)
	if failure != nil {
		return nil, fault.StagingError(failure)
	}
	return form, nil
}

func (s *Server) mediaBegin(w http.ResponseWriter, r *http.Request) (*Call, keyauthority.Authority, bool) {
	x := &Call{request: s.begin(w, r), family: wire.Chat, actor: "api_key"}
	x.semanticHeaders = semanticHeaders(r.Header)
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	x.semanticQuery, x.semanticQueryInvalid = query, queryErr != nil
	if !s.Gate.AdmitLater(r.Context()) {
		s.Refuse(x, w, fault.Overloaded)
		return x, keyauthority.Authority{}, true
	}
	authority, e := s.Gate.Authenticate(r, "inference")
	if e != nil {
		s.Gate.Release(r.Context())
		s.Refuse(x, w, e)
		return x, keyauthority.Authority{}, true
	}
	if x.preferences, x.priority, e = routingControls(r, authority.Policy); e != nil {
		s.Gate.Release(r.Context())
		s.Refuse(x, w, e)
		return x, keyauthority.Authority{}, true
	}
	if e = s.Gate.AwaitAdmission(r.Context(), x.priority); e != nil {
		s.Gate.Release(r.Context())
		s.Refuse(x, w, e)
		return x, keyauthority.Authority{}, true
	}
	s.bindAuthority(x, authority)
	if x.attribution, e = ingress.ParseAttribution(r, authority); e != nil {
		s.Gate.Release(r.Context())
		s.Refuse(x, w, e)
		return x, keyauthority.Authority{}, true
	}
	return x, authority, false
}

func (s *Server) serveMedia(ctx context.Context, w http.ResponseWriter, x *Call, authority keyauthority.Authority, request *media.Request) {
	defer s.Gate.Release(ctx)
	x.media = request
	switch request.Op() {
	case media.OpImageEdit, media.OpImageVariation:
		x.estimate = mediaMultipartTokens
	case media.OpTranscription, media.OpTranslation:
		x.estimate = 1500
	}
	defer s.cleanupUploads(request)
	if e := s.prepareMedia(ctx, x, authority); e != nil {
		s.Refuse(x, w, e)
		return
	}
	overall := time.Duration(x.primary.OverallTimeout) * time.Millisecond
	ctx, cancel := x.routeContext(ctx)
	defer cancel()
	var e *fault.Error
	if x.lease, e = s.Deps.Admission.ReserveKey(ctx, x.AdmissionAuthority(authority), x.clientSurface(), x.estimate, overall, x.limitRoute()); e != nil {
		s.Refuse(x, w, e)
		return
	}
	defer s.settleAdmission(ctx, x)

	out := s.executeMedia(ctx, w, x)
	if out.err != nil {
		status := (&fault.StreamWriter{W: w, Family: x.family, Committed: out.committed}).Finish(out.err, out.cancelled)
		s.finishMedia(x, out, status)
		return
	}
	if _, events := out.result.Payload.(transport.Events); !events {
		s.writeMediaResult(w, x, out)
	}
	s.finishMedia(x, out, out.status)
}

func (s *Server) finishMedia(x *Call, out *mediaOutcome, status int) {
	s.Finish(x, &Outcome{Err: out.err, Committed: out.committed, Cancelled: out.cancelled}, status)
}

func (s *Server) cleanupUploads(request *media.Request) {
	if request == nil {
		return
	}
	for _, handle := range request.Uploads() {
		if err := s.Deps.Staging.Remove(handle); err != nil {
			s.log.Warn("media upload cleanup failed", "error", err)
		}
	}
}

func (s *Server) prepareMedia(ctx context.Context, x *Call, authority keyauthority.Authority) *fault.Error {
	request := x.media
	x.mode = request.Mode()
	route, ok := x.Snapshot().FindRoute(request.Route())
	if !ok {
		return fault.ModelNotFound(request.Route())
	}
	x.Route = &route
	authorize := s.keyAuthorizer(authority)
	if e := authorize(&route); e != nil {
		return e
	}
	x.bind(mediaPlanner, authorize, request.Op() == media.OpVideoCreate)
	return s.planNamed(ctx, x)
}

func (s *Server) planMedia(ctx context.Context, x *Call) *fault.Error {
	if x.Snapshot().GuardrailEngine(x.Route.Slug, x.keyID) != nil {
		if e := x.GuardrailSurfaceGate(x.Route); e != nil {
			return e
		}
	}
	if e := s.checkRouteFidelity(ctx, x.Route); e != nil {
		return e
	}
	request := x.media
	snapshot := x.Snapshot()
	route := *x.Route
	if route.Fidelity.Strict() {
		if e := strictMediaContext(x); e != nil {
			return e
		}
	}
	video := request.Op() == media.OpVideoCreate
	if video && !videoLifecycle(func(op string) bool { return slices.Contains(route.Operations, op) }) {
		return fault.InvalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow video lifecycle operations.", nil)
	}
	session := s.planning(x, mediaPlanning, x.Route, string(request.Op()), "openai", x.mode)
	options := s.selectionOptions(ctx, x)
	options.Parameters = routing.Listed(request.ParameterNames())
	options.Prepare = func(p routing.Provider, t routing.Target) (*routing.CandidatePreparation, error) {
		if !p.Connector().Supports(string(request.Op()), "openai", x.mode) {
			return nil, errors.New("connector capability unavailable")
		}
		if video && !videoLifecycle(func(op string) bool { return p.Supports(t.ProviderModel, op, "openai", "unary") }) {
			return nil, errors.New("video lifecycle capabilities unavailable")
		}
		encoded, err := x.encodeMedia(snapshot, p, t.ID, t.ProviderModel)
		if err != nil {
			session.semantic = err
			return nil, err
		}
		if p.ProfileID == "" {
			return &routing.CandidatePreparation{Parameters: routing.Listed(request.ParameterNames())}, nil
		}
		parameters, err := media.OutboundParameterNames(encoded.call, encoded.effective)
		if err != nil {
			session.semantic = err
			return nil, err
		}
		return &routing.CandidatePreparation{Parameters: routing.Listed(parameters)}, nil
	}
	_, _, failure := session.selectPlan(ctx, options)
	return failure
}

func strictMediaContext(x *Call) *fault.Error {
	if x.semanticQueryInvalid || len(x.semanticQuery) != 0 {
		return fault.InvalidRequest("target_capability", "This native media route does not qualify caller query settings.", nil)
	}
	return strictMediaHeaders(x)
}

func strictMediaHeaders(x *Call) *fault.Error {
	for name := range x.semanticHeaders {
		canonical := http.CanonicalHeaderKey(name)
		lower := strings.ToLower(canonical)
		if canonical == "Idempotency-Key" || canonical == "X-Idempotency-Key" || canonical == "X-Olp-Client-Contract" || canonical == "X-Olp-Route" ||
			strings.HasPrefix(lower, "openai-") || strings.HasPrefix(lower, "x-openai-") || strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "x-goog-") || strings.HasPrefix(lower, "x-amzn-") || canonical == "Api-Version" {
			param := "/headers/" + canonical
			return fault.InvalidRequest("target_capability", "This native media route does not qualify the caller's semantic header.", &param)
		}
	}
	return nil
}

type encodedMedia struct {
	call      *media.UpstreamCall
	effective *media.Request
	template  *mediacontract.Template
	bound     mediacontract.Bound
}

func (x *Call) encodeMedia(snapshot *routing.View, provider routing.Provider, targetID, model string) (encodedMedia, error) {
	if !x.Strict() {
		call, effective, failure := media.EncodeConfigured(x.media, provider.Connector(), model)
		if failure != nil {
			return encodedMedia{}, errors.New(failure.Message)
		}
		return encodedMedia{call: call, effective: effective}, nil
	}
	call, effective, failure := media.EncodeStrictConfigured(x.media, provider.Connector(), model)
	if failure != nil {
		return encodedMedia{}, errors.New(failure.Message)
	}
	template, ok := snapshot.MediaTemplate(x.Route.Slug, targetID, string(x.media.Op()))
	if !ok {
		return encodedMedia{}, errors.New("compiled strict media contract unavailable")
	}
	input, err := media.ContractInput(x.media, call, effective, nil)
	if err != nil {
		return encodedMedia{}, err
	}
	bound, err := template.Bind(input)
	if err != nil {
		return encodedMedia{}, err
	}
	return encodedMedia{call: call, effective: effective, template: template, bound: bound}, nil
}

func videoLifecycle(supported func(op string) bool) bool {
	return supported(string(media.OpVideoGet)) && supported(string(media.OpVideoContent)) && supported(string(media.OpVideoDelete))
}

type mediaOutcome struct {
	result    *transport.Result
	err       *fault.Error
	committed bool
	cancelled bool
	status    int
	localJob  string
}

func (s *Server) executeMedia(ctx context.Context, w http.ResponseWriter, x *Call) *mediaOutcome {
	attempted := runAttempts(ctx, s, x, attemptAdapter[*transport.Result]{
		estimate: func(routing.Attempt, *routing.Provider) int64 { return x.estimate },
		dispatch: func(ctx context.Context, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, *transport.Result, *attemptFailure) {
			return s.mediaAttempt(ctx, w, x, attempt, provider, slot, fact)
		},
	})
	out := &mediaOutcome{
		result:    attempted.result,
		err:       attempted.err,
		committed: attempted.committed,
		cancelled: attempted.cancelled,
	}
	if out.err == nil {
		out.status = http.StatusOK
	}
	return out
}

func (s *Server) mediaAttempt(ctx context.Context, w http.ResponseWriter, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, *transport.Result, *attemptFailure) {
	attemptCtx, atr := x.request.trace.Attempt(ctx, string(provider.Kind), a.ProviderRevisionID, a.UpstreamModel)
	finishTrace := func() {
		if atr == nil {
			return
		}
		if u := fact.Usage; u != nil {
			atr.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
		}
		atr.Finish(fact.Class, fact.Status)
	}
	fail := func(class string, f *attemptFailure) (events.AttemptFact, *transport.Result, *attemptFailure) {
		if f == nil {
			f = &attemptFailure{}
		}
		f.class = class
		fact.Class = class
		fact.Committed = f.committed
		f.accepted = fact.Status == http.StatusOK
		f.acceptance = upstream.Evidence{Reached: f.dispatched, Status: f.Status, Accepted: f.accepted}.Acceptance()
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = f.acceptance
			if f.committed {
				fact.Interaction.ClientState = usage.ClientPartial
			}
		}
		fact.Duration = s.Now().Sub(fact.StartedAt)
		if f.retryAfter > 0 {
			retry := f.retryAfter
			fact.RetryAfter = &retry
		}
		fact.RecordEvidence(f.billingUncertain())
		finishTrace()
		return fact, nil, f
	}
	encoded, err := x.encodeMedia(x.request.release.Snapshot(), *provider, a.TargetID, a.UpstreamModel)
	if err != nil {
		return fail(classProtocol, nil)
	}
	call, effective, strictTemplate, bound := encoded.call, encoded.effective, encoded.template, encoded.bound
	cfg := provider.Connector()
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fail(classTimeout, &attemptFailure{overall: true})
	}
	timeout := min(a.Timeout, remaining)
	actx, cancel := context.WithTimeout(attemptCtx, timeout)
	defer cancel()

	secret, err := s.slotSecret(actx, x, slot, provider, a.UpstreamModel)
	if err != nil {
		return fail(classCredential, nil)
	}
	headers := http.Header{}
	atr.InjectUpstream(headers)
	networkSecret, err := s.providerNetworkSecret(actx, x.request.release, provider)
	if err != nil {
		return fail(classCredential, nil)
	}
	target := transport.Target{Sensitive: &x.sensitive, CallerCredential: provider.CredentialSource == "caller" && x.origin != "probe", Config: cfg, Model: cfg.Model(a.UpstreamModel), Secret: secret, NetworkSecret: networkSecret, ConnectionScope: providerConnectionScope(provider, slot)}
	result, failure := s.Deps.Media.Transport.Do(actx, target, call, transport.Options{Strict: strictTemplate != nil, Headers: headers})
	if failure != nil {
		fact.Status = failure.Status
		return fail(mediaClass(failure), &attemptFailure{
			Status:     failure.Status,
			retryAfter: failure.RetryAfter,
			upstream:   failure.Upstream,
			dispatched: failure.Dispatched,
		})
	}
	fact.Status = result.Status
	if artifact := mediaArtifact(result.Payload); strictTemplate != nil && artifact != nil {
		if _, err := artifact.BlobReference(); err != nil {
			return fail(classProtocol, &attemptFailure{dispatched: true})
		}
	}
	if strictTemplate != nil && result.Source.Valid() {
		if _, err := strictTemplate.JSONResult(bound.Descriptor, result.Source); err != nil {
			if images, ok := result.Payload.(transport.Images); ok {
				s.removeImages(images.ImageResult)
			}
			class := classProtocol
			if call.Ambiguous {
				class = classAmbiguous
			}
			return fail(class, &attemptFailure{dispatched: true})
		}
	}
	fb := result.FirstByte
	fact.FirstByte = &fb
	fact.Class = "success"
	fact.Usage = mediaUsage(effective, result)
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = upstream.Terminal
	}
	if events, ok := result.Payload.(transport.Events); ok {
		var streamFailure *attemptFailure
		fact.Usage, fact.Committed, streamFailure = s.streamMediaEvents(actx, w, x, events.Body, strictTemplate, bound)
		if streamFailure != nil {
			if call.Ambiguous && !streamFailure.committed && streamFailure.class == classTimeout {
				streamFailure.class = classAmbiguous
			}
			return fail(streamFailure.class, streamFailure)
		}
		if fact.Interaction != nil {
			fact.Interaction.ClientState = usage.ClientTerminal
		}
	}
	fact.Duration = s.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(true)
	finishTrace()
	return fact, result, nil
}

func mediaClass(f *transport.Failure) string {
	if f.Ambiguous {
		return classAmbiguous
	}
	return string(f.Class)
}

func mediaError(failure *media.Error) *fault.Error {
	if failure == nil {
		return nil
	}
	typeName := "invalid_request_error"
	if failure.Status >= 500 {
		typeName = "server_error"
	}
	return &fault.Error{Status: failure.Status, Type: typeName, Code: failure.Code, Message: failure.Message}
}
