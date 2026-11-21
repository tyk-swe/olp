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
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/mediacontract"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
)

type MediaDeps struct {
	Transport *transport.Transport
	Jobs      *jobs.Service
	Store     *jobs.Store
}

const multipartParseDeadline = 60 * time.Second

const mediaMultipartTokens = 2000

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

func (s *Server) BeginMedia(w http.ResponseWriter, r *http.Request) (*Call, keyauthority.Authority, bool) {
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
	s.BindAuthority(x, authority)
	if x.attribution, e = ingress.ParseAttribution(r, authority); e != nil {
		s.Gate.Release(r.Context())
		s.Refuse(x, w, e)
		return x, keyauthority.Authority{}, true
	}
	return x, authority, false
}

func (x *Call) UnaryFamily(family wire.Format) {
	x.family = family
	x.mode = "unary"
}

func (x *Call) NoteDecodedMedia(request *media.Request, sized *int64) {
	x.family = wire.Format(request.Op())
	if sized != nil {
		x.estimate, x.sizedInput = *sized+1, sized
	}
}

func (x *Call) MarkDispatched() { x.dispatched = true }

func (x *Call) NoteDispatchFailure(failure *AttemptFailure) {
	x.dispatched = failure == nil || failure.dispatched
}

func MediaResultOutcome(result *transport.Result, status int) *MediaOutcome {
	return &MediaOutcome{result: result, committed: true, status: status}
}

func MediaStatusOutcome(status int) *MediaOutcome {
	return &MediaOutcome{committed: true, status: status}
}

func (o *MediaOutcome) Status() int { return o.status }

func (s *Server) ServeMedia(ctx context.Context, w http.ResponseWriter, x *Call, authority keyauthority.Authority, request *media.Request) {
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
	defer s.SettleAdmission(ctx, x)

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

func (s *Server) finishMedia(x *Call, out *MediaOutcome, status int) {
	s.Finish(x, &Outcome{Err: out.err, Committed: out.committed, Cancelled: out.cancelled}, status)
}

func (s *Server) FinishMedia(x *Call, out *MediaOutcome, status int) {
	s.finishMedia(x, out, status)
}

func (s *Server) FailMediaOutcome(x *Call, w http.ResponseWriter, failure *AttemptFailure) {
	e := failure.Fault()
	s.finishMedia(x, &MediaOutcome{err: e, committed: failure.committed}, e.Status)
	fault.WriteSurfaceError(w, e, "openai")
}

func (s *Server) cleanupUploads(request *media.Request) {
	if request == nil {
		return
	}
	for _, handle := range request.Uploads() {
		if err := s.Deps.Staging.Remove(handle); err != nil {
			s.Log.Warn("media upload cleanup failed", "error", err)
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
	if e := s.CheckRouteFidelity(ctx, x.Route); e != nil {
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

type MediaOutcome struct {
	result    *transport.Result
	err       *fault.Error
	committed bool
	cancelled bool
	status    int
	localJob  string
}

func (s *Server) executeMedia(ctx context.Context, w http.ResponseWriter, x *Call) *MediaOutcome {
	attempted := runAttempts(ctx, s, x, attemptAdapter[*transport.Result]{
		demand: func(routing.Attempt, *routing.Provider) quota.TokenDemand { return quota.CombinedDemand(x.estimate) },
		dispatch: func(ctx context.Context, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, *transport.Result, *AttemptFailure) {
			return s.mediaAttempt(ctx, w, x, attempt, provider, slot, fact)
		},
	})
	out := &MediaOutcome{
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

func mediaClass(f *transport.Failure) string {
	if f.Ambiguous {
		return ClassAmbiguous
	}
	return string(f.Class)
}

func MediaError(failure *media.Error) *fault.Error {
	if failure == nil {
		return nil
	}
	typeName := "invalid_request_error"
	if failure.Status >= 500 {
		typeName = "server_error"
	}
	return &fault.Error{Status: failure.Status, Type: typeName, Code: failure.Code, Message: failure.Message}
}
