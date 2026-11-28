package kernel

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

const StateRouteHeader = "X-OLP-Route"

const ResourceEstimate = 100

// Once a provider has accepted durable work, its owner mapping must outlive
// a client disconnect. Persistence still has a short independent deadline.
const resourceCommitTimeout = 5 * time.Second

func ResourceCommitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), resourceCommitTimeout)
}

func (s *Server) SettleFence(ctx context.Context, settle func(context.Context) error) {
	commit, cancel := ResourceCommitContext(ctx)
	defer cancel()
	if err := settle(commit); err != nil {
		s.Log.Warn("retained resource fence not settled", "error", err.Error())
	}
}

const maxResourceList = 100

func (p *Pin) Timeout() time.Duration { return p.attempt.Timeout }

type Pin struct {
	Target   routing.Target
	Provider routing.Provider
	attempt  routing.Attempt
	Slot     routing.Slot
	Model    string
	Hold     *dispatchHold
}

func ReadBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

func stateQualified(p *routing.Provider, model, operation, mode string) bool {
	if !p.Connector().Supports(operation, "openai", mode) {
		return false
	}
	if !p.Supports(model, operation, "openai", mode) {
		return false
	}
	return p.Connector().SupportsFilesBatches()
}

func ResourceURL(cfg connectors.Config, model, path string, query url.Values) (string, error) {
	return cfg.ResourceURL(model, path, query)
}

func ResourceModel(res *resources.Resource) string {
	var meta struct {
		UpstreamModel string `json:"upstream_model"`
	}
	_ = json.Unmarshal(res.Metadata, &meta)
	return meta.UpstreamModel
}

func (s *Server) SelectPin(ctx context.Context, x *Call, route *routing.Route, operation, mode string) (*Pin, *fault.Error) {
	return s.SelectPinSurface(ctx, x, route, operation, "openai", mode, func(p *routing.Provider, model string) bool {
		return stateQualified(p, model, operation, mode)
	})
}

func (s *Server) SelectPinSurface(ctx context.Context, x *Call, route *routing.Route, operation, surface, mode string, qualified func(*routing.Provider, string) bool) (*Pin, *fault.Error) {
	return s.SelectBoundPinSurface(ctx, x, route, operation, surface, mode, ResourceDemand(x), nil, qualified)
}

func (s *Server) SelectGenerationPin(ctx context.Context, x *Call, route *routing.Route, surface, mode string, replyBound int64, qualified func(*routing.Provider, string) bool) (*Pin, *fault.Error) {
	return s.SelectBoundPinSurface(ctx, x, route, "generation", surface, mode, quota.GenerationDemand(x.estimate, replyBound, 1), nil, qualified)
}

func ResourceDemand(x *Call) quota.TokenDemand {
	return quota.CombinedDemand(max(ResourceEstimate, x.estimate))
}

type RetainedSlotQualifier func(*routing.Provider, *routing.Slot, string) bool

func (s *Server) SelectBoundPinSurface(ctx context.Context, x *Call, route *routing.Route, operation, surface, mode string, demand quota.TokenDemand, bound *resources.Resource, qualified func(*routing.Provider, string) bool, slotQualifiers ...RetainedSlotQualifier) (*Pin, *fault.Error) {
	if e := s.CheckRouteFidelity(ctx, route); e != nil {
		return nil, e
	}
	if e := x.CheckBody(route); e != nil {
		return nil, e
	}
	snapshot := x.request.release.Snapshot()
	// Pinned work must select a target on this route rather than delegate.
	x.fixed = true
	qualifiesSlot := func(provider *routing.Provider, slot *routing.Slot, model string) bool {
		for _, qualify := range slotQualifiers {
			if qualify != nil && !qualify(provider, slot, model) {
				return false
			}
		}
		return true
	}
	options := s.selectionOptions(ctx, x)
	options.Prepare = func(p routing.Provider, t routing.Target) (*routing.CandidatePreparation, error) {
		if bound != nil && (bound.ProviderID != p.ID || bound.ProviderRevisionID != p.RevisionID || bound.TargetID != t.ID) {
			return nil, errors.New("resource affinity unavailable")
		}
		if !qualified(&p, t.ProviderModel) {
			return nil, errors.New("provider capability unavailable")
		}
		if len(slotQualifiers) > 0 {
			attempt := routing.Attempt{ProviderID: p.ID, UpstreamModel: t.ProviderModel}
			for i := range p.Slots {
				slot := &p.Slots[i]
				if bound != nil && (bound.SlotID != slot.ID || !sameCredential(bound.CredentialID, slot.CredentialID)) {
					continue
				}
				if qualifiesSlot(&p, slot, t.ProviderModel) && s.slotAvailable(x, attempt, slot) && !s.cooling(ctx, p.ID, slot) {
					return nil, nil
				}
			}
			return nil, errors.New("credential model authority unavailable")
		}
		return nil, nil
	}
	session := s.planning(x, pinnedPlanning, route, operation, surface, mode)
	plan, _, failure := session.selectPlan(ctx, options)
	if failure != nil {
		return nil, failure
	}
	attempt := plan.Attempts[0]
	provider, ok := snapshot.FindProvider(attempt.ProviderID)
	if !ok {
		return nil, selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, route.Slug)
	}
	if e := x.checkCallerTarget(&provider, attempt.UpstreamModel); e != nil {
		return nil, e
	}
	var target routing.Target
	for _, candidate := range route.Targets {
		if candidate.ID == attempt.TargetID {
			target = candidate
			break
		}
	}
	if target.ID == "" {
		return nil, selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, route.Slug)
	}
	session.adoptPin(plan)
	deadline := s.Now().Add(time.Duration(route.OverallTimeout) * time.Millisecond)
	if mode == "realtime" || mode == "webrtc" {
		var ok bool
		deadline, ok = ctx.Deadline()
		if !ok {
			return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "Realtime admission requires a session deadline.")
		}
	}
	for i := range provider.Slots {
		slot := &provider.Slots[i]
		if bound != nil && (bound.SlotID != slot.ID || !sameCredential(bound.CredentialID, slot.CredentialID)) {
			continue
		}
		if !qualifiesSlot(&provider, slot, attempt.UpstreamModel) {
			continue
		}
		if !s.slotAvailable(x, attempt, slot) || s.cooling(ctx, provider.ID, slot) {
			continue
		}
		gate := s.gateAttempt(ctx, x, attempt, &provider, slot, demand, deadline)
		switch gate.verdict {
		case gateAdmitted:
			return &Pin{Target: target, Provider: provider, attempt: attempt, Slot: *slot, Model: attempt.UpstreamModel, Hold: gate.hold}, nil
		case gateRejected:
			return nil, gate.rejection.Fault()
		case gateExpired:
			return nil, (&AttemptFailure{class: ClassTimeout}).Fault()
		}
	}
	return nil, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No credential slot is currently able to serve `"+route.Slug+"`.")
}

func (s *Server) PinnedDo(ctx context.Context, x *Call, p *Pin, method, endpoint string, body []byte, contentType string) (*PinnedResponse, *AttemptFailure) {
	exchange := s.beginPinned(ctx, x, p, method)
	if err := exchange.bound.validateEndpoint(endpoint); err != nil {
		return nil, exchange.fail(ClassConnect, nil)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := exchange.bound.request(ctx, method, endpoint, reader, make(http.Header))
	if err != nil {
		return nil, exchange.fail(ClassConnect, nil)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "olp/gateway")
	accept := "application/json"
	if x.PinnedBinary {
		accept = "application/binary"
	}
	req.Header.Set("Accept", accept)
	if x.mode == "streaming" && (x.family == wire.GeminiInteractions || x.family == wire.Responses) {
		req.Header.Set("Accept", "text/event-stream")
	}
	if err := exchange.bound.authenticate(ctx, req, body); err != nil {
		if ctx.Err() != nil {
			return nil, exchange.fail(ClassCancelled, nil)
		}
		return nil, exchange.fail(ClassCredential, nil)
	}
	maps.Copy(req.Header, x.PinnedHeaders)
	// Skills use the GA contract the pinned SDK parses (id-addressed
	// versions, latest_version_id). The dated beta header would opt
	// into legacy response shapes with incompatible identities, so
	// it is deliberately not sent.
	return s.doPinnedRequest(ctx, p, exchange, req, openai.ParseErrorBody)
}

func UpstreamError(f *AttemptFailure) *fault.Error {
	if f.upstream != nil && f.upstream.Message != "" {
		return &fault.Error{Status: f.Status, Type: f.upstream.Type, Code: f.upstream.Code, Message: f.upstream.Message}
	}
	return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
}

func (s *Server) StateBegin(w http.ResponseWriter, r *http.Request, family wire.Format) (*Call, keyauthority.Authority, bool) {
	x := newCall(s.begin(w, r), family, callIdentity{actor: "api_key"}, callLineage{}, callWork{})
	authority, ok := s.EnterCall(w, r, x, s.authenticateInference)
	return x, authority, !ok
}

func (s *Server) StateDeadline(ctx context.Context, route *routing.Route) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(route.OverallTimeout)*time.Millisecond)
}

func (s *Server) ReserveState(ctx context.Context, x *Call, authority keyauthority.Authority, overall time.Duration) *fault.Error {
	if e := x.CheckBody(x.Route); e != nil {
		return e
	}
	var e *fault.Error
	x.lease, e = s.Deps.Admission.ReserveKey(ctx, x.AdmissionAuthority(authority), x.clientSurface(), max(ResourceEstimate, x.estimate), overall, x.limitRoute())
	return e
}

type nativeStateWording struct {
	missingRoute string
	strictRoute  string
}

var nativeStateWordings = map[string]nativeStateWording{
	"anthropic": {missingRoute: "X-OLP-Route or params.model is required.", strictRoute: "Strict routes do not serve Anthropic files and batches."},
	"gemini":    {missingRoute: "X-OLP-Route is required.", strictRoute: "Strict routes do not serve Gemini files."},
}

func (s *Server) NativeStateRoute(x *Call, authority keyauthority.Authority, r *http.Request, requested string) (*routing.Route, *fault.Error) {
	wording := nativeStateWordings[x.family.Surface()]
	slug := cmp.Or(strings.TrimSpace(r.Header.Get(StateRouteHeader)), requested)
	if slug == "" {
		return nil, fault.InvalidRequest("missing_required_parameter", wording.missingRoute, nil)
	}
	route, ok := x.Snapshot().FindRoute(slug)
	if !ok {
		return nil, fault.ModelNotFound(slug)
	}
	x.Route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
		return nil, fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
	}
	if !slices.Contains(route.Operations, "batch") {
		return nil, fault.InvalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow batch operations.", nil)
	}
	if x.Strict() {
		return nil, PolicyUnavailable("target_capability", wording.strictRoute)
	}
	if e := x.GuardrailSurfaceGate(&route); e != nil {
		return nil, e
	}
	return &route, nil
}

func (s *Server) NativeStatePin(ctx context.Context, x *Call, route *routing.Route) (*Pin, *fault.Error) {
	surface := x.family.Surface()
	return s.SelectPinSurface(ctx, x, route, "batch", surface, "unary", func(p *routing.Provider, model string) bool {
		return p.Connector().Supports("batch", surface, "unary") && p.Supports(model, "batch", surface, "unary")
	})
}

func (s *Server) ReserveStateAndSlot(ctx context.Context, x *Call, authority keyauthority.Authority, p *Pin, route *routing.Route) *fault.Error {
	overall := time.Duration(route.OverallTimeout) * time.Millisecond
	if e := s.ReserveState(ctx, x, authority, overall); e != nil {
		return e
	}
	if p.Hold != nil {
		return nil
	}
	gate := s.gateSlot(ctx, &p.Provider, &p.Slot, quota.CombinedDemand(ResourceEstimate), s.Now().Add(overall), "", "", 0)
	if gate.verdict != gateAdmitted {
		return gateError(gate)
	}
	p.Hold = gate.hold
	return nil
}

func (s *Server) ResourceSettle(ctx context.Context, x *Call, p *Pin) {
	if p != nil {
		s.SettlePinnedHold(ctx, p.Hold)
	}
	s.SettleCaps(ctx, x)
	quota.SettleKey(ctx, x.lease, x.dispatched, nil, s.Log)
}
