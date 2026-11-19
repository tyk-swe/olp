package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/sse"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) beginGeminiInteraction(w http.ResponseWriter, r *http.Request, resourceCall bool) (*call, keyauthority.Authority, bool) {
	actor := "api_key"
	if resourceCall {
		actor = "api_key_resource"
	}
	x := &call{request: s.begin(w, r), family: wire.GeminiInteractions, actor: actor, mode: "unary"}
	authority, ok := s.enter(w, r, x, func(r *http.Request) (keyauthority.Authority, *fault.Error) {
		if _, e := geminiClientKey(r, "stream", "last_event_id", "include_input"); e != nil {
			return keyauthority.Authority{}, e
		}
		return s.gate.Authenticate(r, "inference")
	})
	return x, authority, ok
}

func (s *Server) geminiInteractionCreate(w http.ResponseWriter, r *http.Request) {
	x, authority, ok := s.beginGeminiInteraction(w, r, false)
	if !ok {
		return
	}
	defer s.gate.Release(r.Context())
	status := http.StatusInternalServerError
	var out *outcome
	var p *pin
	defer func() {
		s.finish(x, out, status)
		if p != nil {
			s.settlePinnedHold(r.Context(), p.hold)
		}
		s.settleAdmission(r.Context(), x)
	}()
	fail := func(e *fault.Error) {
		x.failure, status = e, e.Status
		if x.dispatched && x.strict() && e.Status >= 500 {
			copy := *e
			copy.NoRetry = true
			e = &copy
		}
		fault.WriteSurfaceError(w, e, "gemini")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["last_event_id"]) > 0 || len(query["stream"]) > 0 || len(query["include_input"]) > 0 {
		fail(fault.InvalidRequest("invalid_request", "Interaction create does not accept retrieval controls.", nil))
		return
	}
	body, e := s.gate.ReadBody(r)
	if e != nil {
		fail(e)
		return
	}
	maxBody := int(s.cfg.MaxBodyBytes)
	if maxBody <= 0 {
		maxBody = len(body)
	}
	input, err := geminilifecycle.ParseInteractionRequest(body, maxBody)
	if err != nil || !routeidentity.ValidSlug(input.Model) {
		fail(fault.InvalidRequest("invalid_interaction_request", "The Gemini Interaction request is malformed.", nil))
		return
	}
	if input.Stream {
		x.mode = "streaming"
	}
	route, found := x.request.release.Snapshot().FindRoute(input.Model)
	if !found {
		fail(fault.ModelNotFound(input.Model))
		return
	}
	x.route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		fail(fault.ModelNotFound(input.Model))
		return
	}
	if !slices.Contains(route.Operations, "generation") {
		fail(fault.InvalidRequest("invalid_request", "This route does not allow Interactions.", nil))
		return
	}
	if e := x.guardrailSurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	if e := x.checkBody(&route); e != nil {
		fail(e)
		return
	}
	if input.Store || input.Previous != "" || input.Background {
		if !authority.Policy.AllowProviderState {
			fail(fault.InvalidRequest("policy_conflict", "This API key does not allow provider-retained Interaction state.", nil))
			return
		}
		if s.deps.Resources == nil || !s.deps.Resources.Encrypted() || s.deps.Resolver == nil {
			fail(fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Encrypted Interaction state is not configured."))
			return
		}
	}
	if x.preferences, x.priority, e = routingControls(r, authority.Policy); e != nil {
		fail(e)
		return
	}
	if input.Background {
		if e := unboundedWorkLimits(x.admissionAuthority(authority), nil, nil, &route); e != nil {
			fail(e)
			return
		}
	}
	ctx, cancel := s.stateDeadline(r.Context(), &route)
	defer cancel()
	sized := int64(len(body)) / 4
	x.estimate, x.sizedInput = max(resourceEstimate, sized), &sized
	x.lease, e = s.deps.Admission.ReserveKey(ctx, x.admissionAuthority(authority), x.clientSurface(), x.estimate, time.Duration(route.OverallTimeout)*time.Millisecond, x.limitRoute())
	if e != nil {
		fail(e)
		return
	}
	var parent *resources.Resource
	previousUpstream := ""
	if input.Previous != "" {
		parent, previousUpstream, err = s.deps.Resources.ReadInteractionContract(ctx, authority.ID, input.Previous)
		if errors.Is(err, resources.ErrNotFound) || parent != nil && parent.RouteSlug != input.Model {
			fail(fault.InvalidRequest("invalid_previous_interaction_id", "The previous Interaction is not owned by this key and route.", new("previous_interaction_id")))
			return
		}
		if err != nil {
			fail(fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The previous Interaction cannot be read."))
			return
		}
		var retainedRoute *routing.Route
		p, retainedRoute, e = s.resolveResource(ctx, x, authority, parent, "generation", retainedNewWork)
		if e != nil || retainedRoute == nil || retainedRoute.Slug != route.Slug || p.provider.ProfileID != "gemini-interactions" || !p.provider.Supports(p.model, "generation", "gemini", x.mode) {
			fail(pinUnavailable())
			return
		}
		x.pinnedSlot = &p.slot
		p.attempt.Price = s.deps.Runtime.RoutingInputs().Price(string(p.provider.Kind), p.provider.ID, p.provider.VendorID, p.model, "generation", s.now())
		p.attempt.Discount = s.deps.Runtime.RoutingInputs().Discount(string(p.provider.Kind), p.provider.ID, p.provider.VendorID, s.now())
		deadline, _ := ctx.Deadline()
		gate := s.gateAttempt(ctx, x, p.attempt, &p.provider, &p.slot, x.estimate, deadline)
		if gate.verdict != gateAdmitted {
			fail(gateError(gate))
			return
		}
		p.hold = gate.hold
		x.attempts, x.budget = []routing.Attempt{p.attempt}, 1
	} else {
		p, e = s.selectPinSurface(ctx, x, &route, "generation", "gemini", x.mode, func(provider *routing.Provider, model string) bool {
			return provider.ProfileID == "gemini-interactions" && provider.Connector().Supports("generation", "gemini", x.mode) && provider.Supports(model, "generation", "gemini", x.mode)
		})
		if e != nil {
			fail(e)
			return
		}
	}
	if input.Background {
		if e := unboundedWorkLimits(x.admissionAuthority(x.authority), &p.provider, &p.slot, &route); e != nil {
			fail(e)
			return
		}
	}
	// Admission ran before the target was known, so the cost hold follows the
	// target's selection, before anything is dispatched.
	if e = s.reserveFallbackCost(ctx, x); e != nil {
		fail(e)
		return
	}
	endpoint, err := p.provider.Connector().InteractionsURL("", "")
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_error", "The Interactions profile cannot address this model."))
		return
	}
	bound, err := input.Bind(p.provider.Connector().Model(p.model), previousUpstream)
	if err != nil {
		fail(fault.InvalidRequest("invalid_interaction_request", "The Interaction identities could not be bound.", nil))
		return
	}
	response, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, bound, "application/json")
	if failure != nil {
		fail(failure.toError())
		return
	}

	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unsupported Interaction status."))
		return
	}
	if input.Stream {
		var committed bool
		var streamErr error
		committed, streamErr = s.streamGeminiInteraction(ctx, w, x, p, response, authority, input.Store, parent, nil, "", false)
		if streamErr != nil {
			if errors.Is(streamErr, fault.ErrClientWrite) || r.Context().Err() != nil {
				x.failure = (&attemptFailure{class: classCancelled}).toError()
				x.failure.NoRetry = true
				status, out = 0, &outcome{err: x.failure, committed: committed, cancelled: true}
				return
			}
			x.failure = fault.ServerError(http.StatusBadGateway, "interaction_incomplete", "The Interaction stream ended before its terminal contract.")
			status = x.failure.Status
			if !committed {
				fault.WriteSurfaceError(w, x.failure, "gemini")
			}
			out = &outcome{err: x.failure, committed: committed}
			return
		}
		status, out = http.StatusOK, &outcome{err: x.failure, committed: true}
		return
	}
	limit := s.cfg.MaxResponseBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	raw, err := response.readBody(limit)
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Interaction response exceeded its configured limit."))
		return
	}
	result, err := geminilifecycle.ParseInteractionResponse(raw, int(limit))
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Interaction resource."))
		return
	}
	terminal := geminilifecycle.InteractionTurnFinished(result.Status)
	nativeFailure := result.Status == "failed"
	recordGeminiInteractionUsage(response.exchange, result.Source.Root())
	switch {
	case nativeFailure:
		response.exchange.complete(classUpstreamServer)
		x.failure = geminiGenerationFailure()
	case terminal:
		response.exchange.complete(classSuccess)
	default:
		fact := response.exchange.fact()
		fact.Class = classSuccess
		fact.ObserveUsage(fact.Usage, false)
		response.exchange.end()
	}
	localID := result.ID
	if input.Store {
		stored, storeErr := s.storeGeminiInteraction(ctx, response.exchange, p, authority.ID, result.ID, result.Status, parent, !terminal)
		if storeErr != nil {
			fail(fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Interaction mapping could not be committed."))
			return
		}
		localID = stored.ID
	}
	if nativeFailure {
		result.Source, err = fault.RedactNativeFailureDocument(result.Source, x.sensitive)
		if err != nil {
			fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction failure could not be safely delivered."))
			return
		}
	}
	projection, err := result.WithProjection(localID, input.Previous, input.Model)
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction result contains an unbound resource reference."))
		return
	}
	if input.Store && containsNativeResourceID(projection, result.ID, previousUpstream) {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction result contains an unbound native resource identity."))
		return
	}
	if int64(len(projection)) > limit {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The projected Interaction exceeded its configured limit."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	x.responseHeaders(w.Header(), false)
	w.Header().Set("X-Should-Retry", "false")
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	w.WriteHeader(response.StatusCode)
	n, writeErr := w.Write(projection)
	if writeErr == nil && n == len(projection) {
		writeErr = controller.Flush()
	}
	if writeErr != nil || n != len(projection) {
		x.failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The Interaction result could not be delivered.")
		x.failure.NoRetry = true
		status, out = 0, &outcome{err: x.failure, committed: true, cancelled: true}
		response.exchange.delivery(deliveryState(n))
		return
	}
	x.delivered(s.now())
	status, out = response.StatusCode, &outcome{err: x.failure, committed: true}
	response.exchange.delivery(usage.ClientTerminal)
}

func (s *Server) storeGeminiInteraction(ctx context.Context, exchange *pinnedExchange, p *pin, owner, upstreamID, state string, parent *resources.Resource, deferUsage bool) (*resources.Resource, error) {
	x := exchange.request
	version := resources.InteractionContract
	expiry := s.now().Add(resources.ContinuationLifetime - time.Minute)
	model, _ := json.Marshal(p.model)
	metadataFields := map[string]json.RawMessage{"upstream_model": model}
	if deferUsage && !s.pendingGenerationUsage(x, exchange.fact(), metadataFields) {
		return nil, errors.New("Interaction generation accounting template unavailable")
	}
	metadata, err := json.Marshal(metadataFields)
	if err != nil {
		return nil, err
	}
	r := &resources.Resource{Kind: resources.KindInteraction, APIKeyID: owner, RouteSlug: x.route.Slug,
		ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: x.route.RevisionID,
		TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID, State: state, Metadata: metadata,
		ContractVersion: &version, ExpiresAt: &expiry}
	if parent != nil {
		r.ParentID = &parent.UUID
	}
	commitCtx, stopCommit := resourceCommitContext(ctx)
	defer stopCommit()
	stored, err := s.deps.Resources.PutInteractionContract(commitCtx, r, upstreamID)
	if err == nil && deferUsage {
		if err = attachGenerationHTTP(x, stored); err != nil {
			return nil, err
		}
		exchange.fact().GenerationUsageDeferred = true
		exchange.recordEvidence()
	}
	return stored, err
}

// Untouched OIF strings keep their source escape spelling. Inspect decoded
// values as well as member names before exposing an owner-scoped projection:
// a raw byte search misses IDs such as "int\\u005ffoo".
func containsNativeResourceID(body []byte, ids ...string) bool {
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil {
		return true
	}
	contains := func(value string) bool {
		for _, id := range ids {
			if id != "" && strings.Contains(value, id) {
				return true
			}
		}
		return false
	}
	var visit func(oif.Value) bool
	visit = func(value oif.Value) bool {
		switch value.Kind() {
		case oif.String:
			text, ok := value.Text()
			return !ok || contains(text)
		case oif.Object:
			for _, member := range value.Members() {
				if contains(member.Name) || visit(member.Value) {
					return true
				}
			}
		case oif.Array:
			for _, item := range value.Elements() {
				if visit(item) {
					return true
				}
			}
		}
		return false
	}
	return visit(doc.Root())
}

func (s *Server) geminiInteractionResource(w http.ResponseWriter, r *http.Request) {
	x, authority, ok := s.beginGeminiInteraction(w, r, true)
	if !ok {
		return
	}
	defer s.gate.Release(r.Context())
	status := http.StatusInternalServerError
	var out *outcome
	var p *pin
	defer func() {
		s.finish(x, out, status)
		if p != nil {
			s.settlePinnedHold(r.Context(), p.hold)
		}
		quota.SettleKey(r.Context(), x.lease, x.dispatched, x.settledTokens(), s.log)
	}()
	fail := func(e *fault.Error) {
		x.failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, "gemini")
	}
	if s.deps.Resources == nil || !s.deps.Resources.Encrypted() || s.deps.Resolver == nil || !authority.Policy.AllowProviderState {
		fail(fault.PermissionError("provider_state_unavailable", "Interaction state is unavailable to this key."))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(fault.InvalidRequest("invalid_request", "The Interaction query is malformed.", nil))
		return
	}
	stream := false
	if value, found := query["stream"]; found {
		if value[0] != "true" && value[0] != "false" {
			fail(fault.InvalidRequest("invalid_request", "stream must be true or false.", nil))
			return
		}
		stream = value[0] == "true"
	}
	if stream {
		x.mode = "streaming"
	}
	if r.Method != http.MethodGet && (len(query["stream"]) > 0 || len(query["last_event_id"]) > 0 || len(query["include_input"]) > 0) || len(query["last_event_id"]) > 0 && !stream {
		fail(fault.InvalidRequest("invalid_request", "Interaction retrieval controls are invalid for this method.", nil))
		return
	}
	if values := query["include_input"]; len(values) > 0 && values[0] != "true" && values[0] != "false" {
		fail(fault.InvalidRequest("invalid_request", "include_input must be true or false.", nil))
		return
	}
	if values := query["last_event_id"]; len(values) > 0 && len(values[0]) > 512 {
		fail(fault.InvalidRequest("invalid_request", "last_event_id is too long.", nil))
		return
	}
	localID := r.PathValue("id")
	res, upstreamID, err := s.deps.Resources.ReadInteractionContract(r.Context(), authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		fail(fault.NotFoundError("interaction_not_found", "This Interaction does not exist for this key."))
		return
	}
	if err != nil {
		fail(fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The Interaction mapping cannot be read."))
		return
	}
	current, found := x.request.release.Snapshot().FindRoute(res.RouteSlug)
	if !found || !authority.Allows("inference", current.Slug, current.ProjectID, s.now()) {
		fail(fault.NotFoundError("interaction_not_found", "This Interaction does not exist for this key."))
		return
	}
	x.route = &current
	if e := x.guardrailSurfaceGate(&current); e != nil {
		fail(e)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), &current)
	defer cancel()
	if e := s.reserveState(ctx, x, authority, time.Duration(current.OverallTimeout)*time.Millisecond); e != nil {
		fail(e)
		return
	}
	x.estimate = resourceEstimate
	var retained *routing.Route
	var e *fault.Error
	use := retainedHousekeeping
	if r.Method == http.MethodGet {
		use = retainedRetrieval
	}
	p, retained, e = s.resolveResource(ctx, x, authority, res, "generation", use)
	if e != nil || retained == nil || retained.Slug != current.Slug || p.provider.ProfileID != "gemini-interactions" || !p.provider.Supports(p.model, "generation", "gemini", x.mode) {
		fail(pinUnavailable())
		return
	}
	x.pinnedSlot = &p.slot
	deadline, _ := ctx.Deadline()
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, deadline, "")
	if gate.verdict != gateAdmitted {
		fail(gateError(gate))
		return
	}
	p.hold = gate.hold
	method, action := r.Method, ""
	if r.Method == http.MethodPost {
		if !strings.HasSuffix(r.URL.Path, "/cancel") {
			fail(fault.NotFoundError("not_found", "Unknown Interaction action."))
			return
		}
		action = "cancel"
	}
	endpoint, err := p.provider.Connector().InteractionsURL(upstreamID, action)
	if err != nil {
		fail(pinUnavailable())
		return
	}
	if method == http.MethodGet {
		upstreamQuery := url.Values{}
		for _, name := range []string{"stream", "last_event_id", "include_input"} {
			if values := query[name]; len(values) == 1 {
				upstreamQuery.Set(name, values[0])
			}
		}
		if len(upstreamQuery) > 0 {
			endpoint += "?" + upstreamQuery.Encode()
		}
	}
	response, failure := s.pinnedDo(ctx, x, p, method, endpoint, nil, "")
	if method == http.MethodDelete && (failure == nil || failure.status == http.StatusNotFound) {
		deletionErr := s.finishGeminiInteractionDeletion(ctx, w, x, response, func(commitCtx context.Context) error {
			return s.deps.Resources.DeleteInteractionContract(commitCtx, authority.ID, localID)
		})
		if deletionErr == nil {
			return
		}
		if failure == nil {
			fail(deletionErr)
			return
		}
	}
	if failure != nil {
		fail(failure.toError())
		return
	}

	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unsupported Interaction status."))
		return
	}
	if stream {
		committed, streamErr := s.streamGeminiInteraction(ctx, w, x, p, response, authority, true, nil, res, upstreamID, len(query["last_event_id"]) > 0)
		if streamErr != nil {
			if errors.Is(streamErr, fault.ErrClientWrite) || r.Context().Err() != nil {
				x.failure = (&attemptFailure{class: classCancelled}).toError()
				status, out = 0, &outcome{err: x.failure, committed: committed, cancelled: true}
				return
			}
			x.failure = fault.ServerError(http.StatusBadGateway, "interaction_incomplete", "The Interaction retrieval stream ended before its terminal contract.")
			status = x.failure.Status
			if !committed {
				fault.WriteSurfaceError(w, x.failure, "gemini")
			}
			out = &outcome{err: x.failure, committed: committed}
			return
		}
		status, out = http.StatusOK, &outcome{committed: true}
		return
	}
	limit := s.cfg.MaxResponseBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	raw, err := response.readBody(limit)
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Interaction response exceeded its configured limit."))
		return
	}
	result, err := geminilifecycle.ParseInteractionResponse(raw, int(limit))
	if err != nil || result.ID != upstreamID {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid or different Interaction."))
		return
	}
	// A matching native result belongs to the original generation even when
	// its owner-scoped projection cannot be delivered.
	response.exchange.complete(classSuccess)
	commitCtx, stopCommit := resourceCommitContext(ctx)
	_, err = s.observeGeminiInteraction(commitCtx, authority.ID, localID, result.Status, result.Source.Root())
	stopCommit()
	if err != nil {
		fail(fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Interaction state could not be updated."))
		return
	}
	parentID := ""
	if res.ParentID != nil {
		parentID = resources.LocalID(resources.KindInteraction, *res.ParentID)
	}
	if result.Status == "failed" {
		result.Source, err = fault.RedactNativeFailureDocument(result.Source, x.sensitive)
		if err != nil {
			fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction failure could not be safely delivered."))
			return
		}
	}
	projection, err := result.WithProjection(localID, parentID, res.RouteSlug)
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unbound Interaction reference."))
		return
	}
	if containsNativeResourceID(projection, upstreamID) {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unbound native resource identity."))
		return
	}
	if int64(len(projection)) > limit {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The projected Interaction exceeded its configured limit."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	x.responseHeaders(w.Header(), false)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	w.WriteHeader(response.StatusCode)
	n, writeErr := w.Write(projection)
	if writeErr == nil && n == len(projection) {
		writeErr = controller.Flush()
	}
	if writeErr != nil || n != len(projection) {
		x.failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The Interaction resource could not be delivered.")
		status, out = 0, &outcome{err: x.failure, committed: true, cancelled: true}
		response.exchange.delivery(deliveryState(n))
		return
	}
	x.delivered(s.now())
	response.exchange.delivery(usage.ClientTerminal)
	status, out = response.StatusCode, &outcome{committed: true}
}

func (s *Server) finishGeminiInteractionDeletion(ctx context.Context, w http.ResponseWriter, x *call, response *pinnedResponse, tombstone func(context.Context) error) *fault.Error {
	if response != nil {
		defer response.Body.Close()
		response.exchange.complete(classSuccess)
	}
	commitCtx, stopCommit := resourceCommitContext(ctx)
	err := tombstone(commitCtx)
	stopCommit()
	if err != nil {
		return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The deleted Interaction could not be tombstoned.")
	}
	if response == nil {
		s.writeStateJSONStatus(w, x, nil, http.StatusOK)
		return nil
	}
	s.writeStateJSONStatus(w, x, nil, http.StatusOK, response.exchange)
	return nil
}

func (s *Server) streamGeminiInteraction(ctx context.Context, w http.ResponseWriter, x *call, p *pin, response *pinnedResponse, authority keyauthority.Authority, store bool, parent, existing *resources.Resource, expectedID string, resumed bool) (bool, error) {
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return false, errors.New("provider did not return Interaction SSE")
	}
	limit := int(s.cfg.MaxEventBytes)
	if limit <= 0 {
		limit = 1 << 20
	}
	state := &geminilifecycle.InteractionStream{ID: expectedID, Created: resumed, Resumed: resumed}
	local := existing
	localID := expectedID
	if existing != nil {
		localID = existing.ID
	}
	previous := ""
	if parent != nil {
		previous = parent.ID
	} else if existing != nil && existing.ParentID != nil {
		previous = resources.LocalID(resources.KindInteraction, *existing.ParentID)
	}
	committed := false
	exchange := response.exchange
	err = sse.Decode(response.Body, limit, func(frame sse.Frame) error {
		event, err := geminilifecycle.ParseInteractionEvent([]byte(frame.Data), limit)
		if err != nil {
			exchange.invalidNative()
			return err
		}
		if err := state.Accept(event); err != nil {
			exchange.invalidNative()
			return err
		}
		if expectedID != "" && state.ID != expectedID {
			exchange.invalidNative()
			return errors.New("provider changed Interaction resource identity")
		}
		if !store && local == nil {
			localID = state.ID
		}
		if object, ok := event.Source.Root().Lookup("interaction"); ok {
			recordGeminiInteractionUsage(exchange, object)
		}
		if state.Done {
			if x.actor != "api_key_resource" && (event.Type == "error" || event.Status == "failed") {
				exchange.complete(classUpstreamServer)
				x.failure = geminiGenerationFailure()
			} else {
				exchange.complete(classSuccess)
			}
		}
		if store && local == nil {
			initial := event.Status
			if initial == "" {
				initial = "created"
			}
			local, err = s.storeGeminiInteraction(ctx, response.exchange, p, authority.ID, state.ID, initial, parent, !state.Done)
			if err != nil {
				return &responseLocalRefusal{cause: err}
			}
			localID = local.ID
		}
		observedStatus := event.Status
		if event.Type == "error" {
			observedStatus = "failed"
		}
		if local != nil && observedStatus != "" {
			object, _ := event.Source.Root().Lookup("interaction")
			commitCtx, stopCommit := resourceCommitContext(ctx)
			_, err := s.observeGeminiInteraction(commitCtx, authority.ID, local.ID, observedStatus, object)
			stopCommit()
			if err != nil {
				return &responseLocalRefusal{cause: err}
			}
		}
		if event.Type == "error" || event.Status == "failed" {
			event.Source, err = fault.RedactNativeFailureDocument(event.Source, x.sensitive)
			if err != nil {
				return err
			}
		}
		projected, err := event.WithProjection(localID, previous, x.route.Slug)
		if err != nil {
			return err
		}
		if local != nil && containsNativeResourceID(projected, state.ID) {
			return errors.New("Interaction event contains an unbound native resource identity")
		}
		var output bytes.Buffer
		if frame.ID != nil {
			output.WriteString("id: ")
			output.WriteString(*frame.ID)
			output.WriteByte('\n')
		}
		if frame.Event != nil {
			output.WriteString("event: ")
			output.WriteString(*frame.Event)
			output.WriteByte('\n')
		}
		if frame.RetryMS != nil {
			output.WriteString("retry: ")
			output.WriteString(strconv.FormatUint(*frame.RetryMS, 10))
			output.WriteByte('\n')
		}
		for line := range bytes.SplitSeq(projected, []byte{'\n'}) {
			output.WriteString("data: ")
			output.Write(line)
			output.WriteByte('\n')
		}
		output.WriteByte('\n')
		if output.Len() > limit {
			return errors.New("projected Interaction event exceeds byte limit")
		}
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
		if !committed {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			x.responseHeaders(w.Header(), true)
			if x.actor != "api_key_resource" {
				w.Header().Set("X-Should-Retry", "false")
			}
			w.WriteHeader(http.StatusOK)
			committed = true
			exchange.delivery(usage.ClientUnobserved)
		}
		if n, err := w.Write(output.Bytes()); err != nil || n != output.Len() {
			if n > 0 {
				exchange.delivery(usage.ClientPartial)
			}
			return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
		}
		if err := controller.Flush(); err != nil {
			return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
		}
		x.delivered(s.now())
		switch {
		case state.Done:
			exchange.delivery(usage.ClientTerminal)
		case event.Type == "step.stop":
			exchange.delivery(usage.ClientActionable)
		default:
			exchange.delivery(usage.ClientPartial)
		}
		return nil
	})
	if err != nil || !state.Done {
		_, local := errors.AsType[*responseLocalRefusal](err)
		switch {
		case errors.Is(err, fault.ErrClientWrite), errors.Is(ctx.Err(), context.Canceled):
			exchange.interrupt(classCancelled, true)
		case errors.Is(ctx.Err(), context.DeadlineExceeded) && !local:
			exchange.interrupt(classTimeout, false)
		default:
			exchange.interrupt(classProtocol, local)
		}
		if err == nil {
			err = errors.New("Interaction SSE ended before terminal event")
		}
		return committed, err
	}
	return committed, nil
}

func geminiGenerationFailure() *fault.Error {
	failure := fault.ServerError(http.StatusBadGateway, "upstream_response_failed", "The provider reported a failed Interaction generation.")
	failure.NoRetry = true
	return failure
}

func recordGeminiInteractionUsage(exchange *pinnedExchange, object oif.Value) {
	if exchange.request.actor == "api_key_resource" {
		return
	}
	observed := geminiInteractionUsage(object)
	if observed == nil {
		return
	}
	fact := exchange.fact()
	statusValue, _ := object.Lookup("status")
	status, _ := statusValue.Text()
	complete := geminilifecycle.InteractionTurnFinished(status)
	fact.ObserveUsage(observed, complete)
	exchange.recordEvidence()
}

func geminiInteractionUsage(object oif.Value) *wire.Usage {
	usageValue, ok := object.Lookup("usage")
	if !ok || usageValue.Kind() != oif.Object {
		return nil
	}
	var native struct {
		Input            *int64                            `json:"total_input_tokens"`
		Output           *int64                            `json:"total_output_tokens"`
		Cached           *int64                            `json:"total_cached_tokens"`
		Thoughts         *int64                            `json:"total_thought_tokens"`
		ToolUse          *int64                            `json:"total_tool_use_tokens"`
		Total            *int64                            `json:"total_tokens"`
		PromptDetails    []geminilifecycle.LiveUsageDetail `json:"prompt_tokens_details"`
		CacheDetails     []geminilifecycle.LiveUsageDetail `json:"cache_tokens_details"`
		CandidateDetails []geminilifecycle.LiveUsageDetail `json:"candidates_tokens_details"`
		ResponseDetails  []geminilifecycle.LiveUsageDetail `json:"response_tokens_details"`
	}
	if json.Unmarshal(usageValue.Bytes(), &native) != nil || native.Input == nil || native.Output == nil || *native.Input < 0 || *native.Output < 0 || native.Cached != nil && (*native.Cached < 0 || *native.Cached > *native.Input) {
		return nil
	}
	usage := geminiExtendedUsage(*native.Input, *native.Output, native.Cached, native.Thoughts, native.ToolUse, native.Total)
	if usage == nil {
		return nil
	}
	if audio, image := geminiUsageDetails(native.PromptDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.AudioInputTokens = &audio
		}
		if image > 0 {
			usage.ImageInputTokens = &image
		}
	}
	if audio, image := geminiUsageDetails(native.CacheDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.CachedAudioInputTokens = &audio
		}
		if image > 0 {
			usage.CachedImageInputTokens = &image
		}
	}
	if audio, image := geminiUsageDetails(native.CandidateDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.AudioOutputTokens = &audio
		}
		if image > 0 {
			usage.ImageOutputTokens = &image
		}
	}
	if audio, image := geminiUsageDetails(native.ResponseDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.AudioOutputTokens = &audio
		}
		if image > 0 {
			usage.ImageOutputTokens = &image
		}
	}
	return usage
}
