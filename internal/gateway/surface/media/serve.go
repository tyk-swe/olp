package media

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	mediacontract "github.com/tyk-swe/olp/internal/contracts/media"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/routing"
)

const mediaMultipartTokens = 2000

type encodedMedia struct {
	call      *media.UpstreamCall
	effective *media.Request
	template  *mediacontract.Template
	bound     mediacontract.Bound
}

func configureMediaCall(x *kernel.Call, request *media.Request, sized *int64) {
	x.SetFamily(wire.Format(request.Op()))
	x.SetMode(request.Mode())
	if sized != nil {
		x.SetEstimate(*sized+1, sized)
	}
	switch request.Op() {
	case media.OpImageEdit, media.OpImageVariation, media.OpVideoCreate:
		x.SetEstimate(mediaMultipartTokens, nil)
	case media.OpTranscription, media.OpTranslation:
		x.SetEstimate(1500, nil)
	}
	x.SetFixedUsage(videoCreateUsage(request))
	x.SetStrictServing(func(targetID string) (oif.ServingIdentity, bool) {
		template, found := x.Snapshot().MediaTemplate(x.Route.Slug, targetID, string(request.Op()))
		if !found {
			return oif.ServingIdentity{}, false
		}
		return template.Serving(), true
	})
}

func (s surface) serveMedia(ctx context.Context, w http.ResponseWriter, x *kernel.Call, authority keyauthority.Authority, request *media.Request, sized *int64) {
	defer s.cleanupUploads(request)
	configureMediaCall(x, request, sized)
	if e := s.prepareMedia(ctx, x, authority, request); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	ctx, cancel := x.RouteContext(ctx)
	defer cancel()
	if e := s.kernel.ReserveRouteKey(ctx, x, authority); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	defer s.kernel.SettleAdmission(ctx, x)

	attempted := kernel.RunAttempts(ctx, s.kernel, x, kernel.AttemptAdapter[*transport.Result]{
		Demand: func(routing.Attempt, *routing.Provider) quota.TokenDemand {
			return quota.CombinedDemand(x.Estimate())
		},
		Dispatch: func(ctx context.Context, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, *transport.Result, *kernel.AttemptFailure) {
			return s.mediaAttempt(ctx, w, x, request, attempt, provider, slot, fact)
		},
	})
	if attempted.Err != nil {
		status := (&fault.StreamWriter{W: w, Family: x.Family(), Committed: attempted.Committed}).Finish(attempted.Err, attempted.Cancelled)
		s.kernel.Finish(x, &kernel.Outcome{Err: attempted.Err, Committed: attempted.Committed, Cancelled: attempted.Cancelled}, status)
		return
	}
	if _, isEvents := attempted.Result.Payload.(transport.Events); isEvents {
		s.kernel.Finish(x, &kernel.Outcome{Committed: attempted.Committed}, http.StatusOK)
		return
	}
	delivered := s.writeMediaResult(w, x, request, attempted.Result, http.StatusOK, "")
	s.kernel.Finish(x, &delivered.outcome, delivered.status)
}

func (s surface) prepareMedia(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, request *media.Request) *fault.Error {
	return s.kernel.PlanSurfaceRoute(ctx, x, authority, request.Route(), request.Op() == media.OpVideoCreate, func(ctx context.Context, x *kernel.Call) *fault.Error {
		return s.planMedia(ctx, x, request)
	})
}

func (s surface) planMedia(ctx context.Context, x *kernel.Call, request *media.Request) *fault.Error {
	if x.Snapshot().GuardrailEngine(x.Route.Slug, x.KeyID()) != nil {
		if e := x.GuardrailSurfaceGate(x.Route); e != nil {
			return e
		}
	}
	if e := s.kernel.CheckRouteFidelity(ctx, x.Route); e != nil {
		return e
	}
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
	return s.kernel.SelectSurfacePlan(ctx, x, kernel.SurfacePlanRequest{
		Operation:  string(request.Op()),
		Surface:    "openai",
		Mode:       x.Mode(),
		Parameters: request.ParameterNames(),
		Eligible: func(p routing.Provider, t routing.Target) error {
			if !p.Connector().Supports(string(request.Op()), "openai", x.Mode()) {
				return errors.New("connector capability unavailable")
			}
			if video && !videoLifecycle(func(op string) bool { return p.Supports(t.ProviderModel, op, "openai", "unary") }) {
				return errors.New("video lifecycle capabilities unavailable")
			}
			return nil
		},
		Prepare: func(p routing.Provider, t routing.Target) (*routing.CandidatePreparation, error) {
			encoded, err := encodeMedia(x, request, snapshot, p, t.ID, t.ProviderModel)
			if err != nil {
				return nil, err
			}
			if p.ProfileID == "" {
				return &routing.CandidatePreparation{Parameters: routing.Listed(request.ParameterNames())}, nil
			}
			parameters, err := media.OutboundParameterNames(encoded.call, encoded.effective)
			if err != nil {
				return nil, err
			}
			return &routing.CandidatePreparation{Parameters: routing.Listed(parameters)}, nil
		},
	})
}

func encodeMedia(x *kernel.Call, request *media.Request, snapshot *routing.View, provider routing.Provider, targetID, model string) (encodedMedia, error) {
	if !x.Strict() {
		call, effective, failure := media.EncodeConfigured(request, provider.Connector(), model)
		if failure != nil {
			return encodedMedia{}, errors.New(failure.Message)
		}
		return encodedMedia{call: call, effective: effective}, nil
	}
	call, effective, failure := media.EncodeStrictConfigured(request, provider.Connector(), model)
	if failure != nil {
		return encodedMedia{}, errors.New(failure.Message)
	}
	template, ok := snapshot.MediaTemplate(x.Route.Slug, targetID, string(request.Op()))
	if !ok {
		return encodedMedia{}, errors.New("compiled strict media contract unavailable")
	}
	input, err := media.ContractInput(request, call, effective)
	if err != nil {
		return encodedMedia{}, err
	}
	bound, err := template.Bind(input)
	if err != nil {
		return encodedMedia{}, err
	}
	return encodedMedia{call: call, effective: effective, template: template, bound: bound}, nil
}

func strictMediaContext(x *kernel.Call) *fault.Error {
	query, invalid := x.SemanticQuery()
	if invalid || len(query) != 0 {
		return fault.InvalidRequest("target_capability", "This native media route does not qualify caller query settings.", nil)
	}
	return strictMediaHeaders(x)
}

func strictMediaHeaders(x *kernel.Call) *fault.Error {
	for name := range x.SemanticHeaders() {
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

func videoLifecycle(supported func(op string) bool) bool {
	return supported(string(media.OpVideoGet)) && supported(string(media.OpVideoContent)) && supported(string(media.OpVideoDelete))
}

func (s surface) cleanupUploads(request *media.Request) {
	if request == nil {
		return
	}
	for _, handle := range request.Uploads() {
		if err := s.spool.Remove(handle); err != nil {
			s.kernel.Log.Warn("media upload cleanup failed", "error", err)
		}
	}
}
