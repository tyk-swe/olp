package kernel

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
)

// Generic retained families share the managed-agent lifecycle contracts:
// owner-scoped gateway IDs, pinned route/provider/credential affinity,
// submission fences for creates, and no local content retention. Each
// family keeps its native wire shape; only identity fields are rewritten.

func retainedSurfaceFormat(surface string) wire.Format {
	switch surface {
	case "anthropic":
		return wire.AnthropicFile
	case "gemini", "native":
		return wire.GeminiFile
	}
	return wire.File
}

func (s *Server) registerRetainedGeneric(mux *http.ServeMux) {
	for _, endpoint := range retainedapi.Endpoints() {
		if endpoint.Operation == "agent_resource" {
			continue
		}
		e := endpoint
		mux.HandleFunc(e.Method+" "+e.Pattern, func(w http.ResponseWriter, r *http.Request) { s.retainedGeneric(w, r, e) })
	}
	mux.HandleFunc("POST /v1/vector_stores/{id}/search", s.retainedSearch)
	mux.HandleFunc("POST /v1/responses/compact", s.retainedCompact)
}

// upstreamPath maps a gateway pattern to the provider-relative path by
// substituting upstream IDs and stripping the gateway surface prefix.
func retainedUpstreamPath(e retainedapi.Endpoint, ids map[string]string) string {
	path := e.Pattern
	for name, id := range ids {
		if e.Surface == "gemini" {
			id = strings.TrimPrefix(id, "cachedContents/")
		}
		if e.Surface == "native" {
			if i := strings.LastIndex(id, "/tuningJobs/"); i >= 0 {
				id = id[i+len("/tuningJobs/"):]
			}
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(id))
	}
	switch e.Surface {
	case "anthropic":
		path = strings.TrimPrefix(path, "/anthropic/v1/")
	case "gemini":
		path = strings.TrimPrefix(path, "/gemini/v1beta/")
	case "native":
		path = strings.TrimPrefix(path, "/native/vertex-tuning/")
		path = "tuningJobs" + strings.TrimPrefix(path, "jobs")
		if e.Action == "cancel" {
			path = strings.TrimSuffix(path, "/cancel") + ":cancel"
		}
	default:
		path = strings.TrimPrefix(path, "/v1/")
	}
	return path
}

// retainedSearch serves native vector-store search and query-only bindings
// through the one authorized retrieval path. It creates no resource.
func (s *Server) retainedSearch(w http.ResponseWriter, r *http.Request) {
	if id, err := uuid.Parse(r.PathValue("id")); err == nil {
		r.SetPathValue("binding_id", id.String())
		s.queryRetrieval(w, r)
		return
	}
	s.retainedProxy(w, r, "retrieval", "vector_stores/{id}/search", "vector_store")
}

// retainedCompact serves native compaction as its own priced call.
func (s *Server) retainedCompact(w http.ResponseWriter, r *http.Request) {
	s.retainedProxy(w, r, "generation", "responses/compact", "")
}

func (s *Server) retainedProxy(w http.ResponseWriter, r *http.Request, operation, upstreamTemplate, storeKind string) {
	x, authority, done := s.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	x.operationOverride = operation
	x.mode = "unary"
	x.actor = "api_key_resource"
	x.retained = retainedOperation{use: RetainedNewWork, billing: retainedMeteredWork}
	var p *Pin
	status := http.StatusInternalServerError
	defer func() { s.Finish(x, nil, status); s.retainedSettle(r.Context(), x, p) }()
	fail := func(err *fault.Error) {
		x.Failure = err
		status = err.Status
		fault.WriteSurfaceError(w, err, "openai")
	}
	if !authority.Policy.AllowProviderState {
		fail(providerStateForbidden())
		return
	}
	body, err := s.Gate.ReadBody(r)
	if err != nil {
		fail(err)
		return
	}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.Config.MaxBodyBytes)})
	if parseErr != nil || source.Root().Kind() != oif.Object {
		fail(fault.InvalidRequest("invalid_request", "A valid JSON object is required.", nil))
		return
	}
	codec := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "retrieval", Surface: "openai"}}
	if storeKind == "" {
		codec.Operation = "compaction"
	}
	if len(r.URL.Query()) != 0 {
		fail(fault.InvalidRequest("invalid_parameter", "This operation does not support query parameters.", nil))
		return
	}
	var route *routing.Route
	var resource *resources.Resource
	upstreamPath := upstreamTemplate
	if storeKind != "" {
		var getErr error
		resource, getErr = s.Deps.Resources.Get(r.Context(), storeKind, x.keyID, r.PathValue("id"))
		if getErr != nil {
			fail(fault.NotFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
		var resolveErr *fault.Error
		// Searching a retained store starts new billable work, so it
		// must satisfy current new-work restrictions rather than the
		// historical retrieval contract that created the binding.
		p, route, resolveErr = s.ResolveResource(r.Context(), x, authority, resource, operation, RetainedNewWork)
		if resolveErr != nil {
			fail(resolveErr)
			return
		}
		upstreamPath = strings.Replace(upstreamTemplate, "{id}", url.PathEscape(resource.UpstreamID), 1)
	} else {
		slug := strings.TrimSpace(r.Header.Get(StateRouteHeader))
		if slug == "" {
			fail(fault.InvalidRequest("missing_required_parameter", "X-OLP-Route is required.", nil))
			return
		}
		candidate, ok := x.Snapshot().FindRoute(slug)
		if !ok {
			fail(fault.ModelNotFound(slug))
			return
		}
		route = &candidate
		x.Route = route
		if !authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
			fail(fault.PermissionError("route_forbidden", "The API key cannot use this route."))
			return
		}
		if !slices.Contains(route.Operations, operation) {
			fail(PolicyUnavailable("operation_unavailable", "The route does not allow this operation."))
			return
		}
		if gateErr := x.GuardrailSurfaceGate(route); gateErr != nil {
			fail(gateErr)
			return
		}
		// Price the work before selection reserves the provider hold:
		// selecting first would admit the slot against the floored
		// minimum estimate regardless of the actual input size.
		x.estimate = int64(len(body))
		// Retained references own the pin that must serve this call;
		// resolve them first so selection is bound to their target
		// and credential slot instead of rejecting them on affinity.
		var candidates []*resources.Resource
		if value, present := source.Root().Lookup("previous_response_id"); present {
			if id, isText := value.Text(); isText && id != "" {
				resolved, _, readErr := s.readResponseResource(r.Context(), x.keyID, id)
				if readErr != nil {
					fail(fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
					return
				}
				candidates = append(candidates, resolved)
			}
		}
		for _, fileID := range retainedFileIDs(source.Root()) {
			resolved, _, readErr := s.readDurable(r.Context(), durableKind(resources.KindFile, fileID), x.keyID, fileID)
			if readErr != nil {
				fail(fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
				return
			}
			candidates = append(candidates, resolved)
		}
		var bound *resources.Resource
		for _, candidate := range candidates {
			if bound == nil {
				bound = candidate
				continue
			}
			if !samePin(candidate, bound) {
				fail(fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
				return
			}
		}
		var pinErr *fault.Error
		p, pinErr = s.selectBoundPinSurface(r.Context(), x, route, operation, "openai", "unary", bound, func(provider *routing.Provider, model string) bool {
			return provider.CredentialSource != "caller" && (provider.Kind == "openai" || provider.Kind == "azure_openai") && provider.Connector().Supports(operation, "openai", "unary")
		})
		if pinErr != nil {
			fail(pinErr)
			return
		}
	}
	x.Route = route
	if gateErr := x.GuardrailSurfaceGate(route); gateErr != nil {
		fail(gateErr)
		return
	}
	rewritten, rewriteErr := s.rewriteRetainedRequest(r.Context(), x, authority, p, route, source, codec)
	if rewriteErr != nil {
		fail(fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
		return
	}
	body = rewritten.Bytes()
	x.estimate = int64(len(body))
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	if err := s.admitRetainedRequest(ctx, x, authority, p, route, false); err != nil {
		fail(err)
		return
	}
	endpoint, urlErr := p.Provider.Connector().ResourceURL(p.Model, upstreamPath, nil)
	if urlErr != nil {
		fail(fault.ServerError(502, "provider_protocol_error", "The retained endpoint is unavailable."))
		return
	}
	native, failure := s.PinnedDo(ctx, x, p, http.MethodPost, endpoint, body, "application/json")
	if failure != nil {
		fail(retainedPublicError(UpstreamError(failure), failure.Status))
		return
	}
	defer native.Body.Close()
	response, readErr := native.ReadBody(s.retainedResponseLimit())
	if readErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an unsupported resource response."))
		return
	}
	responseSource, parseErr := oif.ParseJSON(response, oif.Limits{MaxBytes: retainedBodyLimit})
	if parseErr != nil || responseSource.Root().Kind() != oif.Object {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid response."))
		return
	}
	generation := storeKind == ""
	if generation && observeRetainedUsage(native.Exchange, responseSource, true) != nil {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned invalid usage."))
		return
	}
	rewrite, referenceErr := retainedapi.PrepareRewrite(responseSource, retainedResponseRefs(codec, responseSource))
	if referenceErr != nil {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider response contained an unbound resource reference."))
		return
	}
	// Compaction has no parent resource; project its returned
	// references against the serving pin's owner-scoped identity.
	projectAs := resource
	if projectAs == nil && p != nil && route != nil {
		projectAs = &resources.Resource{APIKeyID: x.keyID, RouteSlug: route.Slug, ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID, TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID}
	}
	projected, projectErr := s.projectRetainedGeneric(ctx, projectAs, codec, responseSource, rewrite)
	if projectErr != nil {
		fail(retainedProjectionError(native.Exchange, projectErr))
		return
	}
	if !generation {
		_ = observeRetainedUsage(native.Exchange, responseSource, false)
	}
	native.Exchange.Complete(ClassSuccess)
	response = projected
	status = native.StatusCode
	s.WriteStateJSONStatus(w, x, response, status, native.Exchange)
}

func retainedRejectedWithoutWork(failure *AttemptFailure) bool {
	if failure == nil {
		return false
	}
	if failure.Acceptance == upstream.NotSent {
		return true
	}
	if failure.Acceptance != upstream.Terminal || failure.accepted {
		return false
	}
	switch failure.Status {
	case 400, 401, 403, 404, 405, 413, 415, 422, 429:
		return true
	}
	return false
}
