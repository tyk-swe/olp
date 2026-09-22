package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/geminilifecycle"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) beginGeminiInteraction(w http.ResponseWriter, r *http.Request, resourceCall bool) (*execution, access.Authority, bool) {
	actor := "api_key"
	if resourceCall {
		actor = "api_key_resource"
	}
	x := &execution{request: s.begin(w, r), family: openai.FamilyGeminiInteractions, actor: actor, mode: "unary"}
	if !s.admit(r.Context()) {
		x.failure = overloaded
		s.finish(x, nil, overloaded.Status)
		writeSurfaceError(w, overloaded, "gemini")
		return x, access.Authority{}, false
	}
	if _, e := geminiClientKey(r, "stream", "last_event_id", "include_input"); e != nil {
		x.failure = e
		s.finish(x, nil, e.Status)
		writeSurfaceError(w, e, "gemini")
		s.release(r.Context())
		return x, access.Authority{}, false
	}
	authority, e := s.authenticate(r, "inference")
	if e == nil {
		x.authority = authority
		x.keyID, x.affinity = authority.ID, []byte(authority.ID)
		x.budgetGroupID = authority.BudgetGroupID
		x.attribution, e = s.parseAttribution(r, authority)
	}
	if e != nil {
		x.failure = e
		s.finish(x, nil, e.Status)
		writeSurfaceError(w, e, "gemini")
		s.release(r.Context())
		return x, access.Authority{}, false
	}
	return x, authority, true
}

func (s *Server) geminiInteractionCreate(w http.ResponseWriter, r *http.Request) {
	x, authority, ok := s.beginGeminiInteraction(w, r, false)
	if !ok {
		return
	}
	defer s.release(r.Context())
	status := http.StatusInternalServerError
	var out *outcome
	var p *pin
	defer func() {
		s.finish(x, out, status)
		if p != nil && p.hold != nil {
			p.hold.settle(r.Context(), x.dispatched, totalTokens(x.usage()))
		}
		settleKey(r.Context(), x.lease, x.dispatched, x.settledTokens(), s.log)
	}()
	fail := func(e *Error) {
		x.failure, status = e, e.Status
		if x.dispatched && len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			if fact.Class == classSuccess {
				fact.Class, fact.BillingUncertain, fact.UsageComplete = classProtocol, true, false
				if fact.Interaction != nil {
					fact.Interaction.UpstreamState = usage.UpstreamTerminal
				}
			}
		}
		if x.dispatched && x.strict() && e.Status >= 500 {
			copy := *e
			copy.NoRetry = true
			e = &copy
		}
		writeSurfaceError(w, e, "gemini")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["last_event_id"]) > 0 || len(query["stream"]) > 0 || len(query["include_input"]) > 0 {
		fail(invalidRequest("invalid_request", "Interaction create does not accept retrieval controls.", nil))
		return
	}
	body, e := s.readBody(r)
	if e != nil {
		fail(e)
		return
	}
	maxBody := int(s.cfg.MaxBodyBytes)
	if maxBody <= 0 {
		maxBody = len(body)
	}
	input, err := geminilifecycle.ParseInteractionRequest(body, maxBody)
	if err != nil || !openai.RouteSlug.MatchString(input.Model) {
		fail(invalidRequest("invalid_interaction_request", "The Gemini Interaction request is malformed.", nil))
		return
	}
	if input.Stream {
		x.mode = "streaming"
	}
	route, found := x.request.release.Snapshot.Routes[input.Model]
	if !found {
		fail(modelNotFound(input.Model))
		return
	}
	x.route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		fail(modelNotFound(input.Model))
		return
	}
	if !slices.Contains(route.Operations, "generation") {
		fail(invalidRequest("invalid_request", "This route does not allow Interactions.", nil))
		return
	}
	if e := policySurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	if input.Store || input.Previous != "" || input.Background {
		if !authority.Policy.AllowProviderState {
			fail(invalidRequest("policy_conflict", "This API key does not allow provider-retained Interaction state.", nil))
			return
		}
		if s.Resources == nil || !s.Resources.Encrypted() || s.Resolver == nil {
			fail(serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Encrypted Interaction state is not configured."))
			return
		}
	}
	if x.preferences, e = routingPreferences(r); e != nil {
		fail(e)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), &route)
	defer cancel()
	x.estimate = max(resourceEstimate, int64(len(body))/4)
	x.lease, e = s.Admission.reserveKey(ctx, authority, x.estimate, time.Duration(route.OverallTimeout)*time.Millisecond)
	if e != nil {
		fail(e)
		return
	}
	var parent *resources.Resource
	previousUpstream := ""
	if input.Previous != "" {
		parent, previousUpstream, err = s.Resources.ReadInteractionContract(ctx, authority.ID, input.Previous)
		if errors.Is(err, resources.ErrNotFound) || parent != nil && parent.RouteSlug != input.Model {
			fail(invalidRequest("invalid_previous_interaction_id", "The previous Interaction is not owned by this key and route.", strPtr("previous_interaction_id")))
			return
		}
		if err != nil {
			fail(serverError(http.StatusConflict, "provider_resource_unavailable", "The previous Interaction cannot be read."))
			return
		}
		var retainedRoute *runtime.Route
		p, retainedRoute, e = s.resolveResource(ctx, x, authority, parent, "generation")
		if e != nil || retainedRoute == nil || retainedRoute.Slug != route.Slug || p.provider.ProfileID != "gemini-interactions" || !p.provider.Supports(p.model, "generation", "gemini", x.mode) {
			fail(pinUnavailable())
			return
		}
		x.pinnedSlot, x.pinnedSecret = &p.slot, p.secret
		deadline, _ := ctx.Deadline()
		gate := s.gateSlot(ctx, &p.provider, &p.slot, x.estimate, deadline)
		if gate.verdict != gateAdmitted {
			fail(gateError(gate))
			return
		}
		p.hold = gate.hold
		x.attempts, x.budget = []runtime.Attempt{p.attempt}, 1
	} else {
		p, e = s.selectPinSurface(ctx, x, &route, "generation", "gemini", x.mode, func(provider *runtime.Provider, model string) bool {
			return provider.ProfileID == "gemini-interactions" && provider.Connector().Supports("generation", "gemini", x.mode) && provider.Supports(model, "generation", "gemini", x.mode)
		})
		if e != nil {
			fail(e)
			return
		}
	}
	endpoint, err := p.provider.Connector().InteractionsURL("", "")
	if err != nil {
		fail(serverError(http.StatusBadGateway, "upstream_error", "The Interactions profile cannot address this model."))
		return
	}
	bound, err := input.Bind(p.provider.Connector().Model(p.model), previousUpstream)
	if err != nil {
		fail(invalidRequest("invalid_interaction_request", "The Interaction identities could not be bound.", nil))
		return
	}
	response, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, bound, "application/json")
	if failure != nil {
		x.dispatched = failure.dispatched
		fail(failure.toError())
		return
	}
	x.dispatched = true
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fail(serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unsupported Interaction status."))
		return
	}
	if input.Stream {
		var committed bool
		var streamErr error
		committed, streamErr = s.streamGeminiInteraction(ctx, w, x, p, response, authority, input.Store, parent, nil, "", false)
		if streamErr != nil {
			x.failure = serverError(http.StatusBadGateway, "interaction_incomplete", "The Interaction stream ended before its terminal contract.")
			status = x.failure.Status
			if !committed {
				writeSurfaceError(w, x.failure, "gemini")
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
	raw, err := readBounded(response.Body, limit)
	if err != nil {
		fail(serverError(http.StatusBadGateway, "upstream_response_too_large", "The Interaction response exceeded its configured limit."))
		return
	}
	result, err := geminilifecycle.ParseInteractionResponse(raw, int(limit))
	if err != nil {
		fail(serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Interaction resource."))
		return
	}
	localID := result.ID
	if input.Store {
		stored, storeErr := s.storeGeminiInteraction(ctx, x, p, authority.ID, result.ID, result.Status, parent)
		if storeErr != nil {
			fail(serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Interaction mapping could not be committed."))
			return
		}
		localID = stored.ID
	}
	projection, err := result.WithProjection(localID, input.Previous, input.Model)
	if err != nil {
		fail(serverError(http.StatusBadGateway, "provider_protocol_error", "The Interaction result contains an unbound resource reference."))
		return
	}
	if input.Store && (bytes.Contains(projection, []byte(result.ID)) || previousUpstream != "" && bytes.Contains(projection, []byte(previousUpstream))) {
		fail(serverError(http.StatusBadGateway, "provider_protocol_error", "The Interaction result contains an unbound native resource identity."))
		return
	}
	if int64(len(projection)) > limit {
		fail(serverError(http.StatusBadGateway, "upstream_response_too_large", "The projected Interaction exceeded its configured limit."))
		return
	}
	s.recordGeminiInteractionUsage(x, result.Source.Root())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	n, writeErr := w.Write(projection)
	if n > 0 {
		x.delivered(s.now())
	}
	if writeErr != nil || n != len(projection) {
		x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The Interaction result could not be delivered.")
		status, out = 0, &outcome{err: x.failure, committed: true, cancelled: true}
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class, fact.Committed = classCancelled, true
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = usage.UpstreamTerminal
				if n > 0 {
					fact.Interaction.ClientState = usage.ClientPartial
				}
			}
		}
		return
	}
	status, out = response.StatusCode, &outcome{committed: true}
	if len(x.facts) > 0 {
		x.facts[len(x.facts)-1].Committed = true
		if x.facts[len(x.facts)-1].Interaction != nil {
			x.facts[len(x.facts)-1].Interaction.UpstreamState = usage.UpstreamTerminal
			x.facts[len(x.facts)-1].Interaction.ClientState = usage.ClientTerminal
		}
	}
}

func (s *Server) storeGeminiInteraction(ctx context.Context, x *execution, p *pin, owner, upstreamID, state string, parent *resources.Resource) (*resources.Resource, error) {
	version := resources.InteractionContract
	expiry := s.now().Add(resources.ContinuationLifetime - time.Minute)
	metadata, _ := json.Marshal(map[string]string{"upstream_model": p.model})
	r := &resources.Resource{Kind: resources.KindInteraction, APIKeyID: owner, RouteSlug: x.route.Slug,
		ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: x.route.RevisionID,
		SlotID: p.slot.ID, CredentialID: p.slot.CredentialID, State: state, Metadata: metadata,
		ContractVersion: &version, ExpiresAt: &expiry}
	if parent != nil {
		r.ParentID = &parent.UUID
	}
	return s.Resources.PutInteractionContract(ctx, r, upstreamID)
}

func (s *Server) geminiInteractionResource(w http.ResponseWriter, r *http.Request) {
	x, authority, ok := s.beginGeminiInteraction(w, r, true)
	if !ok {
		return
	}
	defer s.release(r.Context())
	status := http.StatusInternalServerError
	var out *outcome
	var p *pin
	defer func() {
		s.finish(x, out, status)
		if p != nil && p.hold != nil {
			p.hold.settle(r.Context(), x.dispatched, nil)
		}
		settleKey(r.Context(), x.lease, x.dispatched, x.settledTokens(), s.log)
	}()
	fail := func(e *Error) {
		x.failure, status = e, e.Status
		if x.dispatched && len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			if fact.Class == classSuccess {
				fact.Class = classProtocol
				if fact.Interaction != nil {
					fact.Interaction.UpstreamState = usage.UpstreamTerminal
				}
			}
		}
		writeSurfaceError(w, e, "gemini")
	}
	if s.Resources == nil || !s.Resources.Encrypted() || s.Resolver == nil || !authority.Policy.AllowProviderState {
		fail(permissionError("provider_state_unavailable", "Interaction state is unavailable to this key."))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(invalidRequest("invalid_request", "The Interaction query is malformed.", nil))
		return
	}
	stream := false
	if value, found := query["stream"]; found {
		if value[0] != "true" && value[0] != "false" {
			fail(invalidRequest("invalid_request", "stream must be true or false.", nil))
			return
		}
		stream = value[0] == "true"
	}
	if stream {
		x.mode = "streaming"
	}
	if r.Method != http.MethodGet && (len(query["stream"]) > 0 || len(query["last_event_id"]) > 0 || len(query["include_input"]) > 0) || len(query["last_event_id"]) > 0 && !stream {
		fail(invalidRequest("invalid_request", "Interaction retrieval controls are invalid for this method.", nil))
		return
	}
	if values := query["include_input"]; len(values) > 0 && values[0] != "true" && values[0] != "false" {
		fail(invalidRequest("invalid_request", "include_input must be true or false.", nil))
		return
	}
	if values := query["last_event_id"]; len(values) > 0 && len(values[0]) > 512 {
		fail(invalidRequest("invalid_request", "last_event_id is too long.", nil))
		return
	}
	localID := r.PathValue("id")
	res, upstreamID, err := s.Resources.ReadInteractionContract(r.Context(), authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		fail(notFoundError("interaction_not_found", "This Interaction does not exist for this key."))
		return
	}
	if err != nil {
		fail(serverError(http.StatusConflict, "provider_resource_unavailable", "The Interaction mapping cannot be read."))
		return
	}
	current, found := x.request.release.Snapshot.Routes[res.RouteSlug]
	if !found || !authority.Allows("inference", current.Slug, current.ProjectID, s.now()) {
		fail(notFoundError("interaction_not_found", "This Interaction does not exist for this key."))
		return
	}
	x.route = &current
	if e := policySurfaceGate(&current); e != nil {
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
	var retained *runtime.Route
	var e *Error
	p, retained, e = s.resolveResource(ctx, x, authority, res, "generation")
	if e != nil || retained == nil || retained.Slug != current.Slug || p.provider.ProfileID != "gemini-interactions" || !p.provider.Supports(p.model, "generation", "gemini", x.mode) {
		fail(pinUnavailable())
		return
	}
	x.pinnedSlot, x.pinnedSecret = &p.slot, p.secret
	deadline, _ := ctx.Deadline()
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, deadline)
	if gate.verdict != gateAdmitted {
		fail(gateError(gate))
		return
	}
	p.hold = gate.hold
	method, action := r.Method, ""
	if r.Method == http.MethodPost {
		if !strings.HasSuffix(r.URL.Path, "/cancel") {
			fail(notFoundError("not_found", "Unknown Interaction action."))
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
	if failure != nil {
		x.dispatched = failure.dispatched
		// A prior successful DELETE may have lost its response. A provider 404
		// still permits deleting the gateway's now-stale local mapping.
		if method == http.MethodDelete && failure.status == http.StatusNotFound {
			if err := s.Resources.DeleteInteractionContract(ctx, authority.ID, localID); err == nil {
				w.WriteHeader(http.StatusOK)
				if len(x.facts) > 0 {
					x.facts[len(x.facts)-1].Committed = true
				}
				status, out = http.StatusOK, &outcome{committed: true}
				return
			}
		}
		fail(failure.toError())
		return
	}
	x.dispatched = true
	defer response.Body.Close()
	if method != http.MethodDelete && response.StatusCode != http.StatusOK {
		fail(serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unsupported Interaction status."))
		return
	}
	if method == http.MethodDelete {
		if err := s.Resources.DeleteInteractionContract(ctx, authority.ID, localID); err != nil {
			fail(serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The deleted Interaction could not be tombstoned."))
			return
		}
		w.WriteHeader(http.StatusOK)
		if len(x.facts) > 0 {
			x.facts[len(x.facts)-1].Committed = true
		}
		status, out = http.StatusOK, &outcome{committed: true}
		return
	}
	if stream {
		committed, streamErr := s.streamGeminiInteraction(ctx, w, x, p, response, authority, true, nil, res, upstreamID, len(query["last_event_id"]) > 0)
		if streamErr != nil {
			x.failure = serverError(http.StatusBadGateway, "interaction_incomplete", "The Interaction retrieval stream ended before its terminal contract.")
			status = x.failure.Status
			if !committed {
				writeSurfaceError(w, x.failure, "gemini")
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
	raw, err := readBounded(response.Body, limit)
	if err != nil {
		fail(serverError(http.StatusBadGateway, "upstream_response_too_large", "The Interaction response exceeded its configured limit."))
		return
	}
	result, err := geminilifecycle.ParseInteractionResponse(raw, int(limit))
	if err != nil || result.ID != upstreamID {
		fail(serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid or different Interaction."))
		return
	}
	parentID := ""
	if res.ParentID != nil {
		parentID = resources.LocalID(resources.KindInteraction, *res.ParentID)
	}
	projection, err := result.WithProjection(localID, parentID, res.RouteSlug)
	if err != nil {
		fail(serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unbound Interaction reference."))
		return
	}
	if bytes.Contains(projection, []byte(upstreamID)) {
		fail(serverError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unbound native resource identity."))
		return
	}
	if int64(len(projection)) > limit {
		fail(serverError(http.StatusBadGateway, "upstream_response_too_large", "The projected Interaction exceeded its configured limit."))
		return
	}
	if err := s.Resources.MarkInteractionStatus(ctx, authority.ID, localID, result.Status); err != nil {
		fail(serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Interaction state could not be updated."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	if len(x.facts) > 0 {
		x.facts[len(x.facts)-1].Committed = true
	}
	n, writeErr := w.Write(projection)
	if n > 0 {
		x.delivered(s.now())
	}
	if writeErr != nil || n != len(projection) {
		x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The Interaction resource could not be delivered.")
		status, out = 0, &outcome{err: x.failure, committed: true, cancelled: true}
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class = classCancelled
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = usage.UpstreamTerminal
				if n > 0 {
					fact.Interaction.ClientState = usage.ClientPartial
				}
			}
		}
		return
	}
	status, out = response.StatusCode, &outcome{committed: true}
}

func (s *Server) streamGeminiInteraction(ctx context.Context, w http.ResponseWriter, x *execution, p *pin, response *http.Response, authority access.Authority, store bool, parent, existing *resources.Resource, expectedID string, resumed bool) (bool, error) {
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
	err = sse.Decode(response.Body, limit, func(frame sse.Frame) error {
		event, err := geminilifecycle.ParseInteractionEvent([]byte(frame.Data), limit)
		if err != nil {
			return err
		}
		if err := state.Accept(event); err != nil {
			return err
		}
		if expectedID != "" && state.ID != expectedID {
			return errors.New("provider changed Interaction resource identity")
		}
		if !store && local == nil {
			localID = state.ID
		}
		if store && local == nil {
			initial := event.Status
			if initial == "" {
				initial = "created"
			}
			local, err = s.storeGeminiInteraction(ctx, x, p, authority.ID, state.ID, initial, parent)
			if err != nil {
				return err
			}
			localID = local.ID
		}
		if local != nil && event.Status != "" {
			if err := s.Resources.MarkInteractionStatus(ctx, authority.ID, local.ID, event.Status); err != nil {
				return err
			}
		}
		projected, err := event.WithProjection(localID, previous, x.route.Slug)
		if err != nil {
			return err
		}
		if local != nil && bytes.Contains(projected, []byte(state.ID)) {
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
		if !committed {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			committed = true
			if len(x.facts) > 0 {
				x.facts[len(x.facts)-1].Committed = true
			}
		}
		if _, err := w.Write(output.Bytes()); err != nil {
			return err
		}
		x.delivered(s.now())
		if f := &x.facts[len(x.facts)-1]; f.Interaction != nil {
			f.Interaction.UpstreamState = usage.UpstreamAccepted
			f.Interaction.ClientState = usage.ClientPartial
			if event.Type == "step.stop" {
				f.Interaction.ClientState = usage.ClientActionable
			}
			if state.Done {
				f.Interaction.UpstreamState, f.Interaction.ClientState = usage.UpstreamTerminal, usage.ClientTerminal
			}
		}
		if event.Type == "interaction.completed" {
			if object, ok := event.Source.Root().Lookup("interaction"); ok {
				s.recordGeminiInteractionUsage(x, object)
			}
		}
		return http.NewResponseController(w).Flush()
	})
	if err != nil || !state.Done {
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class, fact.BillingUncertain = classProtocol, true
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = usage.UpstreamUnknown
			}
		}
		if err == nil {
			err = errors.New("Interaction SSE ended before terminal event")
		}
		return committed, err
	}
	return committed, nil
}

func (s *Server) recordGeminiInteractionUsage(x *execution, object oif.Value) {
	if len(x.facts) == 0 || x.actor == "api_key_resource" {
		return
	}
	usageValue, ok := object.Lookup("usage")
	if !ok || usageValue.Kind() != oif.Object {
		return
	}
	var native struct {
		Input  *int64 `json:"total_input_tokens"`
		Output *int64 `json:"total_output_tokens"`
		Cached *int64 `json:"total_cached_tokens"`
	}
	if json.Unmarshal(usageValue.Bytes(), &native) != nil || native.Input == nil || native.Output == nil || *native.Input < 0 || *native.Output < 0 || native.Cached != nil && (*native.Cached < 0 || *native.Cached > *native.Input) {
		return
	}
	fact := &x.facts[len(x.facts)-1]
	fact.Usage = &openai.Usage{InputTokens: *native.Input, OutputTokens: *native.Output}
	if native.Cached != nil {
		fact.Usage.CachedInputTokens = native.Cached
	}
	fact.recordEvidence(false)
}
