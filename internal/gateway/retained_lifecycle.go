package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

func (s *Server) registerRetainedLifecycle(mux *http.ServeMux) {
	for _, endpoint := range retainedapi.Endpoints() {
		if !retainedapi.Supports("gemini", endpoint.Operation, endpoint.Surface, "unary") {
			continue
		}
		mux.HandleFunc(endpoint.Method+" "+endpoint.Pattern, func(w http.ResponseWriter, r *http.Request) { s.retainedLifecycle(w, r, endpoint) })
	}
}

func (s *Server) retainedLifecycle(w http.ResponseWriter, r *http.Request, e retainedapi.Endpoint) {
	x, authority, done := s.stateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.release(r.Context())
	x.operationOverride = e.Operation
	x.mode = "unary"
	x.actor = "api_key_resource"
	var p *pin
	status := http.StatusInternalServerError
	defer func() { s.finish(x, nil, status); s.resourceSettle(r.Context(), x, p) }()
	fail := func(err *Error) {
		if e.Action == "create" {
			copy := *err
			copy.NoRetry = true
			err = &copy
		}
		x.failure = err
		status = err.Status
		writeSurfaceError(w, err, e.Surface)
	}
	if !authority.Policy.AllowProviderState {
		fail(providerStateForbidden())
		return
	}
	if e.Action != "list" && len(r.URL.Query()) != 0 {
		fail(invalidRequest("unsupported_parameter", "This resource method does not accept query parameters.", nil))
		return
	}
	if e.Action == "list" {
		status = s.listRetainedLifecycle(w, r, x, authority, e, fail)
		return
	}
	var body []byte
	var source oif.Document
	if e.Method == http.MethodPost || e.Method == http.MethodPatch {
		var err *Error
		body, err = s.readBody(r)
		if err != nil {
			fail(err)
			return
		}
		var parseErr error
		source, parseErr = oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.cfg.MaxBodyBytes)})
		if parseErr != nil || source.Root().Kind() != oif.Object {
			fail(invalidRequest("invalid_request", "A valid JSON object is required.", nil))
			return
		}
		if err = retainedAgentInput(source); err != nil {
			fail(err)
			return
		}
		source, parseErr = normalizeRetainedAgent(source)
		if parseErr != nil {
			fail(invalidRequest("invalid_request", "The agent definition could not be normalized.", nil))
			return
		}
	}
	var route *routing.Route
	var resource *resources.Resource
	var err *Error
	var idempotency string
	var digest []byte
	if e.Action == "create" {
		slug := strings.TrimSpace(r.Header.Get(stateRouteHeader))
		if slug == "" {
			fail(invalidRequest("missing_required_parameter", "X-OLP-Route is required.", nil))
			return
		}
		idempotency = r.Header.Get("Idempotency-Key")
		if idempotency == "" {
			fail(invalidRequest("missing_required_parameter", "Idempotency-Key is required for resource creation.", nil))
			return
		}
		if len(idempotency) > 128 {
			fail(invalidRequest("invalid_idempotency_key", "Idempotency-Key must be at most 128 bytes.", nil))
			return
		}
		digest, err = s.retainedSubmissionDigest(e, slug, body)
		if err != nil {
			fail(err)
			return
		}
		prior, lookupErr := s.Resources.LookupLifecycle(r.Context(), x.keyID, e.Operation, idempotency, digest)
		if errors.Is(lookupErr, resources.ErrSubmissionConflict) {
			fail(serverError(409, "idempotency_conflict", "This submission identity is already bound to another request."))
			return
		}
		if lookupErr == nil {
			// Recovery reveals only an already-owned identity and starts no work.
			x.route = &routing.Route{Slug: prior.RouteSlug, RevisionID: prior.RouteRevisionID}
			w.Header().Set("X-OLP-Resource-ID", prior.ID)
			fail(serverError(409, "submission_exists", "This submission already exists; inspect X-OLP-Resource-ID instead of recreating it."))
			return
		}
		if !errors.Is(lookupErr, resources.ErrNotFound) {
			fail(serverError(503, "provider_state_unavailable", "The submission could not be read."))
			return
		}
		candidate, ok := x.snapshot().Routes[slug]
		if !ok {
			fail(modelNotFound(slug))
			return
		}
		route = &candidate
		x.route = route
		if !route.Fidelity.Strict() {
			fail(policyUnavailable("strict_route_required", "Native agent resources require a strict route."))
			return
		}
		if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
			fail(permissionError("route_forbidden", "The API key cannot use this route."))
			return
		}
		if !slices.Contains(route.Operations, e.Operation) {
			fail(policyUnavailable("operation_unavailable", "The route does not allow this operation."))
			return
		}
		if err = x.guardrailSurfaceGate(route); err != nil {
			fail(err)
			return
		}
		var base *resources.Resource
		if value, present := source.Root().Lookup("base_agent"); present && value.Kind() != oif.Null {
			id, _ := value.Text()
			var getErr error
			base, getErr = s.Resources.Get(r.Context(), "agent_resource", x.keyID, id)
			if getErr != nil || base.RouteSlug != route.Slug || base.RouteRevisionID != route.RevisionID || base.State == "submitting" || base.State == "submission_unknown" || base.ContractVersion == nil || *base.ContractVersion != retainedapi.Revision {
				fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
				return
			}
		}
		p, err = s.selectBoundPinSurface(r.Context(), x, route, e.Operation, e.Surface, "unary", base, func(provider *routing.Provider, model string) bool {
			return provider.Connector().Supports(e.Operation, e.Surface, "unary") && provider.Supports(model, e.Operation, e.Surface, "unary")
		})
		if err != nil {
			fail(err)
			return
		}
		refs := map[string]string{"base_agent": "agent_resource"}
		source, parseErr := retainedapi.Rewrite(source, refs, func(kind, id string) (string, error) {
			bound, getErr := s.Resources.Get(r.Context(), kind, x.keyID, id)
			if getErr != nil || !retainedAffinity(bound, p, route) || bound.State == "submitting" || bound.State == "submission_unknown" {
				return "", resources.ErrNotFound
			}
			return bound.UpstreamID, nil
		})
		if parseErr != nil {
			fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
			return
		}
		body = source.Bytes()
	} else {
		var getErr error
		resource, getErr = s.Resources.Get(r.Context(), e.Kind, x.keyID, r.PathValue("id"))
		if getErr != nil {
			fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
		if resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
			fail(pinUnavailable())
			return
		}
		use := retainedRetrieval
		if e.Action == "delete" {
			use = retainedHousekeeping
		}
		p, route, err = s.resolveResource(r.Context(), x, authority, resource, e.Operation, use)
		if err != nil {
			fail(err)
			return
		}
		x.route = route
		if !p.provider.Connector().Supports(e.Operation, e.Surface, "unary") {
			fail(pinUnavailable())
			return
		}
		if e.Action != "delete" {
			if err = x.guardrailSurfaceGate(route); err != nil {
				fail(err)
				return
			}
		}
		if resource.State == "submitting" || resource.State == "submission_unknown" {
			fail(serverError(409, "submission_unknown", "Creation has not been reconciled; this request cannot be resubmitted."))
			return
		}
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	if err := s.retainedControlPricing(x, p, route); err != nil {
		fail(err)
		return
	}
	if err = s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
		fail(err)
		return
	}
	if err = s.gateRetainedSlot(ctx, x, p, route); err != nil {
		fail(err)
		return
	}
	path := "agents"
	if resource != nil {
		path += "/" + url.PathEscape(resource.UpstreamID)
	}
	endpoint, urlErr := p.provider.Connector().ResourceURL(p.model, path, nil)
	if urlErr != nil {
		fail(serverError(502, "provider_protocol_error", "The retained endpoint is unavailable."))
		return
	}
	if e.Action == "create" {
		meta, _ := json.Marshal(map[string]string{"upstream_model": p.model, "contract_revision": retainedapi.Revision})
		version := retainedapi.Revision
		candidate := &resources.Resource{Kind: e.Kind, APIKeyID: x.keyID, RouteSlug: route.Slug, ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: route.RevisionID, TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID, Metadata: meta, ContractVersion: &version}
		var fresh bool
		var prepareErr error
		resource, fresh, prepareErr = s.Resources.PrepareLifecycle(ctx, candidate, e.Operation, idempotency, digest)
		if errors.Is(prepareErr, resources.ErrSubmissionConflict) {
			fail(serverError(409, "idempotency_conflict", "This submission identity is already bound to another request."))
			return
		}
		if prepareErr != nil {
			fail(serverError(503, "provider_state_unavailable", "The submission could not be persisted."))
			return
		}
		w.Header().Set("X-OLP-Resource-ID", resource.ID)
		if !fresh {
			fail(serverError(409, "submission_exists", "This submission already exists; inspect X-OLP-Resource-ID instead of recreating it."))
			return
		}
		if prepareErr = s.Resources.DispatchLifecycle(ctx, resource.UUID); prepareErr != nil {
			fail(serverError(503, "provider_state_unavailable", "The submission could not be fenced."))
			return
		}
		defer func() {
			commitCtx, cancel := resourceCommitContext(r.Context())
			defer cancel()
			_ = s.Resources.UnknownLifecycle(commitCtx, resource.UUID)
		}()
	}
	native, failure := s.pinnedDo(ctx, x, p, e.Method, endpoint, body, "application/json")
	if failure != nil {
		if resource != nil && e.Action != "create" && failure.status == http.StatusNotFound {
			commitCtx, cancel := resourceCommitContext(ctx)
			defer cancel()
			if removeErr := s.Resources.Tombstone(commitCtx, resource.ID); removeErr != nil {
				fail(serverError(503, "provider_state_unavailable", "The deleted resource could not be reconciled."))
				return
			}
			fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
		fail(retainedPublicError(upstreamError(failure), p, resource))
		return
	}
	defer native.Body.Close()
	response, readErr := readBounded(native.Body, s.retainedResponseLimit())
	if readErr != nil {
		fail(serverError(502, "provider_protocol_error", "The provider response exceeded the supported resource contract."))
		return
	}
	if native.StatusCode < 200 || native.StatusCode >= 300 {
		fail(serverError(502, "provider_protocol_error", "The provider returned an unsupported resource response."))
		return
	}
	commitCtx, commitCancel := resourceCommitContext(ctx)
	defer commitCancel()
	if e.Action == "delete" {
		if putErr := s.Resources.Tombstone(commitCtx, resource.ID); putErr != nil {
			fail(serverError(503, "provider_state_unavailable", "Deletion was accepted but its local record could not be updated."))
			return
		}
		// Gemini's native delete result is an empty JSON message.
		status = native.StatusCode
		x.completePinned()
		body := []byte(`{}`)
		if status == http.StatusNoContent {
			body = nil
		}
		s.writeStateJSONStatus(w, x, body, status)
		return
	}
	nativeID, validID := nativeResourceID(response, retainedBodyLimit)
	if !validID {
		fail(serverError(502, "provider_protocol_error", "The provider response omitted a valid resource identity."))
		return
	}
	if e.Action == "create" {
		resource.UpstreamID = nativeID
		resource.State = "active"
		if putErr := s.Resources.CompleteLifecycle(commitCtx, resource); putErr != nil {
			fail(serverError(503, "provider_state_unavailable", "The provider accepted creation but its resource identity could not be committed."))
			return
		}
	} else if nativeID != resource.UpstreamID {
		fail(serverError(502, "provider_protocol_error", "The provider returned a mismatched resource identity."))
		return
	}
	projected, projectErr := s.projectRetainedAgent(ctx, x, resource, response)
	if projectErr != nil {
		fail(serverError(502, "provider_protocol_error", "The provider response contained an unbound resource identity."))
		return
	}
	status = native.StatusCode
	x.completePinned()
	s.writeStateJSONStatus(w, x, projected, status)
}

func (s *Server) retainedSubmissionDigest(endpoint retainedapi.Endpoint, slug string, body []byte) ([]byte, *Error) {
	digester, ok := s.Runtime.(interface {
		Digest(secrets.DigestPurpose, string) []byte
	})
	if !ok {
		return nil, serverError(503, "authority_unavailable", "Resource submission identity is unavailable.")
	}
	canonical, err := canonicalBatchJSON(body)
	if err != nil {
		return nil, invalidRequest("invalid_request", "A valid JSON object is required.", nil)
	}
	return digester.Digest(secrets.RetainedSubmissionDigest, endpoint.Operation+"\x00"+endpoint.Kind+"\x00"+slug+"\x00"+string(canonical)), nil
}

func retainedAffinity(resource *resources.Resource, p *pin, route *routing.Route) bool {
	return resource != nil && resource.RouteSlug == route.Slug && resource.RouteRevisionID == route.RevisionID && resource.ProviderID == p.provider.ID && resource.ProviderRevisionID == p.provider.RevisionID && resource.TargetID == p.target.ID && resource.SlotID == p.slot.ID && sameCredential(resource.CredentialID, p.slot.CredentialID)
}
func sameCredential(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (s *Server) retainedResponseLimit() int64 {
	if s.cfg.MaxResponseBytes > 0 && s.cfg.MaxResponseBytes < retainedBodyLimit {
		return s.cfg.MaxResponseBytes
	}
	return retainedBodyLimit
}

func retainedAgentInput(source oif.Document) *Error {
	if err := retainedAgentEnvironment(source); err != nil {
		return err
	}
	for _, field := range source.Root().Members() {
		if !slices.Contains([]string{"agent_config", "base_agent", "base_environment", "description", "system_instruction", "tools"}, field.Name) {
			return invalidRequest("unsupported_field", "The agent contract does not support field "+field.Name+".", nil)
		}
		if field.Value.Kind() == oif.Null {
			continue
		}
		expected := oif.String
		switch field.Name {
		case "agent_config", "base_environment":
			expected = oif.Object
		case "tools":
			expected = oif.Array
		}
		if field.Value.Kind() != expected {
			return invalidRequest("invalid_parameter", "Invalid value type for agent field: "+field.Name, nil)
		}
	}
	return nil
}

func (s *Server) projectRetainedAgent(ctx context.Context, x *call, resource *resources.Resource, body []byte) ([]byte, error) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: retainedBodyLimit})
	if err != nil {
		return nil, err
	}
	if contractErr := retainedAgentEnvironment(source); contractErr != nil {
		return nil, resources.ErrContract
	}
	source, err = normalizeRetainedAgent(source)
	if err != nil {
		return nil, err
	}
	projected, err := retainedapi.Rewrite(source, map[string]string{"/id": "agent_resource", "base_agent": "agent_resource"}, func(kind, id string) (string, error) {
		if id == resource.UpstreamID {
			return resource.ID, nil
		}
		identity := resource.NativeIdentity()
		identity.Kind = kind
		identity.UpstreamID = id
		bound, findErr := s.Resources.GetByUpstreamIncludingDeleted(ctx, identity)
		if findErr != nil {
			return "", findErr
		}
		return bound.ID, nil
	})
	if err != nil {
		return nil, err
	}
	return projected.Bytes(), nil
}

// Provider errors may echo native resource identifiers. Preserve the stable
// HTTP status while keeping provider identities outside public errors.
func retainedPublicError(err *Error, p *pin, resource *resources.Resource) *Error {
	out := *err
	out.Message = "The provider could not complete this resource operation."
	out.Param = nil
	out.Code = "provider_resource_error"
	out.Type = "provider_error"
	return &out
}

func (s *Server) listRetainedLifecycle(w http.ResponseWriter, r *http.Request, x *call, a access.Authority, e retainedapi.Endpoint, fail func(*Error)) int {
	limit := 20
	for name := range r.URL.Query() {
		if name != "page_size" && name != "page_token" {
			fail(invalidRequest("unsupported_parameter", "This listing supports page_size and page_token.", nil))
			return 400
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			fail(invalidRequest("invalid_parameter", "page_size must be between 1 and 100.", nil))
			return 400
		}
		limit = parsed
	}
	rows, err := s.Resources.ListLifecycle(r.Context(), e.Kind, x.keyID, limit+1, r.URL.Query().Get("page_token"))
	if err != nil {
		if errors.Is(err, resources.ErrNotFound) {
			fail(invalidRequest("invalid_parameter", "The page token is unavailable for this owner.", nil))
			return http.StatusBadRequest
		}
		fail(serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider state is unavailable."))
		return http.StatusServiceUnavailable
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]json.RawMessage, 0, len(rows))
	type listedResource struct {
		resource *resources.Resource
		pin      *pin
		route    *routing.Route
	}
	ready := make([]listedResource, 0, len(rows))
	var overall time.Duration
	for _, resource := range rows {
		if resource.State == "submitting" || resource.State == "submission_unknown" || resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
			continue
		}
		p, route, resolveErr := s.resolveResource(r.Context(), x, a, resource, e.Operation, retainedRetrieval)
		if resolveErr != nil {
			if resolveErr.Status != http.StatusNotFound && resolveErr.Status != http.StatusConflict {
				fail(resolveErr)
				return resolveErr.Status
			}
			continue
		}
		if gateErr := x.guardrailSurfaceGate(route); gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
		if err := s.retainedControlPricing(x, p, route); err != nil {
			fail(err)
			return err.Status
		}
		ready = append(ready, listedResource{resource, p, route})
		timeout := time.Duration(route.OverallTimeout) * time.Millisecond
		if len(ready) == 1 || timeout < overall {
			overall = timeout
		}
	}
	ctx := r.Context()
	if len(ready) > 0 {
		// One deadline covers the page and every lease held for its reads.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overall)
		defer cancel()
		if gateErr := s.reserveState(ctx, x, a, overall); gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
		authority := x.admissionAuthority(a)
		admitted := make(map[string]bool)
		costChecked := false
		for _, entry := range ready {
			if admitted[entry.route.Slug] {
				continue
			}
			lease, gateErr := s.Admission.reserveRouteLimits(ctx, &authority, entry.route.Slug, resourceEstimate, overall, costReservation{}, &costChecked)
			if gateErr != nil {
				fail(gateErr)
				return gateErr.Status
			}
			if lease != nil {
				if x.lease == nil {
					x.lease = lease
				} else {
					x.lease.Attach(lease)
				}
			}
			admitted[entry.route.Slug] = true
		}
	}
	responseBytes := 256
	for _, entry := range ready {
		resource, p, route := entry.resource, entry.pin, entry.route
		if gateErr := s.gateRetainedSlot(ctx, x, p, route); gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
		endpoint, urlErr := p.provider.Connector().ResourceURL(p.model, "agents/"+url.PathEscape(resource.UpstreamID), nil)
		if urlErr != nil {
			s.settlePinHold(ctx, x, p.hold, nil)
			fail(pinUnavailable())
			return 409
		}
		native, callErr := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if callErr != nil {
			s.settlePinHold(ctx, x, p.hold, nil)
			if callErr.status == http.StatusNotFound {
				commitCtx, commitCancel := resourceCommitContext(ctx)
				removeErr := s.Resources.Tombstone(commitCtx, resource.ID)
				commitCancel()
				if removeErr != nil {
					fail(serverError(503, "provider_state_unavailable", "The deleted resource could not be reconciled."))
					return 503
				}
				continue
			}
			fail(retainedPublicError(upstreamError(callErr), p, resource))
			return upstreamError(callErr).Status
		}
		body, readErr := readBounded(native.Body, s.retainedResponseLimit())
		native.Body.Close()
		if readErr == nil {
			var id string
			var valid bool
			id, valid = nativeResourceID(body, retainedBodyLimit)
			if !valid || id != resource.UpstreamID {
				readErr = resources.ErrContract
			}
		}
		var projected []byte
		if readErr == nil {
			projected, readErr = s.projectRetainedAgent(ctx, x, resource, body)
		}
		if readErr == nil {
			x.completePinned()
		}
		s.settlePinHold(ctx, x, p.hold, nil)
		if readErr != nil {
			fail(serverError(502, "provider_protocol_error", "The provider returned an invalid resource."))
			return 502
		}
		responseBytes += len(projected) + 1
		if responseBytes > retainedBodyLimit {
			fail(serverError(502, "provider_protocol_error", "The resource page exceeds the supported response size; request a smaller page."))
			return 502
		}
		items = append(items, projected)
	}
	response := map[string]any{e.Collection: items}
	if hasMore && len(rows) > 0 {
		response["next_page_token"] = rows[len(rows)-1].ID
	}
	body, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		fail(serverError(500, "internal_error", "The resource list could not be encoded."))
		return 500
	}
	s.writeStateJSONStatus(w, x, body, http.StatusOK)
	return 200
}

const retainedBodyLimit = 4 << 20

func (s *Server) gateRetainedSlot(ctx context.Context, x *call, p *pin, route *routing.Route) *Error {
	if p.hold != nil {
		return nil
	}
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
	if gate.verdict != gateAdmitted {
		return gateError(gate)
	}
	p.hold = gate.hold
	return nil
}

// Environment and credential references require their own owner-scoped
// contracts. Inline environment values may pass without provider references.
func retainedAgentEnvironment(source oif.Document) *Error {
	v, present := source.Root().Lookup("base_environment")
	if present && v.Kind() != oif.Null && v.Kind() != oif.Object {
		return policyUnavailable("unsupported_stateful_reference", "Native environment references are not supported by this agent resource contract.")
	}
	var bindings []oif.Value
	if env, ok := v.Lookup("env"); ok {
		for _, variable := range env.Members() {
			bindings = append(bindings, variable.Value)
		}
	}
	if network, ok := v.Lookup("network"); ok {
		if allowlist, ok := network.Lookup("allowlist"); ok {
			bindings = append(bindings, allowlist.Elements()...)
		}
	}
	for _, binding := range bindings {
		if credential, ok := binding.Lookup("credential"); ok && credential.Kind() != oif.Null {
			return policyUnavailable("unsupported_stateful_reference", "Native credential references are not supported by this agent resource contract.")
		}
	}
	return nil
}

// The native SDK represents a standalone agent with an empty optional base.
func normalizeRetainedAgent(source oif.Document) (oif.Document, error) {
	if value, present := source.Root().Lookup("base_agent"); present {
		if id, text := value.Text(); text && id == "" {
			return oif.Apply(source, []oif.Change{{Pointer: "/base_agent", Remove: true, Origin: oif.ResourceBinding, Reason: "empty optional base agent"}})
		}
	}
	return source, nil
}

// This contract contains resource control calls, without model execution or
// usage observations. Never admit configured fees through the unpriced path.
func (s *Server) retainedControlPricing(x *call, p *pin, route *routing.Route) *Error {
	if x.routingInputs == nil && s.Runtime != nil {
		x.routingInputs = s.Runtime.RoutingInputs()
	}
	if p.attempt.Price == nil {
		p.attempt.Price = x.routingInputs.Price(p.provider.Kind, p.provider.ID, p.provider.VendorID, p.model, x.operationName(), x.request.startedAt)
	}
	if p.attempt.Price != nil || x.attemptCardRevisionForRoute(p.provider.ID, route, x.request.startedAt) != nil {
		return policyUnavailable("unsupported_pricing", "Agent resource control does not support priced execution. Use an unpriced resource route.")
	}
	return nil
}
