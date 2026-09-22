package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

func responsePath(cfg connectors.Config, suffix string) string {
	if cfg.Kind == "azure_openai" && cfg.Hosting() != "azure-v1" && cfg.Hosting() != "azure-responses-legacy" {
		return "deployments/responses" + suffix
	}
	return "responses" + suffix
}

func providerStateForbidden() *Error {
	return permissionError("provider_state_forbidden", "This API key does not allow provider state; provider-retained content requires the allow_provider_state policy.")
}

func (s *Server) responsesStateGate(ctx context.Context, x *execution, authority access.Authority, parsed *openai.Request) *Error {
	var previous string
	if raw := parsed.Field("previous_response_id"); raw != nil {
		_ = json.Unmarshal(raw, &previous)
	}
	var background, store bool
	if raw := parsed.Field("background"); raw != nil {
		_ = json.Unmarshal(raw, &background)
	}
	if raw := parsed.Field("store"); raw != nil {
		_ = json.Unmarshal(raw, &store)
	}
	if route, ok := x.request.release.Snapshot.Routes[parsed.Route]; ok && runtime.FidelityMode(route.Fidelity) == runtime.FidelityStrict && parsed.Field("store") == nil {
		// Native Responses omission requests provider retention. Strict admission
		// cannot silently inject store:false to avoid the caller's state policy.
		store = true
	}
	if previous == "" && !background && !store {
		return nil
	}
	if !authority.Policy.AllowProviderState {
		if previous != "" {
			param := "previous_response_id"
			return invalidRequest("unsupported_stateful_reference", "The gateway does not hold prior responses; previous_response_id is not supported.", &param)
		}
		if background {
			param := "background"
			return invalidRequest("unsupported_parameter", "Background responses require stateful polling, which the gateway does not provide.", &param)
		}
		param := "store"
		return invalidRequest("unsupported_parameter", "Storing responses at the provider requires the allow_provider_state policy.", &param)
	}
	if s.Resources == nil {
		return serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider state is not configured on this installation.")
	}
	x.providerState = true
	if previous == "" {
		return nil
	}
	res, err := s.Resources.Get(ctx, resources.KindResponse, authority.ID, previous)
	if errors.Is(err, resources.ErrNotFound) {
		param := "previous_response_id"
		return invalidRequest("invalid_previous_response_id", "previous_response_id must name a stored response owned by this key.", &param)
	}
	if err != nil {
		return serverError(http.StatusInternalServerError, "internal_error", "The stored response could not be read.")
	}
	if res.RouteSlug != parsed.Route {
		param := "previous_response_id"
		return invalidRequest("invalid_previous_response_id", "previous_response_id must reference a response created under this model.", &param)
	}
	x.pin = res
	x.providerState = true
	encoded, err := json.Marshal(res.UpstreamID)
	if err != nil {
		return serverError(http.StatusInternalServerError, "internal_error", "The stored response could not be read.")
	}
	parsed.SetField("previous_response_id", encoded)
	return nil
}

func (s *Server) pinAttempts(ctx context.Context, x *execution) *Error {
	if x.pin == nil {
		return nil
	}
	p, _, e := s.resolveResource(ctx, x, x.authority, x.pin, x.family.Operation())
	if e != nil {
		return e
	}
	if !p.provider.Supports(p.model, x.family.Operation(), x.family.Surface(), x.mode) {
		return serverError(http.StatusConflict, "provider_resource_credential_unavailable",
			"The provider that owns this object can no longer serve this operation.")
	}
	x.attempts = []runtime.Attempt{p.attempt}
	x.budget = 1
	x.pinnedSlot = &p.slot
	x.pinnedSecret = p.secret
	return nil
}

func responseStoreRequested(parsed *openai.Request) bool {
	raw := parsed.Field("store")
	if raw == nil {
		return true
	}
	var store bool
	return json.Unmarshal(raw, &store) == nil && store
}

func (s *Server) mapStoredResponse(ctx context.Context, x *execution, authority access.Authority, body []byte) ([]byte, *Error) {
	if s.Resources == nil || len(x.facts) == 0 || !responseStoreRequested(x.parsed) {
		return body, nil
	}
	upstreamID, ok := upstreamString(body, "id")
	if !ok || len(upstreamID) > 512 {
		return nil, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed response object.")
	}
	fact := x.facts[len(x.facts)-1]
	var credential *string
	if fact.CredentialID != "" {
		credential = &fact.CredentialID
	}
	state := "created"
	if status, ok := upstreamString(body, "status"); ok {
		state = status
	}
	metadata := map[string]json.RawMessage{}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) == nil {
		for _, name := range []string{"object", "status", "status_details", "created_at", "expires_at"} {
			if raw, present := obj[name]; present {
				metadata[name] = raw
			}
		}
	}
	if fact.UpstreamModel != "" {
		encoded, _ := json.Marshal(fact.UpstreamModel)
		metadata["upstream_model"] = encoded
	}
	var expires *time.Time
	if raw, present := metadata["expires_at"]; present {
		var seconds int64
		if json.Unmarshal(raw, &seconds) == nil && seconds > 0 {
			at := time.Unix(seconds, 0).UTC()
			expires = &at
		}
	}
	deferred := s.pendingResponseUsage(x, &fact, metadata)
	encoded, _ := json.Marshal(metadata)
	res, err := s.Resources.GetByUpstream(ctx, resources.KindResponse, authority.ID, fact.ProviderID, upstreamID)
	if errors.Is(err, resources.ErrNotFound) {
		res, err = s.Resources.Put(ctx, &resources.Resource{
			Kind:               resources.KindResponse,
			APIKeyID:           authority.ID,
			RouteSlug:          x.route.Slug,
			ProviderID:         fact.ProviderID,
			ProviderRevisionID: fact.ProviderRevisionID,
			RouteRevisionID:    x.route.RevisionID,
			SlotID:             fact.SlotID,
			CredentialID:       credential,
			UpstreamID:         upstreamID,
			State:              state,
			Metadata:           encoded,
			ExpiresAt:          expires,
		})
	}
	if err != nil {
		return nil, serverError(http.StatusInternalServerError, "internal_error", "The stored response mapping could not be recorded.")
	}
	if deferred {
		last := &x.facts[len(x.facts)-1]
		last.ResponseUsageDeferred = true
		last.recordEvidence(false)
		if e := s.reconcileResponse(ctx, res.ID, body); e != nil {
			return nil, e
		}
	}
	out, err := rewriteID(body, "id", res.ID)
	if err != nil {
		return nil, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed response object.")
	}
	return out, nil
}

func responseUsage(body []byte) *openai.Usage {
	var obj struct {
		Usage *struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &obj) != nil || obj.Usage == nil {
		return nil
	}
	usage := &openai.Usage{InputTokens: obj.Usage.InputTokens, OutputTokens: obj.Usage.OutputTokens}
	if obj.Usage.InputTokensDetails != nil && obj.Usage.InputTokensDetails.CachedTokens >= 0 {
		cached := obj.Usage.InputTokensDetails.CachedTokens
		usage.CachedInputTokens = &cached
	}
	return usage
}

func (s *Server) responseCall(w http.ResponseWriter, r *http.Request, op func(context.Context, *execution, access.Authority, *resources.Resource, *pin, *runtime.Route) *Error) {
	x, authority, done := s.stateBegin(w, r, openai.FamilyResponses)
	if done {
		return
	}
	defer s.release(r.Context())
	if !authority.Policy.AllowProviderState {
		s.stateFail(x, w, providerStateForbidden(), x.family)
		return
	}
	res, err := s.Resources.Get(r.Context(), resources.KindResponse, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No stored response with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The stored response could not be read."), x.family)
		return
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, res, "generation")
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond))
	if gate.verdict != gateAdmitted {
		s.stateFail(x, w, gateError(gate), x.family)
		return
	}
	p.hold = gate.hold
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.stateFail(x, w, e, x.family)
	}
}

func (s *Server) responseUpstream(ctx context.Context, x *execution, res *resources.Resource, p *pin, method, suffix string, body []byte, w http.ResponseWriter) *Error {
	endpoint, err := resourceURL(p.provider.Connector(), p.model, responsePath(p.provider.Connector(), suffix), nil)
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.pinnedDo(ctx, x, p, method, endpoint, body, "application/json")
	if failure != nil {
		if failure.status == http.StatusNotFound {
			return notFoundError("not_found", "No stored response with this identifier exists for this key.")
		}
		return upstreamError(failure)
	}
	defer resp.Body.Close()
	x.dispatched = true
	result, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	if e := s.reconcileResponse(ctx, res.ID, result); e != nil {
		return e
	}
	if status, ok := upstreamString(result, "status"); ok {
		_ = s.Resources.Update(ctx, res.ID, status, nil, nil)
	}
	out, err := rewriteID(result, "id", res.ID)
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed response object.")
	}
	s.writeStateJSON(w, x, out)
	return nil
}

func (s *Server) getResponse(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, func(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, p *pin, route *runtime.Route) *Error {
		return s.responseUpstream(ctx, x, res, p, http.MethodGet, "/"+url.PathEscape(res.UpstreamID), nil, w)
	})
}

func (s *Server) cancelResponse(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, func(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, p *pin, route *runtime.Route) *Error {
		return s.responseUpstream(ctx, x, res, p, http.MethodPost, "/"+url.PathEscape(res.UpstreamID)+"/cancel", []byte(`{}`), w)
	})
}

func (s *Server) deleteResponse(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, func(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, p *pin, route *runtime.Route) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, responsePath(p.provider.Connector(), "/"+url.PathEscape(res.UpstreamID)), nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.Resources.Tombstone(ctx, res.ID)
				return notFoundError("not_found", "No stored response with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		x.dispatched = true
		result, _ := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		if err := s.Resources.Tombstone(ctx, res.ID); err != nil {
			return serverError(http.StatusInternalServerError, "internal_error", "The response mapping could not be deleted.")
		}
		out, err := rewriteID(result, "id", res.ID)
		if err != nil {
			encoded, _ := json.Marshal(res.ID)
			out, _ = json.Marshal(map[string]json.RawMessage{
				"id": encoded, "object": json.RawMessage(`"response"`), "deleted": json.RawMessage(`true`),
			})
		}
		s.writeStateJSON(w, x, out)
		return nil
	})
}

func (s *Server) responseInputItems(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, func(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, p *pin, route *runtime.Route) *Error {
		query := url.Values{}
		for _, name := range []string{"limit", "after", "order"} {
			if value := r.URL.Query().Get(name); value != "" {
				query.Set(name, value)
			}
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, responsePath(p.provider.Connector(), "/"+url.PathEscape(res.UpstreamID)+"/input_items"), query)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				return notFoundError("not_found", "No stored response with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		x.dispatched = true
		result, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		s.writeStateJSON(w, x, result)
		return nil
	})
}

func responsesRouteEligible(p *runtime.Provider, model string) bool {
	return stateQualified(p, model, "generation", "unary")
}

var errResponseMapping = errors.New("stored response mapping failed")

func (s *Server) mapStreamResponseFrame(ctx context.Context, x *execution, fact *AttemptFact, frame []byte) ([]byte, error) {
	if s.Resources == nil || !responseStoreRequested(x.parsed) {
		return frame, nil
	}
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 {
		return frame, nil
	}
	payload := bytes.TrimSpace(frame[i+7:])
	if len(payload) == 0 || payload[0] != '{' {
		return frame, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return frame, nil
	}
	raw, ok := fields["response"]
	if !ok {
		return frame, nil
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil || len(response) == 0 {
		return frame, nil
	}
	var upstreamID string
	if json.Unmarshal(response["id"], &upstreamID) != nil || upstreamID == "" || len(upstreamID) > 512 ||
		strings.HasPrefix(upstreamID, resources.KindResponse+"_") {
		return frame, nil
	}
	if x.responseMap == nil {
		x.responseMap = map[string]string{}
	}
	local, ok := x.responseMap[upstreamID]
	if !ok {
		res, err := s.Resources.GetByUpstream(ctx, resources.KindResponse, x.keyID, fact.ProviderID, upstreamID)
		if errors.Is(err, resources.ErrNotFound) {
			res, err = s.putStreamResponse(ctx, x, fact, upstreamID, response)
		}
		if err != nil {
			return nil, errResponseMapping
		}
		local = res.ID
		fact.ResponseUsageDeferred = backgroundResponseRequested(x.parsed)
		x.responseMap[upstreamID] = local
	}
	if fact.ResponseUsageDeferred {
		if e := s.reconcileResponse(ctx, local, raw); e != nil {
			return nil, errResponseMapping
		}
	}
	encoded, err := json.Marshal(local)
	if err != nil {
		return nil, errResponseMapping
	}
	response["id"] = encoded
	fields["response"], err = json.Marshal(response)
	if err != nil {
		return nil, errResponseMapping
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, errResponseMapping
	}
	out := make([]byte, 0, i+7+len(body)+2)
	out = append(out, frame[:i+7]...)
	out = append(out, body...)
	out = append(out, '\n', '\n')
	return out, nil
}

func (s *Server) putStreamResponse(ctx context.Context, x *execution, fact *AttemptFact, upstreamID string, response map[string]json.RawMessage) (*resources.Resource, error) {
	metadata := map[string]json.RawMessage{}
	for _, name := range []string{"object", "status", "status_details", "created_at", "expires_at"} {
		if raw, present := response[name]; present {
			metadata[name] = raw
		}
	}
	if fact.UpstreamModel != "" {
		encoded, _ := json.Marshal(fact.UpstreamModel)
		metadata["upstream_model"] = encoded
	}
	state := "created"
	if raw, present := response["status"]; present {
		var status string
		if json.Unmarshal(raw, &status) == nil && status != "" {
			state = status
		}
	}
	var expires *time.Time
	if raw, present := response["expires_at"]; present {
		var seconds int64
		if json.Unmarshal(raw, &seconds) == nil && seconds > 0 {
			at := time.Unix(seconds, 0).UTC()
			expires = &at
		}
	}
	var credential *string
	if fact.CredentialID != "" {
		credential = &fact.CredentialID
	}
	deferred := s.pendingResponseUsage(x, fact, metadata)
	encoded, _ := json.Marshal(metadata)
	res, err := s.Resources.Put(ctx, &resources.Resource{
		Kind:               resources.KindResponse,
		APIKeyID:           x.keyID,
		RouteSlug:          x.route.Slug,
		ProviderID:         fact.ProviderID,
		ProviderRevisionID: fact.ProviderRevisionID,
		RouteRevisionID:    x.route.RevisionID,
		SlotID:             fact.SlotID,
		CredentialID:       credential,
		UpstreamID:         upstreamID,
		State:              state,
		Metadata:           encoded,
		ExpiresAt:          expires,
	})
	if err == nil && deferred {
		fact.ResponseUsageDeferred = true
	}
	return res, err
}
